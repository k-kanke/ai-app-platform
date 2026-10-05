package api_test

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/kube"
)

func (e *env) deployment(name string) (subPath string, readOnly bool, dataClaim string, replicas int32, found bool) {
	d, err := e.cs.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return "", false, "", 0, false
	}
	for _, m := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/app" {
			subPath, readOnly = m.SubPath, m.ReadOnly
		}
	}
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == "data" {
			dataClaim = v.PersistentVolumeClaim.ClaimName
		}
	}
	return subPath, readOnly, dataClaim, *d.Spec.Replicas, true
}

func (e *env) jobCount(role string) int {
	l, _ := e.cs.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{LabelSelector: kube.LabelRole + "=" + role})
	return len(l.Items)
}

func (e *env) waitDraft(app, want string) map[string]any {
	e.t.Helper()
	return e.waitPhase(app, want)
}

// The whole release strategy, end to end: preview first, production only changes on approval.
func TestReleaseStrategyLifecycle(t *testing.T) {
	e := newEnv(t, nil, nil)
	code, out := e.do("POST", "/api/v1/apps", map[string]string{"id": "memo", "name": "m", "prompt": "メモ帳", "strategy": "release"}, nil)
	if code != 202 {
		t.Fatalf("create: %d %v", code, out)
	}

	// 1) Creating produces a PREVIEW, not a production app.
	app := e.waitDraft("memo", "PREVIEW")
	if app["draftState"] != "PREVIEW_READY" || app["previewUrl"] != "http://memo-preview.apps.test" {
		t.Fatalf("expected a ready preview, got %v", app)
	}
	if _, _, _, _, found := e.deployment("aap-gen-memo"); found {
		t.Fatal("production must not exist before the first approval")
	}
	sub, ro, claim, _, found := e.deployment("aap-gen-memo-preview")
	if !found || sub != "draft" || !ro || claim != "aap-gen-memo-pdata" {
		t.Fatalf("preview must run draft/ read-only on its own data copy: sub=%q ro=%v claim=%q", sub, ro, claim)
	}
	pvcs, _ := e.cs.CoreV1().PersistentVolumeClaims(ns).List(context.Background(), metav1.ListOptions{})
	if len(pvcs.Items) != 4 {
		t.Fatalf("release strategy uses 4 PVCs (src, data, pdata, dsnap), got %d", len(pvcs.Items))
	}
	agents, _ := e.cs.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{LabelSelector: kube.LabelRole + "=agent"})
	var agentSub string
	for _, m := range agents.Items[0].Spec.Template.Spec.Containers[0].VolumeMounts {
		agentSub = m.SubPath
	}
	if agentSub != "draft" {
		t.Fatalf("the Agent must edit draft/ only, mounts %q", agentSub)
	}

	// 2) Approve -> release 1 is live, read-only, with a content hash; the preview is gone.
	if c, o := e.do("POST", "/api/v1/apps/memo/approve", nil, nil); c != 202 {
		t.Fatalf("approve: %d %v", c, o)
	}
	app = e.waitDraft("memo", "READY")
	if app["liveRelease"] != float64(1) || app["draftState"] != nil {
		t.Fatalf("after approval: %v", app)
	}
	sub, ro, claim, _, _ = e.deployment("aap-gen-memo")
	if sub != "releases/1" || !ro || claim != "aap-gen-memo-data" {
		t.Fatalf("production must run releases/1 read-only: sub=%q ro=%v claim=%q", sub, ro, claim)
	}
	if _, _, _, _, found := e.deployment("aap-gen-memo-preview"); found {
		t.Fatal("the preview should be removed after approval")
	}
	_, rel := e.do("GET", "/api/v1/apps/memo/releases", nil, nil)
	r0 := rel["releases"].([]any)[0].(map[string]any)
	if len(r0["sourceHash"].(string)) != 64 || r0["dataSnapshot"] != false {
		t.Fatalf("release 1 record: %v", r0)
	}

	// 3) A modification changes the DRAFT and the preview, never production.
	e.do("POST", "/api/v1/apps/memo/changes", map[string]string{"prompt": "文字を大きく"}, nil)
	app = e.waitDraft("memo", "READY")
	if app["draftState"] != "PREVIEW_READY" || app["previewUrl"] == nil {
		t.Fatalf("expected a preview of the change: %v", app)
	}
	sub, _, _, _, _ = e.deployment("aap-gen-memo")
	if sub != "releases/1" {
		t.Fatalf("production changed before approval: %q", sub)
	}
	if e.jobCount("dcopy") != 1 {
		t.Fatalf("the preview data must be a copy of production data (dcopy jobs: %d)", e.jobCount("dcopy"))
	}

	// 4) Approve -> release 2; data was snapshotted with production stopped; production restarted.
	e.do("POST", "/api/v1/apps/memo/approve", nil, nil)
	app = e.waitDraft("memo", "READY")
	if app["liveRelease"] != float64(2) {
		t.Fatalf("expected release 2: %v", app)
	}
	sub, _, _, replicas, _ := e.deployment("aap-gen-memo")
	if sub != "releases/2" || replicas != 1 {
		t.Fatalf("production should run releases/2 with 1 replica: %q %d", sub, replicas)
	}
	if e.jobCount("dsnap") != 1 {
		t.Fatalf("approval with a live release must snapshot data (dsnap jobs: %d)", e.jobCount("dsnap"))
	}
	_, rel = e.do("GET", "/api/v1/apps/memo/releases", nil, nil)
	if rel["releases"].([]any)[0].(map[string]any)["dataSnapshot"] != true {
		t.Fatalf("release 2 should record its data snapshot: %v", rel)
	}

	// 5) Roll back, with data.
	if c, o := e.do("POST", "/api/v1/apps/memo/rollback", map[string]bool{"withData": true}, nil); c != 202 {
		t.Fatalf("rollback: %d %v", c, o)
	}
	app = e.waitDraft("memo", "READY")
	sub, _, _, replicas, _ = e.deployment("aap-gen-memo")
	if app["liveRelease"] != float64(1) || sub != "releases/1" || replicas != 1 || e.jobCount("drestore") != 1 {
		t.Fatalf("rollback: live=%v sub=%q replicas=%d drestore=%d", app["liveRelease"], sub, replicas, e.jobCount("drestore"))
	}

	// 6) A draft can be thrown away.
	e.do("POST", "/api/v1/apps/memo/changes", map[string]string{"prompt": "やっぱり別の案"}, nil)
	e.waitDraft("memo", "READY")
	if c, _ := e.do("POST", "/api/v1/apps/memo/discard", nil, nil); c != 202 {
		t.Fatalf("discard: %d", c)
	}
	app = e.waitDraft("memo", "READY")
	if app["draftState"] != nil || e.jobCount("reset") < 1 {
		t.Fatalf("discard: %v resets=%d", app, e.jobCount("reset"))
	}
	if _, _, _, _, found := e.deployment("aap-gen-memo-preview"); found {
		t.Fatal("discard should remove the preview")
	}
}

// A failed change is visible nowhere in production.
func TestFailedDraftNeverTouchesProduction(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.do("POST", "/api/v1/apps", map[string]string{"id": "memo", "name": "m", "prompt": "p", "strategy": "release"}, nil)
	e.waitDraft("memo", "PREVIEW")
	e.do("POST", "/api/v1/apps/memo/approve", nil, nil)
	e.waitDraft("memo", "READY")

	e.failAgent.Store(true)
	e.do("POST", "/api/v1/apps/memo/changes", map[string]string{"prompt": "壊れる変更"}, nil)
	app := e.waitDraft("memo", "READY")
	op := app["operation"].(map[string]any)
	if op["state"] != "FAILED" || op["userMessage"] == nil {
		t.Fatalf("the change should be reported as failed with a reason: %v", op)
	}
	sub, _, _, _, _ := e.deployment("aap-gen-memo")
	if sub != "releases/1" {
		t.Fatalf("production must be untouched: %q", sub)
	}
	if _, _, _, _, found := e.deployment("aap-gen-memo-preview"); found {
		t.Fatal("no preview after a failed draft")
	}
	if e.jobCount("reset") < 1 {
		t.Fatal("the draft must be reset to the live release")
	}
	// ... and approving without a preview is refused.
	if c, _ := e.do("POST", "/api/v1/apps/memo/approve", nil, nil); c != 409 {
		t.Fatalf("approve without a preview should be 409, got %d", c)
	}
}

func TestReleaseEndpointsRefuseWhatDoesNotApply(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.do("POST", "/api/v1/apps", map[string]string{"id": "old", "name": "o", "prompt": "p"}, nil) // inplace
	e.waitDraft("old", "READY")
	for _, path := range []string{"/approve", "/discard", "/rollback"} {
		if c, _ := e.do("POST", "/api/v1/apps/old"+path, nil, nil); c != 409 {
			t.Fatalf("%s on an in-place app should be 409, got %d", path, c)
		}
	}
	if c, _ := e.do("POST", "/api/v1/apps", map[string]string{"id": "x-preview", "name": "x", "prompt": "p"}, nil); c != 400 {
		t.Fatalf("ids ending in -preview collide with preview hosts, got %d", c)
	}
	if c, _ := e.do("POST", "/api/v1/apps", map[string]string{"id": "y", "name": "y", "prompt": "p", "strategy": "nope"}, nil); c != 400 {
		t.Fatalf("unknown strategy should be 400, got %d", c)
	}
	_ = strings.TrimSpace // keep import used if tests above change
}

// Iterating on a preview and failing must not leave a stale preview or a half-edited draft behind.
func TestFailedIterationDropsThePreview(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.do("POST", "/api/v1/apps", map[string]string{"id": "memo", "name": "m", "prompt": "p", "strategy": "release"}, nil)
	e.waitDraft("memo", "PREVIEW")
	e.do("POST", "/api/v1/apps/memo/approve", nil, nil)
	e.waitDraft("memo", "READY")

	e.do("POST", "/api/v1/apps/memo/changes", map[string]string{"prompt": "1回目"}, nil)
	app := e.waitDraft("memo", "READY")
	if app["draftState"] != "PREVIEW_READY" {
		t.Fatalf("setup: expected a preview: %v", app)
	}
	resetsBefore := e.jobCount("reset")

	// "もう少し直す" -- and the agent fails this time.
	e.failAgent.Store(true)
	e.do("POST", "/api/v1/apps/memo/changes", map[string]string{"prompt": "2回目(失敗)"}, nil)
	app = e.waitDraft("memo", "READY")
	if app["operation"].(map[string]any)["state"] != "FAILED" {
		t.Fatalf("the second change should have failed: %v", app["operation"])
	}
	if _, _, _, _, found := e.deployment("aap-gen-memo-preview"); found {
		t.Fatal("a stale preview of a half-failed draft must be removed")
	}
	if app["draftState"] != nil {
		t.Fatalf("draft state should be cleared, got %v", app["draftState"])
	}
	if e.jobCount("reset") != resetsBefore+1 {
		t.Fatal("the draft must be reset to the live release")
	}
	if sub, _, _, _, _ := e.deployment("aap-gen-memo"); sub != "releases/1" {
		t.Fatalf("production changed: %q", sub)
	}
}
