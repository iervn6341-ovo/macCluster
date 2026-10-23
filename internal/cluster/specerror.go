package cluster

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Reasons a Pod is not run. They appear as the container's waiting reason and in a Warning event, so the
// person who applied the Pod sees why without reading agent logs.
const (
	ReasonUnsupportedPodSpec = "UnsupportedPodSpec" // valid Kubernetes, not implemented on this node
	ReasonInvalidPodSpec     = "InvalidPodSpec"     // misuse of a macCluster label or option
	ReasonNotManaged         = "NotManagedByMacCluster"
)

// SpecError describes everything in a Pod that this node cannot honour, not just the first thing found.
type SpecError struct {
	Reason string
	Fields []string // "field: why", one per problem
}

func (e *SpecError) Error() string {
	if len(e.Fields) == 1 {
		return e.Fields[0]
	}
	return strings.Join(e.Fields, "; ")
}

func invalid(format string, args ...any) error {
	return &SpecError{Reason: ReasonInvalidPodSpec, Fields: []string{fmt.Sprintf(format, args...)}}
}

// Note is something that does not stop the Pod but that the user should know, such as a field that is
// accepted and ignored.
type Note struct {
	Type    string // corev1.EventTypeNormal or corev1.EventTypeWarning
	Reason  string
	Message string
}

const (
	NoteServiceAccountTokenNotMounted = "ServiceAccountTokenNotMounted"
	NoteHostNamespacesIgnored         = "HostNamespacesIgnored"
	NoteLimitsNotEnforced             = "ResourceLimitsNotEnforced"
	NoteEgressNotConfigured           = "ClusterEgressNotConfigured"
)

// isServiceAccountVolume reports the projected token volume that the ServiceAccount admission plugin adds
// to every Pod that does not set automountServiceAccountToken: false.
func isServiceAccountVolume(v corev1.Volume) bool {
	return strings.HasPrefix(v.Name, "kube-api-access-") && v.Projected != nil
}
