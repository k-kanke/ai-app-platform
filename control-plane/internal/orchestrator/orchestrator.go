// Package orchestrator drives app create / modify / delete operations.
//
// v1 is intentionally imperative: it walks through steps and calls the
// Kubernetes API in order. Every step is idempotent (deterministic names,
// AlreadyExists == success) so a Control Plane restart can simply run the
// unfinished operation again. Problems found here feed docs/dev-log.md and the
// decision whether a Reconciler is needed (plan-2 §12).
package orchestrator

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/diagnose"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/events"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/kube"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/store"
)

type Options struct {
	TokenSecret    string
	PollInterval   time.Duration
	RuntimeTimeout time.Duration
	HelperTimeout  time.Duration
	AgentTimeout   time.Duration
}

type Orchestrator struct {
	st     *store.Store
	kube   *kube.Client
	broker *events.Broker
	opt    Options
	log    *slog.Logger

	mu      sync.Mutex
	running map[string]bool // op id -> goroutine active
	wg      sync.WaitGroup
}

func New(st *store.Store, k *kube.Client, b *events.Broker, opt Options, log *slog.Logger) *Orchestrator {
	if opt.PollInterval == 0 {
		opt.PollInterval = 2 * time.Second
	}
	if opt.RuntimeTimeout == 0 {
		opt.RuntimeTimeout = 5 * time.Minute
	}
	if opt.HelperTimeout == 0 {
		opt.HelperTimeout = 10 * time.Minute
	}
	if opt.AgentTimeout == 0 {
		opt.AgentTimeout = 25 * time.Minute
	}
	return &Orchestrator{st: st, kube: k, broker: b, opt: opt, log: log, running: map[string]bool{}}
}

// Token returns the per-operation bearer token the Agent uses to post progress.
func Token(secret, opID string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(opID))
	return hex.EncodeToString(m.Sum(nil))
}

func (o *Orchestrator) ValidToken(opID, token string) bool {
	return hmac.Equal([]byte(Token(o.opt.TokenSecret, opID)), []byte(token))
}

// Resume restarts unfinished operations after a Control Plane restart.
func (o *Orchestrator) Resume(ctx context.Context) error {
	ops, err := o.st.UnfinishedOperations(ctx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		o.log.Info("resuming operation", "op", op.ID, "app", op.AppID, "kind", op.Kind, "step", op.Step)
		o.Submit(ctx, op)
	}
	return nil
}

// Submit runs op asynchronously (at most once per op id).
func (o *Orchestrator) Submit(ctx context.Context, op store.Operation) {
	o.mu.Lock()
	if o.running[op.ID] {
		o.mu.Unlock()
		return
	}
	o.running[op.ID] = true
	o.mu.Unlock()
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer func() { o.mu.Lock(); delete(o.running, op.ID); o.mu.Unlock() }()
		o.run(ctx, op)
	}()
}

// Wait blocks until all in-flight operations return (used by tests / shutdown).
func (o *Orchestrator) Wait() { o.wg.Wait() }

// Emit records a progress event, updates the app phase (when phase is one) and notifies SSE clients.
func (o *Orchestrator) Emit(ctx context.Context, appID, opID, phase, msg string) {
	if phase != "" {
		if err := o.st.SetAppPhase(ctx, appID, phase); err != nil {
			o.log.Error("set app phase", "app", appID, "err", err)
		}
	}
	e, err := o.st.AddEvent(ctx, appID, opID, phase, msg)
	if err != nil {
		o.log.Error("add event", "app", appID, "err", err)
		return
	}
	o.broker.Publish(e)
}

func (o *Orchestrator) step(ctx context.Context, op store.Operation, step string) {
	if err := o.st.UpdateOperation(ctx, op.ID, store.OpRunning, step, ""); err != nil {
		o.log.Error("update op", "op", op.ID, "err", err)
	}
}

func (o *Orchestrator) run(ctx context.Context, op store.Operation) {
	var err error
	switch op.Kind {
	case store.KindCreate, store.KindModify:
		err = o.runBuild(ctx, op)
	case store.KindDelete:
		err = o.runDelete(ctx, op)
	default:
		err = fmt.Errorf("unknown operation kind %q", op.Kind)
	}
	if ctx.Err() != nil {
		return // shutting down: leave RUNNING so Resume() picks it up
	}
	if err != nil {
		o.log.Error("operation failed", "op", op.ID, "app", op.AppID, "err", err)
		var ue *userError
		if errors.As(err, &ue) {
			_ = o.st.FailOperation(ctx, op.ID, "failed", err.Error(), ue.user, ue.detail)
		} else {
			_ = o.st.FailOperation(ctx, op.ID, "failed", err.Error(), fallbackMessage, "")
		}
		return
	}
	_ = o.st.UpdateOperation(ctx, op.ID, store.OpSucceeded, "done", "")
}

// retry runs fn up to 3 times for transient API errors.
func retry(ctx context.Context, fn func() error) error {
	var err error
	for i := 1; i <= 3; i++ {
		if err = fn(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i) * 200 * time.Millisecond):
		}
	}
	return err
}

func (o *Orchestrator) waitJob(ctx context.Context, name string, timeout time.Duration) (kube.JobPhase, string, error) {
	deadline := time.Now().Add(timeout)
	for {
		ph, reason, err := o.kube.JobStatus(ctx, name)
		if err != nil {
			return "", "", err
		}
		switch ph {
		case kube.JobSucceeded, kube.JobFailed:
			return ph, reason, nil
		case kube.JobMissing:
			return ph, "job disappeared", nil
		}
		if time.Now().After(deadline) {
			return kube.JobFailed, "timed out waiting for job " + name, nil
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(o.opt.PollInterval):
		}
	}
}

// helper runs a trusted helper Job (init/snap/restore) and waits for it.
func (o *Orchestrator) helper(ctx context.Context, op store.Operation, role string) error {
	if err := retry(ctx, func() error { return o.kube.EnsureHelperJob(ctx, op.AppID, role, op.ID) }); err != nil {
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

func (o *Orchestrator) waitRuntime(ctx context.Context, appID string) (bool, string, error) {
	deadline := time.Now().Add(o.opt.RuntimeTimeout)
	last := ""
	for {
		ready, detail, err := o.kube.RuntimeReady(ctx, appID)
		if err != nil {
			return false, "", err
		}
		if ready {
			return true, "", nil
		}
		last = detail
		if time.Now().After(deadline) {
			return false, "runtime not ready: " + last, nil
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(o.opt.PollInterval):
		}
	}
}

// userError carries a message for the family and the raw detail for operators.
type userError struct {
	cause  error
	user   string
	detail string
}

func (e *userError) Error() string { return e.cause.Error() }
func (e *userError) Unwrap() error { return e.cause }

const fallbackMessage = "うまく作れませんでした。言い方を変えて、もう一度お試しください。"

// Failure stages decide the wording shown to the user.
const (
	stagePrepare = "prepare" // workspace, helper jobs, creating the agent
	stageAgent   = "agent"   // the Agent ran and failed
	stageRuntime = "runtime" // the app was generated but did not start
)

func (o *Orchestrator) runBuild(ctx context.Context, op store.Operation) error {
	modify := op.Kind == store.KindModify
	o.Emit(ctx, op.AppID, op.ID, store.PhaseAgentStarting, "準備しています")

	o.step(ctx, op, "workspace")
	if err := retry(ctx, func() error { return o.kube.EnsureWorkspace(ctx, op.AppID) }); err != nil {
		return o.fail(ctx, op, modify, false, stagePrepare, err)
	}
	if !modify {
		o.step(ctx, op, "init")
		if err := o.helper(ctx, op, kube.RoleInit); err != nil {
			return o.fail(ctx, op, modify, false, stagePrepare, err)
		}
		if op.Purge { // retry of a failed create: start from an empty source
			o.step(ctx, op, "wipe")
			if err := o.helper(ctx, op, kube.RoleWipe); err != nil {
				return o.fail(ctx, op, modify, false, stagePrepare, err)
			}
		}
	} else {
		// Stage 1 safety net: save the working source before the Agent touches it.
		o.step(ctx, op, "snapshot")
		if err := o.helper(ctx, op, kube.RoleSnapshot); err != nil {
			return o.fail(ctx, op, modify, false, stagePrepare, err)
		}
	}

	o.step(ctx, op, "agent")
	spec := kube.AgentSpec{AppID: op.AppID, OpID: op.ID, Kind: op.Kind, Prompt: op.Prompt, Token: Token(o.opt.TokenSecret, op.ID)}
	if err := retry(ctx, func() error { return o.kube.EnsureAgentJob(ctx, spec) }); err != nil {
		return o.fail(ctx, op, modify, true, stagePrepare, err)
	}
	// Completion is decided by the Job status, not by the Agent's own progress reports.
	ph, reason, err := o.waitJob(ctx, kube.JobName(op.AppID, kube.RoleAgent, op.ID), o.opt.AgentTimeout)
	if err != nil {
		return err
	}
	if ph != kube.JobSucceeded {
		return o.fail(ctx, op, modify, true, stageAgent, fmt.Errorf("agent failed: %s", reason))
	}

	o.step(ctx, op, "runtime")
	o.Emit(ctx, op.AppID, op.ID, store.PhaseStarting, "アプリを起動しています")
	if err := retry(ctx, func() error { return o.kube.EnsureRuntime(ctx, op.AppID, op.ID) }); err != nil {
		return o.fail(ctx, op, modify, true, stagePrepare, err)
	}
	ready, why, err := o.waitRuntime(ctx, op.AppID)
	if err != nil {
		return err
	}
	if !ready {
		return o.fail(ctx, op, modify, true, stageRuntime, fmt.Errorf("%s", why))
	}
	o.Emit(ctx, op.AppID, op.ID, store.PhaseReady, "使えるようになりました")
	return nil
}

// explain builds the user-facing message and operator detail for a failure.
func (o *Orchestrator) explain(ctx context.Context, op store.Operation, stage string, cause error) (user, detail string) {
	switch stage {
	case stageAgent:
		tail := ""
		if t, err := o.kube.AgentLogTail(ctx, op.AppID, op.ID, 80); err == nil {
			tail = t
		}
		return diagnose.Classify(tail, cause.Error()), diagnose.Tail(tail, 60, 6000)
	case stageRuntime:
		return "アプリは作れましたが、起動しませんでした。言い方を変えて、もう一度お試しください。", cause.Error()
	default:
		return "準備の途中で問題が起きました。少し待ってから、もう一度お試しください。", cause.Error()
	}
}

// fail marks the outcome. For a modify, the previous source is restored (if the
// Agent may have changed it) so the app family members are using keeps working.
func (o *Orchestrator) fail(ctx context.Context, op store.Operation, modify, agentRan bool, stage string, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	user, detail := o.explain(ctx, op, stage, cause)
	wrapped := &userError{cause: cause, user: user, detail: detail}
	if !modify {
		o.Emit(ctx, op.AppID, op.ID, store.PhaseFailed, user)
		return wrapped
	}
	o.Emit(ctx, op.AppID, op.ID, store.PhaseStarting, "変更できませんでした。元の状態に戻しています")
	if agentRan {
		o.step(ctx, op, "restore")
		if err := o.helper(ctx, op, kube.RoleRestore); err != nil {
			o.Emit(ctx, op.AppID, op.ID, store.PhaseFailed, "元に戻せませんでした: "+err.Error())
			return &userError{cause: fmt.Errorf("%v; restore also failed: %w", cause, err), user: "変更に失敗し、元に戻すこともできませんでした。管理者に連絡してください。", detail: detail}
		}
		// Roll the runtime so it runs the restored source again.
		if err := retry(ctx, func() error { return o.kube.EnsureRuntime(ctx, op.AppID, op.ID+"-restored") }); err == nil {
			o.waitRuntime(ctx, op.AppID)
		}
	}
	o.Emit(ctx, op.AppID, op.ID, store.PhaseReady, "変更は取り消されました(元のアプリはそのまま使えます)。"+user)
	return wrapped
}

func (o *Orchestrator) runDelete(ctx context.Context, op store.Operation) error {
	o.Emit(ctx, op.AppID, op.ID, store.PhaseDeleting, "削除しています")
	o.step(ctx, op, "delete")
	if err := retry(ctx, func() error { return o.kube.DeleteApp(ctx, op.AppID, op.Purge) }); err != nil {
		o.Emit(ctx, op.AppID, op.ID, store.PhaseFailed, "削除できませんでした: "+err.Error())
		return err
	}
	o.Emit(ctx, op.AppID, op.ID, store.PhaseDeleted, "削除しました")
	return nil
}

// ReconcileIngresses makes sure every READY app has its public Ingress. It covers
// apps created before Ingress was configured and Ingresses removed by hand.
// Apps with an operation in flight are skipped; that operation owns their resources.
func (o *Orchestrator) ReconcileIngresses(ctx context.Context) error {
	if !o.kube.IngressEnabled() {
		return nil
	}
	apps, err := o.st.ListApps(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, a := range apps {
		if a.Phase != store.PhaseReady {
			continue
		}
		if _, err := o.st.ActiveOperation(ctx, a.ID); err == nil {
			continue
		}
		created, err := o.kube.EnsureIngress(ctx, a.ID)
		if err != nil {
			o.log.Error("ensure ingress", "app", a.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if created {
			o.log.Info("created missing ingress", "app", a.ID)
		}
	}
	return firstErr
}

// RunReconcileLoop reconciles once immediately and then every interval until ctx ends.
func (o *Orchestrator) RunReconcileLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		_ = o.ReconcileIngresses(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
