package kube

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/config"
)

const (
	AppUID  int64 = 1000
	AppPort int32 = 8080
)

// Job roles.
const (
	RoleInit     = "init"
	RoleSnapshot = "snap"
	RoleRestore  = "restore"
	RoleAgent    = "agent"
	RoleWipe     = "wipe"
)

type JobPhase string

const (
	JobMissing   JobPhase = "Missing"
	JobRunning   JobPhase = "Running"
	JobSucceeded JobPhase = "Succeeded"
	JobFailed    JobPhase = "Failed"
)

type Client struct {
	cs  kubernetes.Interface
	cfg config.Config

	// LogReader overrides how pod logs are read (tests). nil = the Kubernetes API.
	LogReader func(ctx context.Context, pod string, tailLines int64) (string, error)
}

func New(cs kubernetes.Interface, cfg config.Config) *Client { return &Client{cs: cs, cfg: cfg} }

// ---- Workspace -------------------------------------------------------------

// EnsureWorkspace creates the Source and Data PVCs. Existing PVCs are kept.
func (c *Client) EnsureWorkspace(ctx context.Context, appID string) error {
	return c.ensurePVCs(ctx, appID, []pvcSpec{
		{SourcePVC(appID), c.cfg.SourceSize, "source"},
		{DataPVC(appID), c.cfg.DataSize, "data"},
	})
}

type pvcSpec struct{ name, size, role string }

func (c *Client) ensurePVCs(ctx context.Context, appID string, list []pvcSpec) error {
	for _, p := range list {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: p.name, Namespace: c.cfg.Namespace, Labels: labels(appID, p.role, "")},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(p.size)},
				},
			},
		}
		if c.cfg.StorageClass != "" {
			sc := c.cfg.StorageClass
			pvc.Spec.StorageClassName = &sc
		}
		_, err := c.cs.CoreV1().PersistentVolumeClaims(c.cfg.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create pvc %s: %w", p.name, err)
		}
	}
	return nil
}

// ---- Jobs ------------------------------------------------------------------

func (c *Client) pullPolicy() corev1.PullPolicy { return corev1.PullPolicy(c.cfg.ImagePullMode) }

func int64p(i int64) *int64 { return &i }
func boolp(b bool) *bool    { return &b }
func int32p(i int32) *int32 { return &i }

func restrictedSC(uid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             boolp(true),
		RunAsUser:                int64p(uid),
		AllowPrivilegeEscalation: boolp(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func podSC() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		FSGroup:        int64p(AppUID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func limits(cpu, mem string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
	}
}

func pvcVolume(name, claim string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}
}

func (c *Client) createJob(ctx context.Context, j *batchv1.Job) error {
	_, err := c.cs.BatchV1().Jobs(c.cfg.Namespace).Create(ctx, j, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create job %s: %w", j.Name, err)
	}
	return nil
}

func (c *Client) baseJob(appID, role, opID string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: JobName(appID, role, opID), Namespace: c.cfg.Namespace, Labels: labels(appID, role, opID)},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32p(0),
			TTLSecondsAfterFinished: int32p(86400),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(appID, role, opID)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: boolp(false),
					SecurityContext:              podSC(),
				},
			},
		},
	}
}

// EnsureHelperJob runs a trusted platform script as root over the Source/Data PVCs.
// Roles: init (create layout + fix ownership), snap (copy current -> snapshots/<op>),
// restore (copy snapshots/<op> -> current). The Agent never runs this.
func (c *Client) EnsureHelperJob(ctx context.Context, appID, role, opID string) error {
	var script string
	switch role {
	case RoleInit:
		script = fmt.Sprintf(`set -e
mkdir -p /src/%[1]s /src/%[2]s
chown %[3]d:%[3]d /src/%[1]s /data
chmod 0755 /src/%[1]s`, SubPathCurrent, SubPathSnapshots, AppUID)
	case RoleSnapshot:
		script = fmt.Sprintf(`set -e
dst=/src/%[1]s/%[2]s
rm -rf "$dst.tmp" && mkdir -p "$dst.tmp"
cp -a /src/%[3]s/. "$dst.tmp"/
rm -rf "$dst" && mv "$dst.tmp" "$dst"
# keep only the 3 most recent snapshots
(cd /src/%[1]s && ls -1t | tail -n +4 | xargs -r rm -rf)`, SubPathSnapshots, opID, SubPathCurrent)
	case RoleRestore:
		script = fmt.Sprintf(`set -e
src=/src/%[1]s/%[2]s
test -d "$src"
find /src/%[3]s -mindepth 1 -delete
cp -a "$src"/. /src/%[3]s/
chown -R %[4]d:%[4]d /src/%[3]s`, SubPathSnapshots, opID, SubPathCurrent, AppUID)
	case RoleWipe:
		// Retry of a failed create: drop whatever a half-finished run left in the source.
		script = fmt.Sprintf(`set -e
find /src/%[1]s -mindepth 1 -delete
chown %[2]d:%[2]d /src/%[1]s`, SubPathCurrent, AppUID)
	default:
		return fmt.Errorf("unknown helper role %q", role)
	}
	j := c.baseJob(appID, role, opID)
	j.Spec.ActiveDeadlineSeconds = int64p(600)
	spec := &j.Spec.Template.Spec
	spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	spec.Volumes = []corev1.Volume{pvcVolume("src", SourcePVC(appID)), pvcVolume("data", DataPVC(appID))}
	spec.Containers = []corev1.Container{{
		Name: "helper", Image: c.cfg.HelperImage, ImagePullPolicy: c.pullPolicy(),
		Command: []string{"sh", "-c", script},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "src", MountPath: "/src"}, {Name: "data", MountPath: "/data"}},
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

// AgentSpec describes one Agent run.
type AgentSpec struct {
	AppID, OpID, Kind, Prompt, Token string
	SubPath                          string // "" = SubPathCurrent (in-place strategy); SubPathDraft for releases
}

// EnsureAgentJob starts the ephemeral Agent. It mounts ONLY the Source workspace
// ("current" sub path); it never sees the Data PVC, snapshots, or cluster credentials.
func (c *Client) EnsureAgentJob(ctx context.Context, a AgentSpec) error {
	j := c.baseJob(a.AppID, RoleAgent, a.OpID)
	sub := a.SubPath
	if sub == "" {
		sub = SubPathCurrent
	}
	j.Spec.ActiveDeadlineSeconds = int64p(int64(c.cfg.AgentTimeout.Seconds()))
	spec := &j.Spec.Template.Spec
	spec.Volumes = []corev1.Volume{pvcVolume("workspace", SourcePVC(a.AppID))}
	ctr := corev1.Container{
		Name: "agent", Image: c.cfg.AgentImage, ImagePullPolicy: c.pullPolicy(),
		WorkingDir: "/workspace",
		Env: []corev1.EnvVar{
			{Name: "AAP_AGENT", Value: c.cfg.Agent},
			{Name: "AAP_APP_ID", Value: a.AppID},
			{Name: "AAP_OP_ID", Value: a.OpID},
			{Name: "AAP_OP_KIND", Value: a.Kind},
			{Name: "AAP_PROMPT", Value: a.Prompt},
			{Name: "AAP_CONTROL_PLANE_URL", Value: c.cfg.InternalURL},
			{Name: "AAP_PROGRESS_TOKEN", Value: a.Token},
		},
		VolumeMounts:    []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace", SubPath: sub}},
		Resources:       limits("1", "1Gi"),
		SecurityContext: restrictedSC(AppUID),
	}
	if c.cfg.GeminiModel != "" {
		ctr.Env = append(ctr.Env, corev1.EnvVar{Name: "AAP_GEMINI_MODEL", Value: c.cfg.GeminiModel})
	}
	if c.cfg.AgentSecretName != "" {
		ctr.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: c.cfg.AgentSecretName}, Optional: boolp(true)}}}
	}
	spec.Containers = []corev1.Container{ctr}
	return c.createJob(ctx, j)
}

// AgentLogTail returns the last lines of the Agent Job's pod log (for diagnosing failures).
func (c *Client) AgentLogTail(ctx context.Context, appID, opID string, lines int64) (string, error) {
	return c.JobLogTail(ctx, appID, RoleAgent, opID, lines)
}

// JobLogTail returns the last lines of the pod log of the Job with the given role and operation.
func (c *Client) JobLogTail(ctx context.Context, appID, role, opID string, lines int64) (string, error) {
	sel := fmt.Sprintf("%s=%s,%s=%s,%s=%s", LabelAppID, appID, LabelOpID, opID, LabelRole, role)
	pods, err := c.cs.CoreV1().Pods(c.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil || len(pods.Items) == 0 {
		return "", err
	}
	if c.LogReader != nil {
		return c.LogReader(ctx, pods.Items[0].Name, lines)
	}
	raw, err := c.cs.CoreV1().Pods(c.cfg.Namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{TailLines: &lines}).Do(ctx).Raw()
	return string(raw), err
}

// JobStatus reports a Job's phase; reason carries a failure message.
func (c *Client) JobStatus(ctx context.Context, name string) (JobPhase, string, error) {
	j, err := c.cs.BatchV1().Jobs(c.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return JobMissing, "", nil
	}
	if err != nil {
		return "", "", err
	}
	if j.Status.Succeeded > 0 {
		return JobSucceeded, "", nil
	}
	for _, cond := range j.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return JobFailed, strings.TrimSpace(cond.Reason + ": " + cond.Message), nil
		}
	}
	if j.Status.Failed > 0 {
		return JobFailed, "job pod failed", nil
	}
	return JobRunning, "", nil
}

// ---- Runtime ---------------------------------------------------------------

// EnsureRuntime creates/updates the long-running App Runtime (in-place strategy: reads current/).
// restartToken changes the pod template so a new rollout picks up modified source.
func (c *Client) EnsureRuntime(ctx context.Context, appID, restartToken string) error {
	return c.EnsureRuntimeAt(ctx, appID, restartToken, SubPathCurrent)
}

// EnsureRuntimeAt runs the production runtime from srcSubPath of the Source PVC (read-only).
// The release strategy passes ReleaseDir(n), so production only ever reads an immutable release.
func (c *Client) EnsureRuntimeAt(ctx context.Context, appID, restartToken, srcSubPath string) error {
	d := c.appDeployment(deployParams{
		Name: Deployment(appID), AppID: appID, Role: "runtime", SrcSubPath: srcSubPath,
		DataClaim: DataPVC(appID), Token: restartToken,
	})
	if err := c.applyDeployment(ctx, d); err != nil {
		return err
	}
	if err := c.ensureService(ctx, Service(appID), appID, "runtime"); err != nil {
		return err
	}
	_, err := c.EnsureIngress(ctx, appID)
	return err
}

type deployParams struct {
	Name, AppID, Role, SrcSubPath, DataClaim, Token string
}

func (c *Client) appDeployment(p deployParams) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: c.cfg.Namespace, Labels: labels(p.AppID, p.Role, "")},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32p(1),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}, // RWO + single writer to /data
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelAppID: p.AppID, LabelRole: p.Role}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels(p.AppID, p.Role, ""),
					Annotations: map[string]string{"aap.dev/restart-token": p.Token, "aap.dev/source": p.SrcSubPath},
				},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: boolp(false),
					SecurityContext:              podSC(),
					Volumes: []corev1.Volume{pvcVolume("src", SourcePVC(p.AppID)), pvcVolume("data", p.DataClaim),
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
					Containers: []corev1.Container{{
						Name: "app", Image: c.cfg.RuntimeImage, ImagePullPolicy: c.pullPolicy(),
						Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: AppPort}},
						Env: []corev1.EnvVar{
							{Name: "PORT", Value: fmt.Sprint(AppPort)},
							{Name: "DATA_DIR", Value: "/data"},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "src", MountPath: "/app", SubPath: p.SrcSubPath, ReadOnly: true},
							{Name: "data", MountPath: "/data"},
							{Name: "tmp", MountPath: "/tmp"},
						},
						Resources:       limits("500m", "512Mi"),
						SecurityContext: withReadOnlyRoot(restrictedSC(AppUID)),
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("http")}},
							PeriodSeconds: 5, FailureThreshold: 3,
						},
						StartupProbe: &corev1.Probe{
							ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("http")}},
							PeriodSeconds: 2, FailureThreshold: 60,
						},
					}},
				},
			},
		},
	}
}

func (c *Client) applyDeployment(ctx context.Context, dep *appsv1.Deployment) error {
	deps := c.cs.AppsV1().Deployments(c.cfg.Namespace)
	if _, err := deps.Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create deployment: %w", err)
		}
		cur, gerr := deps.Get(ctx, dep.Name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		cur.Spec.Template = dep.Spec.Template
		cur.Spec.Replicas = dep.Spec.Replicas // a runtime stopped for a snapshot is started again here
		if _, uerr := deps.Update(ctx, cur, metav1.UpdateOptions{}); uerr != nil {
			return fmt.Errorf("update deployment: %w", uerr)
		}
	}
	return nil
}

func (c *Client) ensureService(ctx context.Context, name, appID, role string) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.cfg.Namespace, Labels: labels(appID, role, "")},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{LabelAppID: appID, LabelRole: role},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromString("http")}},
		},
	}
	if _, err := c.cs.CoreV1().Services(c.cfg.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create service: %w", err)
	}
	return nil
}

// IngressEnabled reports whether apps get a public Ingress (class + host pattern set).
func (c *Client) IngressEnabled() bool {
	return c.cfg.IngressClass != "" && c.cfg.IngressHostPattern != ""
}

// EnsureIngress creates the app's Ingress if it does not exist yet. It never
// modifies an existing one. created reports whether a new Ingress was made.
func (c *Client) EnsureIngress(ctx context.Context, appID string) (created bool, err error) {
	return c.ensureIngress(ctx, base(appID), appID, "runtime", strings.ReplaceAll(c.cfg.IngressHostPattern, "{id}", appID), Service(appID))
}

func (c *Client) ensureIngress(ctx context.Context, name, appID, role, host, svc string) (bool, error) {
	if !c.IngressEnabled() {
		return false, nil
	}
	ns := c.cfg.Namespace
	class := c.cfg.IngressClass
	pt := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels(appID, role, "")},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &class,
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &pt, Backend: networkingv1.IngressBackend{
						Service: &networkingv1.IngressServiceBackend{Name: svc, Port: networkingv1.ServiceBackendPort{Name: "http"}}}}},
				}},
			}},
		},
	}
	_, err := c.cs.NetworkingV1().Ingresses(ns).Create(ctx, ing, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("create ingress: %w", err)
	}
	return true, nil
}

func withReadOnlyRoot(sc *corev1.SecurityContext) *corev1.SecurityContext {
	sc.ReadOnlyRootFilesystem = boolp(true)
	return sc
}

// RuntimeReady reports whether the current rollout of the App Runtime is fully available.
func (c *Client) RuntimeReady(ctx context.Context, appID string) (bool, string, error) {
	return c.deploymentReady(ctx, Deployment(appID))
}

func (c *Client) deploymentReady(ctx context.Context, name string) (bool, string, error) {
	d, err := c.cs.AppsV1().Deployments(c.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, "runtime missing", nil
	}
	if err != nil {
		return false, "", err
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	if d.Status.ObservedGeneration < d.Generation {
		return false, "rollout pending", nil
	}
	if d.Status.UpdatedReplicas == want && d.Status.ReadyReplicas == want && d.Status.AvailableReplicas == want && d.Status.Replicas == want {
		return true, "", nil
	}
	return false, fmt.Sprintf("ready %d/%d", d.Status.ReadyReplicas, want), nil
}

// Actual is the runtime state observed from Kubernetes (never cached in SQLite).
type Actual struct {
	SourceBound  bool   `json:"sourceBound"`
	DataBound    bool   `json:"dataBound"`
	RuntimeExist bool   `json:"runtimeExists"`
	RuntimeReady bool   `json:"runtimeReady"`
	Detail       string `json:"detail,omitempty"`
}

func (c *Client) Actual(ctx context.Context, appID string) (Actual, error) {
	var a Actual
	pvcs := c.cs.CoreV1().PersistentVolumeClaims(c.cfg.Namespace)
	for name, dst := range map[string]*bool{SourcePVC(appID): &a.SourceBound, DataPVC(appID): &a.DataBound} {
		p, err := pvcs.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			// WaitForFirstConsumer PVCs stay Pending until used; treat as "exists".
			*dst = p.Status.Phase == corev1.ClaimBound || p.Status.Phase == corev1.ClaimPending
		} else if !apierrors.IsNotFound(err) {
			return a, err
		}
	}
	ready, detail, err := c.RuntimeReady(ctx, appID)
	if err != nil {
		return a, err
	}
	a.RuntimeReady, a.Detail = ready, detail
	a.RuntimeExist = detail != "runtime missing"
	return a, nil
}

// ---- Delete ----------------------------------------------------------------

// DeleteApp removes the runtime and jobs. PVCs (source + user data) are kept
// unless purge is true, so an accidental delete never destroys family data.
func (c *Client) DeleteApp(ctx context.Context, appID string, purge bool) error {
	ns := c.cfg.Namespace
	sel := fmt.Sprintf("%s=%s,%s=%s", LabelManaged, ManagedByName, LabelAppID, appID)
	fg := metav1.DeletePropagationBackground
	do := metav1.DeleteOptions{PropagationPolicy: &fg}
	ignore := func(err error) error {
		if err == nil || apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := ignore(c.cs.NetworkingV1().Ingresses(ns).Delete(ctx, base(appID), do)); err != nil {
		return err
	}
	if err := ignore(c.cs.CoreV1().Services(ns).Delete(ctx, Service(appID), do)); err != nil {
		return err
	}
	if err := ignore(c.cs.AppsV1().Deployments(ns).Delete(ctx, Deployment(appID), do)); err != nil {
		return err
	}
	if err := c.DeletePreview(ctx, appID); err != nil {
		return err
	}
	if err := ignore(c.cs.BatchV1().Jobs(ns).DeleteCollection(ctx, do, metav1.ListOptions{LabelSelector: sel})); err != nil {
		return err
	}
	if purge {
		for _, n := range []string{SourcePVC(appID), DataPVC(appID), PreviewDataPVC(appID), SnapshotPVC(appID)} {
			if err := ignore(c.cs.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, n, do)); err != nil {
				return err
			}
		}
	}
	return nil
}
