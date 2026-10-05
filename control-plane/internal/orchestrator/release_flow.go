package orchestrator

// Release strategy flows (docs/deep-dive-preview-release.md §3).
//
//	create / modify -> runDraft    the Agent edits draft/; a Preview runs it against a copy of production data
//	approve         -> runApprove  draft/ -> immutable releases/<n>/; production switches to it
//	rollback        -> runRollback production back to the previous release (optionally with its data)
//	discard         -> runDiscard  throw the draft and the preview away
//
// Production is only ever touched by approve and rollback.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/kube"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/store"
)

var hashRe = regexp.MustCompile(`AAP_HASH ([0-9a-f]{64})`)

// releaseHelper runs a release-strategy helper Job (a trusted platform script) and waits for it.
func (o *Orchestrator) releaseHelper(ctx context.Context, op store.Operation, role string, n int) error {
	if err := retry(ctx, func() error { return o.kube.EnsureReleaseHelperJob(ctx, op.AppID, role, op.ID, n) }); err != nil {
		return err
	}
	ph, reason, err := o.waitJob(ctx, kube.JobName(op.AppID, role, op.ID), o.opt.HelperTimeout)
	if err != nil {
		return err
	}
	if ph != kube.JobSucceeded {
		return fmt.Errorf("%s job failed: %s", role, reason)
	}
	return nil
}

// waitDeployment polls ready() until it is true or RuntimeTimeout passes.
func (o *Orchestrator) waitDeployment(ctx context.Context, what string, ready func(context.Context) (bool, string, error)) (bool, string, error) {
	deadline := time.Now().Add(o.opt.RuntimeTimeout)
	last := ""
	for {
		ok, detail, err := ready(ctx)
		if err != nil {
			return false, "", err
		}
		if ok {
			return true, "", nil
		}
		last = detail
		if time.Now().After(deadline) {
			return false, what + " not ready: " + last, nil
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(o.opt.PollInterval):
		}
	}
}

// stopRuntime scales production to 0 and waits until its pods are gone.
func (o *Orchestrator) stopRuntime(ctx context.Context, appID string) error {
	if err := retry(ctx, func() error { return o.kube.ScaleRuntime(ctx, appID, 0) }); err != nil {
		return err
	}
	deadline := time.Now().Add(o.opt.RuntimeTimeout)
	for {
		done, err := o.kube.RuntimeStopped(ctx, appID)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("production runtime did not stop in time")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.opt.PollInterval):
		}
	}
}

// ---- create / modify -------------------------------------------------------

func (o *Orchestrator) runDraft(ctx context.Context, op store.Operation, app store.App) error {
	modify := op.Kind == store.KindModify
	o.Emit(ctx, op.AppID, op.ID, store.PhaseAgentStarting, "準備しています")
	_ = o.st.SetDraftState(ctx, op.AppID, store.DraftDrafting)

	o.step(ctx, op, "workspace")
	if err := retry(ctx, func() error { return o.kube.EnsureReleaseWorkspace(ctx, op.AppID) }); err != nil {
		return o.failDraft(ctx, op, app, stagePrepare, err)
	}
	if !modify {
		o.step(ctx, op, "init")
		if err := o.releaseHelper(ctx, op, kube.RoleRInit, 0); err != nil {
			return o.failDraft(ctx, op, app, stagePrepare, err)
		}
		if op.Purge { // retry of a failed create: start from an empty draft
			o.step(ctx, op, "wipe")
			if err := o.releaseHelper(ctx, op, kube.RoleWipeDraft, 0); err != nil {
				return o.failDraft(ctx, op, app, stagePrepare, err)
			}
		}
	}
	// The preview runs against a COPY of production data. Iterating on an existing preview keeps its data.
	if app.LiveRelease > 0 && app.DraftState != store.DraftPreviewReady {
		o.step(ctx, op, "datacopy")
		if err := o.releaseHelper(ctx, op, kube.RoleDataCopy, 0); err != nil {
			return o.failDraft(ctx, op, app, stagePrepare, err)
		}
	}

	o.step(ctx, op, "agent")
	spec := kube.AgentSpec{AppID: op.AppID, OpID: op.ID, Kind: op.Kind, Prompt: op.Prompt, Token: Token(o.opt.TokenSecret, op.ID), SubPath: kube.SubPathDraft}
	if err := retry(ctx, func() error { return o.kube.EnsureAgentJob(ctx, spec) }); err != nil {
		return o.failDraft(ctx, op, app, stagePrepare, err)
	}
	ph, reason, err := o.waitJob(ctx, kube.JobName(op.AppID, kube.RoleAgent, op.ID), o.opt.AgentTimeout)
	if err != nil {
		return err
	}
	o.collectTrace(ctx, op)
	if ph != kube.JobSucceeded {
		return o.failDraft(ctx, op, app, stageAgent, fmt.Errorf("agent failed: %s", reason))
	}

	o.step(ctx, op, "preview")
	o.Emit(ctx, op.AppID, op.ID, store.PhaseStarting, "お試し版を用意しています")
	if err := retry(ctx, func() error { return o.kube.EnsurePreview(ctx, op.AppID, op.ID) }); err != nil {
		return o.failDraft(ctx, op, app, stagePrepare, err)
	}
	ready, why, err := o.waitDeployment(ctx, "preview", func(c context.Context) (bool, string, error) { return o.kube.PreviewReady(c, op.AppID) })
	if err != nil {
		return err
	}
	if !ready {
		return o.failDraft(ctx, op, app, stageRuntime, fmt.Errorf("%s", why))
	}
	_ = o.st.SetDraftState(ctx, op.AppID, store.DraftPreviewReady)
	if app.LiveRelease > 0 {
		o.Emit(ctx, op.AppID, op.ID, store.PhaseReady, "お試し版ができました(本番はそのままです)")
	} else {
		o.Emit(ctx, op.AppID, op.ID, store.PhasePreview, "お試し版ができました")
	}
	return nil
}

// failDraft reports a failed draft. Production is untouched; the draft goes back to the live release.
func (o *Orchestrator) failDraft(ctx context.Context, op store.Operation, app store.App, stage string, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	user, detail := o.explain(ctx, op, stage, cause)
	wrapped := &userError{cause: cause, user: user, detail: detail}
	_ = o.kube.DeletePreview(ctx, op.AppID)
	_ = o.st.SetDraftState(ctx, op.AppID, store.DraftNone)
	if app.LiveRelease > 0 {
		o.step(ctx, op, "reset")
		_ = o.releaseHelper(ctx, op, kube.RoleReset, app.LiveRelease) // best effort: the draft is disposable
		o.Emit(ctx, op.AppID, op.ID, store.PhaseReady, "変更は取り消されました(本番はそのままです)。"+user)
		return wrapped
	}
	o.Emit(ctx, op.AppID, op.ID, store.PhaseFailed, user)
	return wrapped
}

// ---- approve / rollback / discard ------------------------------------------

func (o *Orchestrator) runReleaseOp(ctx context.Context, op store.Operation) error {
	app, err := o.st.GetApp(ctx, op.AppID)
	if err != nil {
		return err
	}
	switch op.Kind {
	case store.KindApprove:
		return o.runApprove(ctx, op, app)
	case store.KindRollback:
		return o.runRollback(ctx, op, app)
	default:
		return o.runDiscard(ctx, op, app)
	}
}

func (o *Orchestrator) userFail(ctx context.Context, op store.Operation, app store.App, msg string, cause error) error {
	if app.LiveRelease > 0 {
		o.Emit(ctx, op.AppID, op.ID, store.PhaseReady, msg)
	} else {
		o.Emit(ctx, op.AppID, op.ID, store.PhasePreview, msg)
	}
	return &userError{cause: cause, user: msg, detail: cause.Error()}
}

func (o *Orchestrator) runApprove(ctx context.Context, op store.Operation, app store.App) error {
	n, convErr := strconv.Atoi(strings.TrimSpace(op.Prompt))
	if convErr != nil || n < 1 {
		return fmt.Errorf("approve: bad release number %q", op.Prompt)
	}
	if app.DraftState != store.DraftPreviewReady {
		return o.userFail(ctx, op, app, "承認できるお試し版がありません。", fmt.Errorf("draft state is %q", app.DraftState))
	}
	o.Emit(ctx, op.AppID, op.ID, store.PhaseStarting, "本番に反映しています")

	// 1) Make the immutable release while production keeps running (no downtime yet).
	o.step(ctx, op, "release")
	if err := o.releaseHelper(ctx, op, kube.RoleRelease, n); err != nil {
		return o.userFail(ctx, op, app, "版を確定できませんでした。本番はそのままです。", err)
	}
	logs, _ := o.kube.JobLogTail(ctx, op.AppID, kube.RoleRelease, op.ID, 50)
	m := hashRe.FindStringSubmatch(logs)
	if m == nil {
		return o.userFail(ctx, op, app, "版を確定できませんでした。本番はそのままです。", fmt.Errorf("no content hash in the release job log"))
	}

	// 2) Switch production. With a live release the data is snapshotted with production stopped,
	//    so the snapshot is consistent.
	snapshot := app.LiveRelease > 0
	if snapshot {
		o.step(ctx, op, "stop")
		if err := o.stopRuntime(ctx, op.AppID); err != nil {
			return o.userFail(ctx, op, app, "本番を止められませんでした。", err)
		}
		o.step(ctx, op, "datasnapshot")
		if err := o.releaseHelper(ctx, op, kube.RoleDSnap, n); err != nil {
			_ = o.kube.EnsureRuntimeAt(ctx, op.AppID, op.ID+"-undo", kube.ReleaseDir(app.LiveRelease))
			return o.userFail(ctx, op, app, "データを保存できませんでした。本番はそのままです。", err)
		}
	}
	prompt := o.lastDraftPrompt(ctx, op.AppID)
	if err := o.st.AddRelease(ctx, store.Release{AppID: op.AppID, N: n, SourceHash: m[1], Prompt: prompt, DataSnapshot: snapshot}); err != nil {
		return err
	}
	o.step(ctx, op, "runtime")
	if err := retry(ctx, func() error { return o.kube.EnsureRuntimeAt(ctx, op.AppID, op.ID, kube.ReleaseDir(n)) }); err != nil {
		return o.userFail(ctx, op, app, "本番に反映できませんでした。", err)
	}
	ready, why, err := o.waitRuntime(ctx, op.AppID)
	if err != nil {
		return err
	}
	if !ready {
		if app.LiveRelease > 0 { // put the previous release back
			_ = o.kube.EnsureRuntimeAt(ctx, op.AppID, op.ID+"-undo", kube.ReleaseDir(app.LiveRelease))
			o.waitRuntime(ctx, op.AppID)
		}
		return o.userFail(ctx, op, app, "新しい版が起動しなかったので、元の版のままにしました。", fmt.Errorf("%s", why))
	}
	_ = o.st.SetLiveRelease(ctx, op.AppID, n)
	_ = o.kube.DeletePreview(ctx, op.AppID)
	_ = o.st.SetDraftState(ctx, op.AppID, store.DraftNone)
	o.Emit(ctx, op.AppID, op.ID, store.PhaseReady, fmt.Sprintf("本番に反映しました(版 %d)", n))
	return nil
}

// lastDraftPrompt returns the request of the newest create/modify operation (what the release contains).
func (o *Orchestrator) lastDraftPrompt(ctx context.Context, appID string) string {
	ops, err := o.st.ListOperations(ctx, appID)
	if err != nil {
		return ""
	}
	for _, x := range ops { // newest first
		if (x.Kind == store.KindCreate || x.Kind == store.KindModify) && x.State == store.OpSucceeded {
			return x.Prompt
		}
	}
	return ""
}

func (o *Orchestrator) runRollback(ctx context.Context, op store.Operation, app store.App) error {
	withData := strings.Contains(op.Prompt, "withData=1")
	live := app.LiveRelease
	if live < 2 {
		return o.userFail(ctx, op, app, "戻せる前の版がありません。", fmt.Errorf("live release is %d", live))
	}
	target := live - 1
	cur, err := o.st.GetRelease(ctx, op.AppID, live)
	if err != nil {
		return err
	}
	o.Emit(ctx, op.AppID, op.ID, store.PhaseStarting, "前の版に戻しています")
	if withData && cur.DataSnapshot {
		o.step(ctx, op, "stop")
		if err := o.stopRuntime(ctx, op.AppID); err != nil {
			return o.userFail(ctx, op, app, "本番を止められませんでした。", err)
		}
		o.step(ctx, op, "datarestore")
		if err := o.releaseHelper(ctx, op, kube.RoleDRestore, live); err != nil {
			_ = o.kube.EnsureRuntimeAt(ctx, op.AppID, op.ID+"-undo", kube.ReleaseDir(live))
			return o.userFail(ctx, op, app, "データを戻せませんでした。今の版のままです。", err)
		}
	}
	o.step(ctx, op, "runtime")
	if err := retry(ctx, func() error { return o.kube.EnsureRuntimeAt(ctx, op.AppID, op.ID, kube.ReleaseDir(target)) }); err != nil {
		return o.userFail(ctx, op, app, "戻せませんでした。", err)
	}
	ready, why, err := o.waitRuntime(ctx, op.AppID)
	if err != nil {
		return err
	}
	if !ready {
		return o.userFail(ctx, op, app, "前の版が起動しませんでした。", fmt.Errorf("%s", why))
	}
	_ = o.st.SetLiveRelease(ctx, op.AppID, target)
	// The draft must not silently diverge from what is live: put it back too.
	_ = o.kube.DeletePreview(ctx, op.AppID)
	_ = o.st.SetDraftState(ctx, op.AppID, store.DraftNone)
	o.step(ctx, op, "reset")
	_ = o.releaseHelper(ctx, op, kube.RoleReset, target)
	o.Emit(ctx, op.AppID, op.ID, store.PhaseReady, fmt.Sprintf("版 %d に戻しました", target))
	return nil
}

func (o *Orchestrator) runDiscard(ctx context.Context, op store.Operation, app store.App) error {
	if app.LiveRelease == 0 {
		return o.userFail(ctx, op, app, "破棄する前に、本番に反映された版がありません。", fmt.Errorf("no live release"))
	}
	o.Emit(ctx, op.AppID, op.ID, store.PhaseStarting, "お試し版を破棄しています")
	_ = o.kube.DeletePreview(ctx, op.AppID)
	o.step(ctx, op, "reset")
	if err := o.releaseHelper(ctx, op, kube.RoleReset, app.LiveRelease); err != nil {
		return o.userFail(ctx, op, app, "下書きを戻せませんでした。", err)
	}
	_ = o.st.SetDraftState(ctx, op.AppID, store.DraftNone)
	o.Emit(ctx, op.AppID, op.ID, store.PhaseReady, "お試し版を破棄しました")
	return nil
}
