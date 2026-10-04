// Package kube creates and inspects the Kubernetes resources of generated apps.
//
// All names are derived deterministically from the app id / operation id so a
// retry after a Control Plane restart is idempotent (AlreadyExists == success).
package kube

import (
	"fmt"
	"regexp"
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
)

var appIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,28}[a-z0-9]$`)

// ValidAppID: DNS-label safe and short enough that derived Job names stay <= 63.
func ValidAppID(id string) bool { return appIDRe.MatchString(id) }

func base(appID string) string       { return Prefix + appID }
func SourcePVC(appID string) string  { return base(appID) + "-src" }
func DataPVC(appID string) string    { return base(appID) + "-data" }
func Deployment(appID string) string { return base(appID) }
func Service(appID string) string    { return base(appID) }

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
