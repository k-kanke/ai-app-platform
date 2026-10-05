// Package kube creates and inspects the Kubernetes resources of generated apps.
//
// All names are derived deterministically from the app id / operation id so a
// retry after a Control Plane restart is idempotent (AlreadyExists == success).
package kube

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	Prefix        = "aap-gen-"
	LabelManaged  = "app.kubernetes.io/managed-by"
	ManagedByName = "ai-app-platform"
	LabelAppID    = "aap.dev/app-id"
	LabelOpID     = "aap.dev/op-id"
	LabelRole     = "aap.dev/role"

	// Source PVC layout: only "current" is visible to the Agent.
	// "snapshots" is written by trusted platform Jobs only.
	SubPathCurrent   = "current"
	SubPathSnapshots = "snapshots"

	// Release strategy layout (docs/deep-dive-preview-release.md §3): the Agent edits draft/ only,
	// production reads an immutable releases/<n>/ read-only.
	SubPathDraft    = "draft"
	SubPathReleases = "releases"
)

var appIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,28}[a-z0-9]$`)

// ReleaseDir is the sub path of release n inside the Source PVC.
func ReleaseDir(n int) string { return fmt.Sprintf("%s/%d", SubPathReleases, n) }

// reserved ids would collide with platform hostnames ({id}.<domain>) or look official.
var reserved = map[string]bool{
	"portal": true, "www": true, "api": true, "admin": true, "argocd": true, "grafana": true,
	"control-plane": true, "cloudflared": true, "mail": true, "login": true, "auth": true,
}

// ValidAppID: DNS-label safe, short enough that derived Job names stay <= 63,
// and not a reserved platform hostname.
func ValidAppID(id string) bool {
	// "<id>-preview" is the host of <id>'s preview, so an id may not end with it.
	return appIDRe.MatchString(id) && !reserved[id] && !strings.HasSuffix(id, "-preview")
}

func base(appID string) string      { return Prefix + appID }
func SourcePVC(appID string) string { return base(appID) + "-src" }

// Release-strategy extras: the preview's own data (a clone of production data) and the
// pre-approval data snapshots. Only the platform's helper Jobs touch -dsnap.
func PreviewDataPVC(appID string) string    { return base(appID) + "-pdata" }
func SnapshotPVC(appID string) string       { return base(appID) + "-dsnap" }
func PreviewDeployment(appID string) string { return base(appID) + "-preview" }
func DataPVC(appID string) string           { return base(appID) + "-data" }
func Deployment(appID string) string        { return base(appID) }
func Service(appID string) string           { return base(appID) }

// JobName returns e.g. aap-gen-meal-agent-1a2b3c4d.
func JobName(appID, role, opID string) string {
	short := opID
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("%s-%s-%s", base(appID), role, short)
}

func labels(appID, role, opID string) map[string]string {
	l := map[string]string{LabelManaged: ManagedByName, LabelAppID: appID, LabelRole: role}
	if opID != "" {
		l[LabelOpID] = opID
	}
	return l
}
