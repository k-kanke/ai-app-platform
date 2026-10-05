package kube

// Release strategy (docs/deep-dive-preview-release.md):
//
//	Source PVC   draft/           the Agent edits this (read-write); the Preview runs it (read-only)
//	             releases/<n>/    immutable copies made on approval; production runs one (read-only)
//	Data PVC     production data  only production writes it
//	pdata PVC    preview data     a copy of production data; thrown away
//	dsnap PVC    data snapshots   taken right before an approval, used to roll data back
//
// All Jobs below are trusted platform code (busybox), never the Agent.

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Release-strategy Job roles.
const (
	RoleRInit     = "rinit"    // lay out draft/ and releases/, fix ownership
	RoleDataCopy  = "dcopy"    // production data -> preview data
	RoleRelease   = "release"  // draft/ -> releases/<n>/ (immutable) + content hash
	RoleDSnap     = "dsnap"    // production data -> data snapshot <n>
	RoleDRestore  = "drestore" // data snapshot <n> -> production data
	RoleReset     = "reset"    // releases/<n>/ -> draft/ (discard a draft)
	RoleWipeDraft = "rwipe"    // empty draft/ (retry of a failed create)
)

// EnsureReleaseWorkspace creates all four PVCs of a release-strategy app. Existing ones are kept.
func (c *Client) EnsureReleaseWorkspace(ctx context.Context, appID string) error {
	return c.ensurePVCs(ctx, appID, []pvcSpec{
		{SourcePVC(appID), c.cfg.SourceSize, "source"},
		{DataPVC(appID), c.cfg.DataSize, "data"},
		{PreviewDataPVC(appID), c.cfg.DataSize, "pdata"},
		{SnapshotPVC(appID), c.cfg.DataSize, "dsnap"},
	})
}

// releaseScript returns the shell script of a release-strategy helper role.
// n is the release number the role works on (ignored by roles that do not need it).
func releaseScript(role string, n int) (string, error) {
	switch role {
	case RoleRInit:
		return fmt.Sprintf(`set -e
mkdir -p /src/%[1]s /src/%[2]s
chown %[3]d:%[3]d /src/%[1]s /data /pdata
chmod 0755 /src/%[1]s /src/%[2]s`, SubPathDraft, SubPathReleases, AppUID), nil
	case RoleWipeDraft:
		return fmt.Sprintf(`set -e
find /src/%[1]s -mindepth 1 -delete
chown %[2]d:%[2]d /src/%[1]s`, SubPathDraft, AppUID), nil
	case RoleDataCopy:
		// busybox cp has no --reflink; on a CoW filesystem this is where it would be a clone.
		return fmt.Sprintf(`set -e
find /pdata -mindepth 1 -delete
cp -a /data/. /pdata/
chown -R %[1]d:%[1]d /pdata`, AppUID), nil
	case RoleRelease:
		// Copy to .tmp, hash the content, rename into place, THEN make it read-only. (Renaming a read-only
		// directory is refused on some filesystems.) Nobody reads release n before this Job succeeds, so a
		// half-made release is never served. Re-running with the same draft yields the same release.
		return fmt.Sprintf(`set -e
n=%[1]d; d=/src/%[2]s/$n
test -d /src/%[3]s
rm -rf "$d.tmp"; mkdir -p "$d.tmp"
cp -a /src/%[3]s/. "$d.tmp"/
h=$(cd "$d.tmp" && find . -type f | sort | xargs -r sha256sum | sha256sum | cut -d' ' -f1)
echo "$h" > "/src/%[2]s/$n.sha256"
if [ -e "$d" ]; then chmod -R u+w "$d"; rm -rf "$d"; fi
mv "$d.tmp" "$d"
chmod -R a-w "$d"
echo "AAP_HASH $h"`, n, SubPathReleases, SubPathDraft), nil
	case RoleDSnap:
		return fmt.Sprintf(`set -e
n=%d
rm -rf /dsnap/$n.tmp; mkdir -p /dsnap/$n.tmp
cp -a /data/. /dsnap/$n.tmp/
rm -rf /dsnap/$n; mv /dsnap/$n.tmp /dsnap/$n
# keep the 3 most recent snapshots
(cd /dsnap && ls -1t | grep -v '\.tmp$' | tail -n +4 | xargs -r rm -rf)
echo "AAP_DSNAP $n"`, n), nil
	case RoleDRestore:
		return fmt.Sprintf(`set -e
n=%[1]d
test -d /dsnap/$n
find /data -mindepth 1 -delete
cp -a /dsnap/$n/. /data/
chown -R %[2]d:%[2]d /data`, n, AppUID), nil
	case RoleReset:
		return fmt.Sprintf(`set -e
n=%[1]d
test -d /src/%[2]s/$n
find /src/%[3]s -mindepth 1 -delete
cp -a /src/%[2]s/$n/. /src/%[3]s/
chmod -R u+w /src/%[3]s
chown -R %[4]d:%[4]d /src/%[3]s`, n, SubPathReleases, SubPathDraft, AppUID), nil
	}
	return "", fmt.Errorf("unknown release helper role %q", role)
}

// EnsureReleaseHelperJob runs one release-strategy helper script over all four PVCs.
// The Job name is derived from (app, role, op) so a retry reuses the same Job.
func (c *Client) EnsureReleaseHelperJob(ctx context.Context, appID, role, opID string, n int) error {
	script, err := releaseScript(role, n)
	if err != nil {
		return err
	}
	j := c.baseJob(appID, role, opID)
	j.Spec.ActiveDeadlineSeconds = int64p(600)
	spec := &j.Spec.Template.Spec
	spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	spec.Volumes = []corev1.Volume{
		pvcVolume("src", SourcePVC(appID)), pvcVolume("data", DataPVC(appID)),
		pvcVolume("pdata", PreviewDataPVC(appID)), pvcVolume("dsnap", SnapshotPVC(appID)),
	}
	spec.Containers = []corev1.Container{{
		Name: "helper", Image: c.cfg.HelperImage, ImagePullPolicy: c.pullPolicy(),
		Command: []string{"sh", "-c", script},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "src", MountPath: "/src"}, {Name: "data", MountPath: "/data"},
			{Name: "pdata", MountPath: "/pdata"}, {Name: "dsnap", MountPath: "/dsnap"}},
		Resources: limits("500m", "128Mi"),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolp(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"CHOWN", "FOWNER", "DAC_OVERRIDE"},
			},
		},
	}}
	return c.createJob(ctx, j)
}

// EnsurePreview runs the draft read-only against the preview's own data copy, in its own
// Deployment / Service / Ingress ("<id>-preview"). It never touches production.
func (c *Client) EnsurePreview(ctx context.Context, appID, restartToken string) error {
	d := c.appDeployment(deployParams{
		Name: PreviewDeployment(appID), AppID: appID, Role: "preview", SrcSubPath: SubPathDraft,
		DataClaim: PreviewDataPVC(appID), Token: restartToken,
	})
	if err := c.applyDeployment(ctx, d); err != nil {
		return err
	}
	if err := c.ensureService(ctx, PreviewDeployment(appID), appID, "preview"); err != nil {
		return err
	}
	_, err := c.ensureIngress(ctx, PreviewDeployment(appID), appID, "preview",
		strings.ReplaceAll(c.cfg.IngressHostPattern, "{id}", appID+"-preview"), PreviewDeployment(appID))
	return err
}

// PreviewReady reports whether the preview Deployment is fully available.
func (c *Client) PreviewReady(ctx context.Context, appID string) (bool, string, error) {
	return c.deploymentReady(ctx, PreviewDeployment(appID))
}

// DeletePreview removes the preview's Deployment, Service and Ingress (its data PVC is kept).
func (c *Client) DeletePreview(ctx context.Context, appID string) error {
	ns := c.cfg.Namespace
	fg := metav1.DeletePropagationBackground
	do := metav1.DeleteOptions{PropagationPolicy: &fg}
	ignore := func(err error) error {
		if err == nil || apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := ignore(c.cs.NetworkingV1().Ingresses(ns).Delete(ctx, PreviewDeployment(appID), do)); err != nil {
		return err
	}
	if err := ignore(c.cs.CoreV1().Services(ns).Delete(ctx, PreviewDeployment(appID), do)); err != nil {
		return err
	}
	return ignore(c.cs.AppsV1().Deployments(ns).Delete(ctx, PreviewDeployment(appID), do))
}

// ScaleRuntime sets the production runtime's replica count (0 = stop it, e.g. to take a quiescent data snapshot).
func (c *Client) ScaleRuntime(ctx context.Context, appID string, replicas int32) error {
	deps := c.cs.AppsV1().Deployments(c.cfg.Namespace)
	d, err := deps.Get(ctx, Deployment(appID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil // nothing running yet
	}
	if err != nil {
		return err
	}
	d.Spec.Replicas = &replicas
	_, err = deps.Update(ctx, d, metav1.UpdateOptions{})
	return err
}

// RuntimeStopped reports whether the production runtime has no pods left (or does not exist).
func (c *Client) RuntimeStopped(ctx context.Context, appID string) (bool, error) {
	d, err := c.cs.AppsV1().Deployments(c.cfg.Namespace).Get(ctx, Deployment(appID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return d.Status.Replicas == 0 && d.Status.ReadyReplicas == 0, nil
}
