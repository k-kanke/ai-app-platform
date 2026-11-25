package kube

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Rolling a repository's deployment onto a newly built image (docs/multi-agent-execution-plan.md, deploy step).
// The Control Plane's Role allows exactly this in the target namespace: read pods, read and patch the Deployment.

const restartedAtAnnotation = "aap.dev/restarted-at"

// ReadyDigests are the image digests the Ready pods of a Deployment are running for container, e.g.
// "sha256:...". They come from the pods' imageID, which for a tag that points to an image index is the index digest
// (the value the registry reports for the tag). Pods that are not Ready or are being deleted are ignored.
func (c *Client) ReadyDigests(ctx context.Context, ns, deployment, container string) ([]string, error) {
	d, err := c.cs.AppsV1().Deployments(ns).Get(ctx, deployment, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", ns, deployment, err)
	}
	if d.Spec.Selector == nil {
		return nil, fmt.Errorf("deployment %s/%s has no selector", ns, deployment)
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, err
	}
	pods, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil || !podReady(p) {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name != container {
				continue
			}
			if i := strings.LastIndex(cs.ImageID, "@"); i >= 0 {
				out = append(out, cs.ImageID[i+1:])
			}
		}
	}
	return out, nil
}

func podReady(p corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// RestartDeployment makes the Deployment roll its pods (the same thing `kubectl rollout restart` does) by setting a pod
// template annotation. The value is chosen by the caller (the task id), so repeating the call for the same task changes nothing.
func (c *Client) RestartDeployment(ctx context.Context, ns, deployment, token string) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`, restartedAtAnnotation, token)
	if _, err := c.cs.AppsV1().Deployments(ns).Patch(ctx, deployment, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("restart deployment %s/%s: %w", ns, deployment, err)
	}
	return nil
}

// RolloutComplete reports whether the Deployment has finished rolling: the controller has seen the latest spec, every
// replica is updated and available, and no old replica is left.
func (c *Client) RolloutComplete(ctx context.Context, ns, deployment string) (bool, string, error) {
	d, err := c.cs.AppsV1().Deployments(ns).Get(ctx, deployment, metav1.GetOptions{})
	if err != nil {
		return false, "", fmt.Errorf("get deployment %s/%s: %w", ns, deployment, err)
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	st := d.Status
	switch {
	case st.ObservedGeneration < d.Generation:
		return false, "the controller has not seen the new spec yet", nil
	case st.UpdatedReplicas < want:
		return false, fmt.Sprintf("%d of %d replicas updated", st.UpdatedReplicas, want), nil
	case st.Replicas > st.UpdatedReplicas:
		return false, "old replicas are still terminating", nil
	case st.AvailableReplicas < want:
		return false, fmt.Sprintf("%d of %d replicas available", st.AvailableReplicas, want), nil
	}
	return true, "", nil
}
