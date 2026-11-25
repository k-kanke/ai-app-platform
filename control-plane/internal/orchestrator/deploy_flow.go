package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/config"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/github"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/store"
)

// The deploy step: after a task's pull request is opened, follow it. When a person merges it, wait for CI to push the new
// image, restart the repository's Deployment onto it, and report when the new image is running. It is driven by a polling loop
// over the tasks in PUBLISHED / MERGED / DEPLOYING, so a Control Plane restart simply continues where it left off.
//
//   PUBLISHED --(PR merged)--> MERGED --(CI pushed the image tagged with the merge commit)--> DEPLOYING --(rolled out)--> DEPLOYED
//       \--(PR closed)--> CLOSED          \--(no new image in time)--> FAILED          \--(too slow)--> FAILED
//
// Only repositories with a deploy target are followed; the Control Plane may restart exactly the Deployments listed there.

// ImageRegistry tells what a tag currently points to.
type ImageRegistry interface {
	Digest(ctx context.Context, image, tag string) (string, error)
}

// DeployTarget returns the deploy target of repo.
func (o *Orchestrator) DeployTarget(repo string) (config.DeployTarget, bool) {
	for _, t := range o.opt.Deploy {
		if t.Repo == repo {
			return t, true
		}
	}
	return config.DeployTarget{}, false
}

// DeployEnabled reports whether merged pull requests are rolled out.
func (o *Orchestrator) DeployEnabled() bool {
	return len(o.opt.Deploy) > 0 && o.opt.GitHub != nil && o.opt.Registry != nil
}

type tokenCache struct {
	mu sync.Mutex
	m  map[string]github.Token
}

// installToken returns a GitHub installation token for repo, reusing one that is still good for a few minutes.
func (o *Orchestrator) installToken(ctx context.Context, repo string) (github.Token, error) {
	o.tokens.mu.Lock()
	defer o.tokens.mu.Unlock()
	if t, ok := o.tokens.m[repo]; ok && time.Until(t.ExpiresAt) > 5*time.Minute {
		return t, nil
	}
	t, err := o.opt.GitHub.InstallationToken(ctx, repo)
	if err != nil {
		return github.Token{}, err
	}
	if o.tokens.m == nil {
		o.tokens.m = map[string]github.Token{}
	}
	o.tokens.m[repo] = t
	return t, nil
}

// RunDeployLoop checks once now and then every interval until ctx ends.
func (o *Orchestrator) RunDeployLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := o.CheckDeploys(ctx); err != nil && ctx.Err() == nil {
			o.log.Error("deploy check", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CheckDeploys makes one pass over the tasks that are being followed.
func (o *Orchestrator) CheckDeploys(ctx context.Context) error {
	if !o.DeployEnabled() {
		return nil
	}
	ts, err := o.st.WatchedTasks(ctx)
	if err != nil {
		return err
	}
	for _, t := range ts {
		target, ok := o.DeployTarget(t.Repo)
		if !ok {
			continue // not a repository that is deployed: the task ends at its pull request
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		o.advanceDeploy(ctx, t, target)
	}
	return nil
}

func (o *Orchestrator) advanceDeploy(ctx context.Context, t store.Task, target config.DeployTarget) {
	switch t.State {
	case store.TaskPublished:
		o.watchPR(ctx, t)
	case store.TaskMerged:
		o.waitForImage(ctx, t, target)
	case store.TaskDeploying:
		o.waitForRollout(ctx, t, target)
	}
}

func (o *Orchestrator) watchPR(ctx context.Context, t store.Task) {
	if t.PRNumber <= 0 {
		return
	}
	tok, err := o.installToken(ctx, t.Repo)
	if err != nil {
		o.log.Warn("deploy: github token", "task", t.ID, "err", err)
		return
	}
	st, err := github.Client{Token: tok.Value, APIBase: o.opt.GitHub.APIBase}.PRStatus(ctx, t.Repo, t.PRNumber)
	if err != nil {
		o.log.Warn("deploy: pull request status", "task", t.ID, "err", err)
		return
	}
	switch {
	case st.Merged:
		if o.st.MarkMerged(ctx, t.ID, st.MergeCommitSHA) == nil {
			o.EmitTask(ctx, t.ID, store.TaskMerged, "PR がマージされました。新しいイメージができるのを待っています")
		}
	case st.State == "closed":
		if o.st.TransitionTask(ctx, t.ID, store.TaskPublished, store.TaskClosed) == nil {
			o.EmitTask(ctx, t.ID, store.TaskClosed, "PR は、マージされずに閉じられました")
		}
	}
}

// running reports the digests the target's Ready pods run, and the digest the registry has for the tag now.
func (o *Orchestrator) imageState(ctx context.Context, target config.DeployTarget) (running []string, latest string, err error) {
	latest, err = o.opt.Registry.Digest(ctx, target.Image, target.Tag)
	if err != nil {
		return nil, "", fmt.Errorf("registry: %w", err)
	}
	running, err = o.kube.ReadyDigests(ctx, target.Namespace, target.Deployment, target.Container)
	if err != nil {
		return nil, "", err
	}
	return running, latest, nil
}

func allEqual(ds []string, want string) bool {
	if len(ds) == 0 {
		return false
	}
	for _, d := range ds {
		if d != want {
			return false
		}
	}
	return true
}

func (o *Orchestrator) failDeploy(ctx context.Context, t store.Task, step, userMsg string, cause error) {
	if err := o.st.FailTask(ctx, t.ID, step, cause.Error(), userMsg, cause.Error()); err != nil {
		o.log.Error("fail task", "task", t.ID, "err", err)
		return
	}
	o.EmitTask(ctx, t.ID, store.TaskFailed, userMsg)
}

func shaTag(target config.DeployTarget, sha string) string {
	if len(sha) > 7 {
		sha = sha[:7]
	}
	return target.SHATagPrefix + sha
}

func (o *Orchestrator) waitForImage(ctx context.Context, t store.Task, target config.DeployTarget) {
	running, latest, err := o.imageState(ctx, target)
	if err != nil {
		o.log.Warn("deploy: image state", "task", t.ID, "err", err) // transient: try again next time
		return
	}
	ready := false
	if t.MergeSHA != "" {
		// CI is done for this merge exactly when the image tagged with the merge commit exists. An older image in the
		// registry (a push unrelated to this pull request) must not count as "the new image".
		if _, err := o.opt.Registry.Digest(ctx, target.Image, shaTag(target, t.MergeSHA)); err == nil {
			ready = true
		}
	} else {
		ready = !allEqual(running, latest) // no merge commit to look for: any change in the registry will have to do
	}
	if ready {
		if allEqual(running, latest) {
			// Already running what the registry has (restarted by hand, or by an earlier task).
			if o.st.TransitionTask(ctx, t.ID, store.TaskMerged, store.TaskDeployed) == nil {
				o.EmitTask(ctx, t.ID, store.TaskDeployed, "すでに新しいイメージで動いています")
			}
			return
		}
		if err := o.kube.RestartDeployment(ctx, target.Namespace, target.Deployment, t.ID); err != nil {
			o.failDeploy(ctx, t, "deploy", "反映のための再起動に失敗しました。", err)
			return
		}
		if o.st.TransitionTask(ctx, t.ID, store.TaskMerged, store.TaskDeploying) == nil {
			o.EmitTask(ctx, t.ID, store.TaskDeploying, "新しいイメージを見つけました。再起動しています")
		}
		return
	}
	if time.Since(t.UpdatedAt) > o.opt.DeployImageWait {
		o.failDeploy(ctx, t, "deploy", "マージされましたが、新しいイメージができませんでした。CI(GitHub Actions)の結果を確認してください。",
			fmt.Errorf("no image for the merge commit %s of %s within %s", shaTag(target, t.MergeSHA), target.Image, o.opt.DeployImageWait))
	}
}

func (o *Orchestrator) waitForRollout(ctx context.Context, t store.Task, target config.DeployTarget) {
	done, why, err := o.kube.RolloutComplete(ctx, target.Namespace, target.Deployment)
	if err != nil {
		o.log.Warn("deploy: rollout status", "task", t.ID, "err", err)
		return
	}
	if done {
		running, latest, ierr := o.imageState(ctx, target)
		if ierr == nil && allEqual(running, latest) {
			if o.st.TransitionTask(ctx, t.ID, store.TaskDeploying, store.TaskDeployed) == nil {
				o.EmitTask(ctx, t.ID, store.TaskDeployed, "反映しました。新しいイメージで動いています")
			}
			return
		}
		why = "the pods do not run the registry's current image yet"
	}
	if time.Since(t.UpdatedAt) > o.opt.DeployRolloutWait {
		o.failDeploy(ctx, t, "deploy", "再起動しましたが、新しいイメージで起動しきりませんでした。Pod の状態を確認してください。",
			fmt.Errorf("rollout did not finish within %s: %s", o.opt.DeployRolloutWait, why))
	}
}
