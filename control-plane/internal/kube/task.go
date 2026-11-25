package kube

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Repository tasks (docs/multi-agent-permissions.md): one workspace PVC per task and short-lived
// Jobs in a namespace of their own (cfg.TasksNamespace). They never share resources with generated apps.

const (
	LabelTaskID     = "aap.dev/task-id"
	RoleImplementer = "implementer"

	TaskWorkspaceMount = "/workspace"
	TaskRepoDir        = "/workspace/repo"
)

var repoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// ValidRepo accepts "owner/name" in GitHub's character set. It is checked again where the clone URL is built.
func ValidRepo(repo string) bool {
	return repoRe.MatchString(repo) && !strings.Contains(repo, "..") && !strings.HasSuffix(repo, ".git")
}

// TaskWorkspacePVC / TaskJobName are derived from the task id, so a retry after a restart is idempotent.
func TaskWorkspacePVC(id string) string { return "aap-task-" + id + "-ws" }
func TaskJobName(id string) string      { return "aap-task-" + id + "-impl" }

func taskLabels(id, role string) map[string]string {
	return map[string]string{LabelManaged: ManagedByName, LabelTaskID: id, LabelRole: role}
}

// EnsureTaskWorkspace creates the task's workspace PVC (local-path: bound on first use, to whichever node
// the scheduler picks for the Implementer). An existing PVC is kept.
func (c *Client) EnsureTaskWorkspace(ctx context.Context, id string) error {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: TaskWorkspacePVC(id), Namespace: c.cfg.TasksNamespace, Labels: taskLabels(id, "workspace")},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(c.cfg.TaskWorkspaceSize)},
			},
		},
	}
	if c.cfg.StorageClass != "" {
		sc := c.cfg.StorageClass
		pvc.Spec.StorageClassName = &sc
	}
	_, err := c.cs.CoreV1().PersistentVolumeClaims(c.cfg.TasksNamespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create task workspace %s: %w", pvc.Name, err)
	}
	return nil
}

// TaskSpec describes one Implementer run.
type TaskSpec struct {
	ID, Repo, Prompt, Token string
}

// cloneScript runs in the init container, where the Implementer's network policy still applies but
// nothing sensitive exists. A retry starts from a clean directory.
const cloneScript = `set -eu
export HOME=/tmp/home GIT_TERMINAL_PROMPT=0 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
mkdir -p "$HOME"
rm -rf /workspace/repo /workspace/out
git clone --depth 1 --no-tags -- "$AAP_GIT_BASE/$AAP_REPO.git" /workspace/repo
mkdir -p /workspace/out`

// EnsureImplementerJob starts the Implementer: an init container clones the repository into the
// workspace, then the agent works on it. The pod has no ServiceAccount token, no Secret volume
// beyond the optional LLM credentials Secret (to be replaced by the LLM gateway, Phase 3), and
// cannot write outside the workspace and /tmp.
func (c *Client) EnsureImplementerJob(ctx context.Context, t TaskSpec) error {
	if !ValidRepo(t.Repo) {
		return fmt.Errorf("invalid repository %q", t.Repo)
	}
	lbl := taskLabels(t.ID, RoleImplementer)
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: TaskJobName(t.ID), Namespace: c.cfg.TasksNamespace, Labels: lbl},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32p(0),
			ActiveDeadlineSeconds:   int64p(int64(c.cfg.AgentTimeout.Seconds()) + 300), // + time to clone
			TTLSecondsAfterFinished: int32p(int32(c.cfg.TaskRetention.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: boolp(false),
					SecurityContext:              podSC(),
					Volumes: []corev1.Volume{
						pvcVolume("workspace", TaskWorkspacePVC(t.ID)),
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
							SizeLimit: resourcePtr("1Gi")}}},
					},
				},
			},
		},
	}
	mounts := []corev1.VolumeMount{{Name: "workspace", MountPath: TaskWorkspaceMount}, {Name: "tmp", MountPath: "/tmp"}}
	sc := restrictedSC(AppUID)
	sc.ReadOnlyRootFilesystem = boolp(true)
	spec := &j.Spec.Template.Spec
	spec.InitContainers = []corev1.Container{{
		Name: "clone", Image: c.cfg.AgentImage, ImagePullPolicy: c.pullPolicy(),
		Command: []string{"sh", "-c", cloneScript},
		Env: []corev1.EnvVar{
			{Name: "AAP_REPO", Value: t.Repo},
			{Name: "AAP_GIT_BASE", Value: c.gitBase()},
		},
		VolumeMounts: mounts, Resources: limits("500m", "512Mi"), SecurityContext: sc,
	}}
	ctr := corev1.Container{
		Name: "implementer", Image: c.cfg.AgentImage, ImagePullPolicy: c.pullPolicy(),
		Command:    []string{"/sbin/tini", "--", "/opt/aap/task-entrypoint.sh"},
		WorkingDir: TaskRepoDir,
		Env: []corev1.EnvVar{
			{Name: "AAP_AGENT", Value: c.cfg.Agent},
			{Name: "AAP_TASK_ID", Value: t.ID},
			{Name: "AAP_REPO", Value: t.Repo},
			{Name: "AAP_PROMPT", Value: t.Prompt},
			{Name: "AAP_CONTROL_PLANE_URL", Value: c.cfg.InternalURL},
			{Name: "AAP_PROGRESS_TOKEN", Value: t.Token},
			{Name: "AAP_MAX_PATCH_BYTES", Value: fmt.Sprint(c.cfg.TaskMaxPatchBytes)},
		},
		VolumeMounts: mounts, Resources: limits("1", "1536Mi"), SecurityContext: sc,
	}
	if c.cfg.GeminiModel != "" {
		ctr.Env = append(ctr.Env, corev1.EnvVar{Name: "AAP_GEMINI_MODEL", Value: c.cfg.GeminiModel})
	}
	if c.cfg.AgentSecretName != "" {
		ctr.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: c.cfg.AgentSecretName}, Optional: boolp(true)}}}
	}
	spec.Containers = []corev1.Container{ctr}
	_, err := c.cs.BatchV1().Jobs(c.cfg.TasksNamespace).Create(ctx, j, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create job %s: %w", j.Name, err)
	}
	return nil
}

func (c *Client) gitBase() string {
	if c.cfg.TaskGitBase != "" {
		return strings.TrimRight(c.cfg.TaskGitBase, "/")
	}
	return "https://github.com"
}

func resourcePtr(q string) *resource.Quantity { r := resource.MustParse(q); return &r }

// TaskJobStatus reports the Implementer Job's phase; reason carries a failure message.
func (c *Client) TaskJobStatus(ctx context.Context, id string) (JobPhase, string, error) {
	return c.jobStatusIn(ctx, c.cfg.TasksNamespace, TaskJobName(id))
}

// TaskLogTail returns the last lines of the Implementer pod's log (empty if the pod is gone).
func (c *Client) TaskLogTail(ctx context.Context, id string, lines int64) (string, error) {
	sel := fmt.Sprintf("%s=%s", LabelTaskID, id)
	pods, err := c.cs.CoreV1().Pods(c.cfg.TasksNamespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil || len(pods.Items) == 0 {
		return "", err
	}
	if c.LogReader != nil {
		return c.LogReader(ctx, pods.Items[0].Name, lines)
	}
	raw, err := c.cs.CoreV1().Pods(c.cfg.TasksNamespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{
		Container: "implementer", TailLines: &lines}).Do(ctx).Raw()
	return string(raw), err
}

// DeleteTask removes everything the task created in the cluster: its Jobs and its workspace.
func (c *Client) DeleteTask(ctx context.Context, id string) error {
	ns := c.cfg.TasksNamespace
	fg := metav1.DeletePropagationBackground
	do := metav1.DeleteOptions{PropagationPolicy: &fg}
	ignore := func(err error) error {
		if err == nil || apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	sel := fmt.Sprintf("%s=%s,%s=%s", LabelManaged, ManagedByName, LabelTaskID, id)
	if err := ignore(c.cs.BatchV1().Jobs(ns).DeleteCollection(ctx, do, metav1.ListOptions{LabelSelector: sel})); err != nil {
		return err
	}
	return ignore(c.cs.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, TaskWorkspacePVC(id), do))
}
