package kube

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const RolePublisher = "publisher"

func PublisherJobName(id string) string { return "aap-task-" + id + "-pub" }

// PublishSpec describes one Publisher run. GitHubToken is the only GitHub credential in the system
// that reaches a pod: one repository, one hour, minted just before this Job.
type PublishSpec struct {
	ID, Repo, PatchSHA, GitHubToken, PublisherToken, Title, Body string
}

// EnsurePublisherJob starts the Publisher: fixed code, no LLM, no workspace. It downloads the approved patch
// from the Control Plane, applies it to a fresh clone and opens the pull request. Its pod carries the
// label aap.dev/role=publisher (the only role allowed to reach GitHub with a token).
func (c *Client) EnsurePublisherJob(ctx context.Context, p PublishSpec) error {
	if !ValidRepo(p.Repo) {
		return fmt.Errorf("invalid repository %q", p.Repo)
	}
	lbl := taskLabels(p.ID, RolePublisher)
	sc := restrictedSC(AppUID)
	sc.ReadOnlyRootFilesystem = boolp(true)
	env := []corev1.EnvVar{
		{Name: "AAP_REPO", Value: p.Repo},
		{Name: "AAP_TASK_ID", Value: p.ID},
		{Name: "AAP_PATCH_SHA", Value: p.PatchSHA},
		{Name: "AAP_GITHUB_TOKEN", Value: p.GitHubToken},
		{Name: "AAP_PUBLISHER_TOKEN", Value: p.PublisherToken},
		{Name: "AAP_PR_TITLE", Value: p.Title},
		{Name: "AAP_PR_BODY", Value: p.Body},
		{Name: "AAP_CONTROL_PLANE_URL", Value: c.cfg.InternalURL},
		{Name: "AAP_MAX_PATCH_BYTES", Value: fmt.Sprint(c.cfg.TaskMaxPatchBytes)},
		{Name: "HOME", Value: "/tmp"},
	}
	if c.cfg.TaskGitBase != "" {
		env = append(env, corev1.EnvVar{Name: "AAP_GIT_BASE", Value: c.cfg.TaskGitBase})
	}
	if c.cfg.GitHubAPIBase != "" {
		env = append(env, corev1.EnvVar{Name: "AAP_GITHUB_API", Value: c.cfg.GitHubAPIBase})
	}
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: PublisherJobName(p.ID), Namespace: c.cfg.TasksNamespace, Labels: lbl},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32p(0),
			ActiveDeadlineSeconds:   int64p(600),
			TTLSecondsAfterFinished: int32p(int32(c.cfg.TaskRetention.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: boolp(false),
					SecurityContext:              podSC(),
					Volumes: []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resourcePtr("512Mi")}}}},
					Containers: []corev1.Container{{
						Name: "publisher", Image: c.cfg.PublisherImage, ImagePullPolicy: c.pullPolicy(),
						Env:             env,
						VolumeMounts:    []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
						Resources:       limits("500m", "256Mi"),
						SecurityContext: sc,
					}},
				},
			},
		},
	}
	_, err := c.cs.BatchV1().Jobs(c.cfg.TasksNamespace).Create(ctx, j, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create job %s: %w", j.Name, err)
	}
	return nil
}

// PublisherJobStatus reports the Publisher Job's phase.
func (c *Client) PublisherJobStatus(ctx context.Context, id string) (JobPhase, string, error) {
	return c.jobStatusIn(ctx, c.cfg.TasksNamespace, PublisherJobName(id))
}
