package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/diagnose"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/github"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/kube"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/patch"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/store"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/trace"
)

// Repository tasks (docs/multi-agent-execution-plan.md Phase 1-2). Same style as the app flows: every step is
// idempotent (deterministic names, AlreadyExists == success), so after a restart an unfinished task simply runs again.

// TaskToken is the bearer token an Implementer uses for its own task (progress events, patch upload).
func TaskToken(secret, id string) string { return Token(secret, store.TaskKey(id)) }

func (o *Orchestrator) ValidTaskToken(id, token string) bool {
	return o.ValidToken(store.TaskKey(id), token)
}

// EmitTask records a progress event on the task's stream and notifies SSE clients.
func (o *Orchestrator) EmitTask(ctx context.Context, id, phase, msg string) {
	e, err := o.st.AddEvent(ctx, store.TaskKey(id), id, phase, msg)
	if err != nil {
		o.log.Error("add task event", "task", id, "err", err)
		return
	}
	o.broker.Publish(e)
}

// ResumeTasks restarts tasks that were running when the previous process stopped.
func (o *Orchestrator) ResumeTasks(ctx context.Context) error {
	ts, err := o.st.UnfinishedTasks(ctx)
	if err != nil {
		return err
	}
	for _, t := range ts {
		o.log.Info("resuming task", "task", t.ID, "state", t.State, "step", t.Step)
		if t.State == store.TaskPublishing {
			o.SubmitPublish(ctx, t)
			continue
		}
		o.SubmitTask(ctx, t)
	}
	return nil
}

// SubmitTask runs the task asynchronously (at most once per task id).
func (o *Orchestrator) SubmitTask(ctx context.Context, t store.Task) {
	key := store.TaskKey(t.ID)
	o.mu.Lock()
	if o.running[key] {
		o.mu.Unlock()
		return
	}
	o.running[key] = true
	o.mu.Unlock()
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer func() { o.mu.Lock(); delete(o.running, key); o.mu.Unlock() }()
		o.runTask(ctx, t)
	}()
}

func (o *Orchestrator) taskStep(ctx context.Context, id, step string) {
	if err := o.st.SetTaskState(ctx, id, store.TaskRunning, step); err != nil {
		o.log.Error("update task", "task", id, "err", err)
	}
}

func (o *Orchestrator) runTask(ctx context.Context, t store.Task) {
	if err := o.runImplementer(ctx, t); err != nil && ctx.Err() == nil {
		o.log.Error("task failed", "task", t.ID, "err", err)
	}
}

func (o *Orchestrator) runImplementer(ctx context.Context, t store.Task) error {
	o.EmitTask(ctx, t.ID, store.TaskRunning, "準備しています")
	o.taskStep(ctx, t.ID, "workspace")
	if err := retry(ctx, func() error { return o.kube.EnsureTaskWorkspace(ctx, t.ID) }); err != nil {
		return o.failTask(ctx, t, "workspace", "準備の途中で問題が起きました。少し待ってから、もう一度お試しください。", err)
	}

	o.taskStep(ctx, t.ID, "implementer")
	o.EmitTask(ctx, t.ID, store.TaskRunning, "リポジトリを取得して、作業を始めます")
	spec := kube.TaskSpec{ID: t.ID, Repo: t.Repo, Prompt: t.Prompt, Token: TaskToken(o.opt.TokenSecret, t.ID)}
	if err := retry(ctx, func() error { return o.kube.EnsureImplementerJob(ctx, spec) }); err != nil {
		return o.failTask(ctx, t, "implementer", "作業を始められませんでした。少し待ってから、もう一度お試しください。", err)
	}
	ph, reason, err := o.waitTaskJob(ctx, t.ID, o.opt.AgentTimeout+5*time.Minute)
	if err != nil {
		return err // context cancelled: the next start resumes this task
	}
	o.collectTaskTrace(ctx, t.ID)
	if ph != kube.JobSucceeded {
		tail, _ := o.kube.TaskLogTail(ctx, t.ID, kube.RoleImplementer, 80)
		return o.failTaskWith(ctx, t, "implementer", diagnose.ClassifyTask(tail, reason), diagnose.Tail(tail, 60, 6000),
			fmt.Errorf("implementer failed: %s", reason))
	}

	data, _, err := o.st.GetTaskPatch(ctx, t.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && len(data) == 0) {
		_ = o.st.SetTaskState(ctx, t.ID, store.TaskNoChanges, "done")
		o.EmitTask(ctx, t.ID, store.TaskNoChanges, "変更はありませんでした")
		return nil
	}
	if err != nil {
		return o.failTask(ctx, t, "patch", "結果を読み込めませんでした。", err)
	}
	if s := patch.Summarize(data); s.Blocked {
		return o.failTask(ctx, t, "patch", "公開できない変更が含まれていたため、止めました(.git の中や、リポジトリの外を指すパス)。",
			fmt.Errorf("blocked patch: %v", s.Risks))
	}
	if err := o.st.SetTaskState(ctx, t.ID, store.TaskAwaitingApproval, "approval"); err != nil {
		return err
	}
	o.EmitTask(ctx, t.ID, store.TaskAwaitingApproval, "差分ができました。内容を確認して、承認してください")
	return nil
}

func (o *Orchestrator) failTask(ctx context.Context, t store.Task, step, userMsg string, cause error) error {
	return o.failTaskWith(ctx, t, step, userMsg, "", cause)
}

func (o *Orchestrator) failTaskWith(ctx context.Context, t store.Task, step, userMsg, detail string, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if detail == "" {
		detail = cause.Error()
	}
	if err := o.st.FailTask(ctx, t.ID, step, cause.Error(), userMsg, detail); err != nil {
		o.log.Error("fail task", "task", t.ID, "err", err)
	}
	o.EmitTask(ctx, t.ID, store.TaskFailed, userMsg)
	return cause
}

func (o *Orchestrator) waitTaskJob(ctx context.Context, id string, timeout time.Duration) (kube.JobPhase, string, error) {
	deadline := time.Now().Add(timeout)
	for {
		ph, reason, err := o.kube.TaskJobStatus(ctx, id)
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
			return kube.JobFailed, "timed out waiting for the implementer job", nil
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(o.opt.PollInterval):
		}
	}
}

func (o *Orchestrator) collectTaskTrace(ctx context.Context, id string) {
	logs, err := o.kube.TaskLogTail(ctx, id, kube.RoleImplementer, 200)
	if err != nil {
		return
	}
	if tr := trace.Extract(logs); tr != "" {
		_ = o.st.SetTaskTrace(ctx, id, tr)
	}
}

// ReconcileTasks removes the workspace and patch of tasks past retention, and marks them EXPIRED.
// A task that is still PENDING or RUNNING is never touched, so an active task keeps its workspace.
func (o *Orchestrator) ReconcileTasks(ctx context.Context) error {
	if o.opt.TaskRetention <= 0 {
		return nil
	}
	old, err := o.st.ExpiredTasks(ctx, time.Now().Add(-o.opt.TaskRetention))
	if err != nil {
		return err
	}
	var firstErr error
	for _, t := range old {
		if err := o.kube.DeleteTask(ctx, t.ID); err != nil {
			o.log.Error("delete task resources", "task", t.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		_ = o.st.DeleteTaskPatch(ctx, t.ID)
		if err := o.st.SetTaskState(ctx, t.ID, store.TaskExpired, ""); err != nil {
			o.log.Error("expire task", "task", t.ID, "err", err)
			continue
		}
		o.EmitTask(ctx, t.ID, store.TaskExpired, "保持期間が過ぎたため、作業場と差分を削除しました")
		o.log.Info("expired task", "task", t.ID, "was", t.State)
	}
	return firstErr
}

// RunTaskReconcileLoop expires old tasks now and then every interval until ctx ends.
func (o *Orchestrator) RunTaskReconcileLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		_ = o.ReconcileTasks(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- Publishing (Phase 4) ----

// PublisherToken authorises the Publisher Job of one task: reading its approved patch and reporting the PR.
// It differs from TaskToken, so an Implementer's token cannot be used to read or publish.
func PublisherToken(secret, id string) string { return Token(secret, "publish:"+id) }

func (o *Orchestrator) ValidPublisherToken(id, token string) bool {
	return o.ValidToken("publish:"+id, token)
}

// PublishEnabled reports whether approved tasks are turned into pull requests.
func (o *Orchestrator) PublishEnabled() bool { return o.opt.GitHub != nil }

// SubmitPublish runs the publish step asynchronously (at most once per task id).
func (o *Orchestrator) SubmitPublish(ctx context.Context, t store.Task) {
	key := store.TaskKey(t.ID) + ":pub"
	o.mu.Lock()
	if o.running[key] {
		o.mu.Unlock()
		return
	}
	o.running[key] = true
	o.mu.Unlock()
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer func() { o.mu.Lock(); delete(o.running, key); o.mu.Unlock() }()
		if err := o.runPublish(ctx, t); err != nil && ctx.Err() == nil {
			o.log.Error("publish failed", "task", t.ID, "err", err)
		}
	}()
}

func prTitle(prompt string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(prompt), "\n", 2)[0])
	if r := []rune(line); len(r) > 70 {
		line = string(r[:70]) + "…"
	}
	return line
}

func (o *Orchestrator) runPublish(ctx context.Context, t store.Task) error {
	if o.opt.GitHub == nil {
		return o.failTask(ctx, t, "publish", "GitHub につながっていないため、PR を作れません。", errors.New("github app is not configured"))
	}
	o.EmitTask(ctx, t.ID, store.TaskPublishing, "PR を作っています")
	var tok github.Token
	if err := retry(ctx, func() error {
		var err error
		tok, err = o.opt.GitHub.InstallationToken(ctx, t.Repo)
		return err
	}); err != nil {
		return o.failTask(ctx, t, "publish", "GitHub の権限を取得できませんでした。GitHub App がこのリポジトリに入っているか確認してください。", err)
	}
	spec := kube.PublishSpec{
		ID: t.ID, Repo: t.Repo, PatchSHA: t.PatchSHA, GitHubToken: tok.Value, PublisherToken: PublisherToken(o.opt.TokenSecret, t.ID),
		Title: prTitle(t.Prompt),
		Body:  "## 依頼\n\n" + t.Prompt + "\n\n---\ngenkan のタスク `" + t.ID + "` から作られました。内容を確認してから、マージしてください。",
	}
	if err := retry(ctx, func() error { return o.kube.EnsurePublisherJob(ctx, spec) }); err != nil {
		return o.failTask(ctx, t, "publish", "PR を作る準備に失敗しました。", err)
	}
	ph, reason, err := o.waitPublisherJob(ctx, t.ID, 10*time.Minute)
	if err != nil {
		return err
	}
	if ph != kube.JobSucceeded {
		tail, _ := o.kube.TaskLogTail(ctx, t.ID, kube.RolePublisher, 60)
		return o.failTaskWith(ctx, t, "publish", diagnose.ClassifyPublish(tail, reason), diagnose.Tail(tail, 60, 6000),
			fmt.Errorf("publisher failed: %s", reason))
	}
	cur, err := o.st.GetTask(ctx, t.ID)
	if err != nil {
		return err
	}
	if cur.State != store.TaskPublished || cur.PRURL == "" {
		return o.failTask(ctx, t, "publish", "PR は作られましたが、その情報を受け取れませんでした。GitHub を確認してください。",
			fmt.Errorf("publisher finished but the task is %s without a PR url", cur.State))
	}
	return nil
}

func (o *Orchestrator) waitPublisherJob(ctx context.Context, id string, timeout time.Duration) (kube.JobPhase, string, error) {
	deadline := time.Now().Add(timeout)
	for {
		ph, reason, err := o.kube.PublisherJobStatus(ctx, id)
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
			return kube.JobFailed, "timed out waiting for the publisher job", nil
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(o.opt.PollInterval):
		}
	}
}
