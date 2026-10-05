package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/api"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/config"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/events"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/kube"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/orchestrator"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/store"
)

const ns = "aap-apps"

type env struct {
	t          *testing.T
	cs         *fake.Clientset
	st         *store.Store
	orch       *orchestrator.Orchestrator
	ts         *httptest.Server
	failAgent  atomic.Bool
	noRuntime  atomic.Bool
	cancelBase context.CancelFunc
}

func newEnv(t *testing.T, st *store.Store, cs *fake.Clientset) *env {
	t.Helper()
	if st == nil {
		st, _ = store.Open(":memory:")
	}
	if cs == nil {
		cs = fake.NewSimpleClientset()
	}
	cfg := config.Config{Namespace: ns, Agent: "gemini", GeminiModel: "gemini-test-model", AgentImage: "agent", RuntimeImage: "rt", HelperImage: "busybox", SourceSize: "1Gi", DataSize: "1Gi",
		AgentTimeout: time.Minute, InternalURL: "http://cp"}
	broker := events.NewBroker()
	kc := kube.New(cs, cfg)
	// Pretend the Agent printed its latency trace as its last log line.
	kc.LogReader = func(ctx context.Context, pod string, tail int64) (string, error) {
		return "== agent=gemini\nsome agent output\nAAP_TRACE {\"v\":1,\"rc\":0,\"writes\":3,\"t_entry\":1000,\"t_first_write\":21000,\"t_last_write\":30000}\n", nil
	}
	orch := orchestrator.New(st, kc, broker, orchestrator.Options{
		TokenSecret: "s3cret", PollInterval: 5 * time.Millisecond, RuntimeTimeout: 500 * time.Millisecond,
		HelperTimeout: time.Second, AgentTimeout: time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	base, cancel := context.WithCancel(context.Background())
	e := &env{t: t, cs: cs, st: st, orch: orch, cancelBase: cancel}
	srv := &api.Server{St: st, Kube: kc, Orch: orch, Broker: broker, BaseCtx: base, AppURLTemplate: "http://{id}.apps.test"}
	e.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(func() { cancel(); orch.Wait(); e.ts.Close() })
	go e.kubelet(base)
	return e
}

// kubelet plays the role of the Kubernetes controllers: it completes Jobs and
// makes Deployments ready, so the Control Plane's flow can run against a fake API.
func (e *env) kubelet(ctx context.Context) {
	for ctx.Err() == nil {
		time.Sleep(5 * time.Millisecond)
		jobs, _ := e.cs.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
		for i := range jobs.Items {
			j := jobs.Items[i]
			// Like the real Job controller: each Job has a pod carrying the template labels.
			if _, err := e.cs.CoreV1().Pods(ns).Get(ctx, j.Name+"-pod", metav1.GetOptions{}); err != nil {
				e.cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name: j.Name + "-pod", Namespace: ns, Labels: j.Spec.Template.Labels}}, metav1.CreateOptions{})
			}
			if j.Status.Succeeded > 0 || j.Status.Failed > 0 {
				continue
			}
			if j.Labels[kube.LabelRole] == kube.RoleAgent && e.failAgent.Load() {
				j.Status.Failed = 1
				j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
			} else {
				j.Status.Succeeded = 1
			}
			e.cs.BatchV1().Jobs(ns).UpdateStatus(ctx, &j, metav1.UpdateOptions{})
		}
		deps, _ := e.cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		for i := range deps.Items {
			d := deps.Items[i]
			if e.noRuntime.Load() {
				continue
			}
			d.Status = appsStatus(d.Generation)
			e.cs.AppsV1().Deployments(ns).UpdateStatus(ctx, &d, metav1.UpdateOptions{})
		}
	}
}

func (e *env) do(method, path string, body any, hdr map[string]string) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) waitPhase(app, want string) map[string]any {
	e.t.Helper()
	var last map[string]any
	for i := 0; i < 400; i++ {
		_, last = e.do("GET", "/api/v1/apps/"+app, nil, nil)
		if last["phase"] == want {
			// also wait for the operation to settle
			if op, ok := last["operation"].(map[string]any); ok && (op["state"] == "PENDING" || op["state"] == "RUNNING") {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("app %s never reached %s, last=%v", app, want, last)
	return nil
}

func TestCreateAppEndToEnd(t *testing.T) {
	e := newEnv(t, nil, nil)
	code, out := e.do("POST", "/api/v1/apps", map[string]string{"id": "meal", "name": "ご飯管理", "prompt": "家族のご飯予定表"}, nil)
	if code != 202 {
		t.Fatalf("status %d: %v", code, out)
	}
	app := e.waitPhase("meal", "READY")
	if app["url"] != "http://meal.apps.test" {
		t.Fatalf("url: %v", app["url"])
	}

	// The Agent's latency trace was collected from its log and stored on the operation.
	_, opsOut := e.do("GET", "/api/v1/apps/meal/operations", nil, nil)
	tr, _ := opsOut["operations"].([]any)[0].(map[string]any)["trace"].(string)
	if !strings.Contains(tr, `"t_first_write":21000`) {
		t.Fatalf("trace not stored on the operation: %q", tr)
	}

	// Source and Data are separate PVCs.
	pvcs, _ := e.cs.CoreV1().PersistentVolumeClaims(ns).List(context.Background(), metav1.ListOptions{})
	if len(pvcs.Items) != 2 {
		t.Fatalf("want 2 PVCs, got %d", len(pvcs.Items))
	}

	// Trust boundary: the Agent Job mounts Source only, with no Data and no API token.
	jobs, _ := e.cs.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{LabelSelector: kube.LabelRole + "=agent"})
	if len(jobs.Items) != 1 {
		t.Fatalf("want 1 agent job, got %d", len(jobs.Items))
	}
	pod := jobs.Items[0].Spec.Template.Spec
	if len(pod.Volumes) != 1 || pod.Volumes[0].PersistentVolumeClaim.ClaimName != "aap-gen-meal-src" {
		t.Fatalf("agent must mount only the source PVC: %+v", pod.Volumes)
	}
	env := map[string]string{}
	for _, e := range pod.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["AAP_AGENT"] != "gemini" || env["AAP_GEMINI_MODEL"] != "gemini-test-model" {
		t.Fatalf("agent/model not passed to the Job: %v", env)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("agent must not get a service account token")
	}
	if pod.Containers[0].SecurityContext.RunAsNonRoot == nil || !*pod.Containers[0].SecurityContext.RunAsNonRoot {
		t.Fatal("agent must run as non-root")
	}

	// Runtime: Source read-only, Data read-write.
	dep, _ := e.cs.AppsV1().Deployments(ns).Get(context.Background(), "aap-gen-meal", metav1.GetOptions{})
	for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		switch m.MountPath {
		case "/app":
			if !m.ReadOnly {
				t.Fatal("source must be read-only in runtime")
			}
		case "/data":
			if m.ReadOnly {
				t.Fatal("data must be writable in runtime")
			}
		}
	}
	if dep.Labels[kube.LabelManaged] != kube.ManagedByName {
		t.Fatal("ownership label missing")
	}
}

func TestDuplicateRequestsAreIdempotent(t *testing.T) {
	e := newEnv(t, nil, nil)
	h := map[string]string{"Idempotency-Key": "abc"}
	body := map[string]string{"id": "meal", "name": "m", "prompt": "p"}
	c1, o1 := e.do("POST", "/api/v1/apps", body, h)
	c2, o2 := e.do("POST", "/api/v1/apps", body, h)
	if c1 != 202 || c2 != 200 || o2["duplicate"] != true {
		t.Fatalf("got %d %d %v", c1, c2, o2)
	}
	if o1["operation"].(map[string]any)["id"] != o2["operation"].(map[string]any)["id"] {
		t.Fatal("retry created a second operation")
	}
	// Without a key the same id is a conflict, never a duplicate resource.
	if c, _ := e.do("POST", "/api/v1/apps", body, nil); c != 409 {
		t.Fatalf("want 409, got %d", c)
	}
	e.waitPhase("meal", "READY")
	jobs, _ := e.cs.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{LabelSelector: kube.LabelRole + "=agent"})
	if len(jobs.Items) != 1 {
		t.Fatalf("want exactly 1 agent job, got %d", len(jobs.Items))
	}
}

func TestFailedModifyRestoresPreviousSource(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.do("POST", "/api/v1/apps", map[string]string{"id": "meal", "name": "m", "prompt": "p"}, nil)
	e.waitPhase("meal", "READY")

	e.failAgent.Store(true)
	code, _ := e.do("POST", "/api/v1/apps/meal/changes", map[string]string{"prompt": "ダークモード"}, nil)
	if code != 202 {
		t.Fatalf("status %d", code)
	}
	app := e.waitPhase("meal", "READY") // app keeps serving
	op := app["operation"].(map[string]any)
	if op["state"] != "FAILED" {
		t.Fatalf("operation should be FAILED, got %v", op["state"])
	}
	jobs, _ := e.cs.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{LabelSelector: kube.LabelRole + "=restore"})
	snaps, _ := e.cs.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{LabelSelector: kube.LabelRole + "=snap"})
	if len(jobs.Items) != 1 || len(snaps.Items) != 1 {
		t.Fatalf("expected snapshot+restore jobs, got snap=%d restore=%d", len(snaps.Items), len(jobs.Items))
	}
}

func TestOnlyOneActiveOperationPerApp(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.noRuntime.Store(true) // keep the create operation in flight
	e.do("POST", "/api/v1/apps", map[string]string{"id": "meal", "name": "m", "prompt": "p"}, nil)
	if c, _ := e.do("POST", "/api/v1/apps/meal/changes", map[string]string{"prompt": "x"}, nil); c != 409 {
		t.Fatalf("want 409 while busy, got %d", c)
	}
}

func TestAgentProgressNeedsValidToken(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.noRuntime.Store(true)
	_, out := e.do("POST", "/api/v1/apps", map[string]string{"id": "meal", "name": "m", "prompt": "p"}, nil)
	opID := out["operation"].(map[string]any)["id"].(string)
	for i := 0; i < 200; i++ { // wait for the agent step
		if op, _ := e.st.GetOperation(context.Background(), opID); op.State == store.OpRunning {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	path := "/internal/v1/operations/" + opID + "/events"
	if c, _ := e.do("POST", path, map[string]string{"phase": "GENERATING"}, map[string]string{"Authorization": "Bearer nope"}); c != 401 {
		t.Fatalf("want 401, got %d", c)
	}
	tok := orchestrator.Token("s3cret", opID)
	if c, _ := e.do("POST", path, map[string]string{"phase": "GENERATING", "message": "画面を作っています"}, map[string]string{"Authorization": "Bearer " + tok}); c != 204 {
		t.Fatalf("want 204, got %d", c)
	}
	evs, _ := e.st.EventsAfter(context.Background(), "meal", 0)
	found := false
	for _, ev := range evs {
		found = found || ev.Message == "画面を作っています"
	}
	if !found {
		t.Fatal("progress event not stored")
	}
}

// Experiment B in miniature: an operation stored as RUNNING (process died
// mid-way) is resumed by a new orchestrator without duplicating resources.
func TestResumeAfterRestart(t *testing.T) {
	st, _ := store.Open(":memory:")
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	st.CreateApp(ctx, "meal", "m")
	op, _, _ := st.CreateOperation(ctx, store.Operation{ID: "0123456789abcdef", AppID: "meal", Kind: store.KindCreate, Prompt: "p"})
	st.UpdateOperation(ctx, op.ID, store.OpRunning, "agent", "")

	e := newEnv(t, st, cs)
	if err := e.orch.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.waitPhase("meal", "READY")
	jobs, _ := cs.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: kube.LabelRole + "=agent"})
	if len(jobs.Items) != 1 {
		t.Fatalf("resume created %d agent jobs", len(jobs.Items))
	}
}

func TestDeleteKeepsDataUnlessPurged(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.do("POST", "/api/v1/apps", map[string]string{"id": "meal", "name": "m", "prompt": "p"}, nil)
	e.waitPhase("meal", "READY")
	if c, _ := e.do("DELETE", "/api/v1/apps/meal", nil, nil); c != 202 {
		t.Fatalf("delete status %d", c)
	}
	for i := 0; i < 200; i++ {
		if c, _ := e.do("GET", "/api/v1/apps/meal", nil, nil); c == 404 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.orch.Wait()
	pvcs, _ := e.cs.CoreV1().PersistentVolumeClaims(ns).List(context.Background(), metav1.ListOptions{})
	if len(pvcs.Items) != 2 {
		t.Fatalf("data must survive a plain delete, have %d PVCs", len(pvcs.Items))
	}
	if _, err := e.cs.AppsV1().Deployments(ns).Get(context.Background(), "aap-gen-meal", metav1.GetOptions{}); err == nil {
		t.Fatal("runtime should be gone")
	}
	// Re-creating the same id reuses the existing PVCs.
	if c, out := e.do("POST", "/api/v1/apps", map[string]string{"id": "meal", "name": "m", "prompt": "p"}, nil); c != 202 {
		t.Fatalf("recreate: %d %v", c, out)
	}
	e.waitPhase("meal", "READY")
}

func TestValidation(t *testing.T) {
	e := newEnv(t, nil, nil)
	for _, b := range []map[string]string{
		{"name": "x"}, // no prompt
		{"id": "Bad_ID", "name": "x", "prompt": "p"}, // bad id
		{"id": strings.Repeat("a", 40), "name": "x", "prompt": "p"},
		{"id": "portal", "name": "x", "prompt": "p"}, // reserved: portal.<domain> belongs to the Portal
	} {
		if c, _ := e.do("POST", "/api/v1/apps", b, nil); c != 400 {
			t.Fatalf("%v: want 400, got %d", b, c)
		}
	}
}

// A failed create explains itself, can be retried from scratch (optionally reworded), and can be deleted.
func TestFailedCreateIsExplainedAndRetryable(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.failAgent.Store(true)
	e.do("POST", "/api/v1/apps", map[string]string{"id": "weather", "name": "w", "prompt": "天気を教えて"}, nil)
	app := e.waitPhase("weather", "FAILED")
	op := app["operation"].(map[string]any)
	if op["state"] != "FAILED" || op["userMessage"] == nil || op["userMessage"] == "" {
		t.Fatalf("failed op should carry a user message: %v", op)
	}
	if _, has := op["detail"]; has {
		t.Fatal("operator detail must not be exposed in the app view")
	}
	// Detail is available to operators.
	_, ops := e.do("GET", "/api/v1/apps/weather/operations", nil, nil)
	if _, has := ops["operations"].([]any)[0].(map[string]any)["detail"]; !has {
		t.Fatal("operations endpoint should include the raw detail")
	}

	// Retry is refused for apps that are not failed.
	e.do("POST", "/api/v1/apps", map[string]string{"id": "ok", "name": "o", "prompt": "p"}, nil) // will also fail (failAgent)
	e.failAgent.Store(false)
	code, out := e.do("POST", "/api/v1/apps/weather/retry", map[string]string{"prompt": "八王子の天気だけでいい"}, nil)
	if code != 202 {
		t.Fatalf("retry: %d %v", code, out)
	}
	app = e.waitPhase("weather", "READY")
	if app["operation"].(map[string]any)["prompt"] != "八王子の天気だけでいい" {
		t.Fatalf("retry should use the reworded request: %v", app["operation"])
	}
	// The retry wiped the half-finished source first.
	wipes, _ := e.cs.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{LabelSelector: kube.LabelRole + "=wipe"})
	if len(wipes.Items) != 1 {
		t.Fatalf("want 1 wipe job, got %d", len(wipes.Items))
	}
	if c, _ := e.do("POST", "/api/v1/apps/weather/retry", nil, nil); c != 409 {
		t.Fatalf("retry of a READY app should be 409, got %d", c)
	}
}

func TestRetryWithoutBodyReusesOriginalRequest(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.failAgent.Store(true)
	e.do("POST", "/api/v1/apps", map[string]string{"id": "shop", "name": "s", "prompt": "買い物リスト"}, nil)
	e.waitPhase("shop", "FAILED")
	e.failAgent.Store(false)
	if c, _ := e.do("POST", "/api/v1/apps/shop/retry", nil, nil); c != 202 {
		t.Fatalf("status %d", c)
	}
	app := e.waitPhase("shop", "READY")
	if app["operation"].(map[string]any)["prompt"] != "買い物リスト" {
		t.Fatalf("original request not reused: %v", app["operation"])
	}
}
