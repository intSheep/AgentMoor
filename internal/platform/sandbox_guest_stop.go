package platform

import (
	"context"
	"reflect"
	"strings"
)

const (
	SandboxGuestTerminated     = "terminated.v1"
	SandboxGuestNeverScheduled = "never-scheduled-cas.v1"
)

// SandboxGuestStopProof records a trusted Kubernetes observer's evidence that
// the one guest stopped (or could never start). It does not attest that every
// Sentry/gofer/shim process or host resource has been released. Nil is valid
// legacy metadata, but cannot authorize new output publication or collection.
type SandboxGuestStopProof struct {
	Kind            string `json:"kind"`
	PodUID          string `json:"pod_uid"`
	ResourceVersion string `json:"resource_version"`
	NodeName        string `json:"node_name,omitempty"`
	ContainerID     string `json:"container_id,omitempty"`
	ContainerName   string `json:"container_name,omitempty"`
	RestartCount    int    `json:"restart_count"`
	ExitCode        *int   `json:"exit_code,omitempty"`
}

func sandboxNodeName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
func sandboxContainerID(id string) bool {
	return strings.HasPrefix(id, "containerd://") && sandboxDigest(strings.TrimPrefix(id, "containerd://"))
}
func sandboxResourceVersion(version string) bool {
	if len(version) == 0 || len(version) > 256 {
		return false
	}
	for _, c := range version {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
func (p SandboxGuestStopProof) valid() bool {
	if !sandboxUUID(p.PodUID) || !sandboxResourceVersion(p.ResourceVersion) || p.RestartCount != 0 {
		return false
	}
	switch p.Kind {
	case SandboxGuestTerminated:
		return sandboxNodeName(p.NodeName) && sandboxContainerID(p.ContainerID) && p.ContainerName == "guest" && p.ExitCode != nil && *p.ExitCode >= 0 && *p.ExitCode <= 255
	case SandboxGuestNeverScheduled:
		return p.NodeName == "" && p.ContainerID == "" && p.ContainerName == "" && p.ExitCode == nil
	}
	return false
}

// GuestStopped is independent of the old result-terminal classification and
// API deletion flag. A Failed phase without container status is not a proof.
func (r SandboxRun) GuestStopped() bool {
	return !r.CleanupUnknown && r.guestStopProofMatchesBinding()
}

// Keep a contradictory observation readable for operators without allowing a
// previously stored proof to authorize new work or collection after it.
func (r SandboxRun) guestStopProofMatchesBinding() bool {
	p := r.GuestStopProof
	return p != nil && p.valid() && r.PodUID == p.PodUID && r.PodNodeName == p.NodeName && r.GuestContainerID == p.ContainerID
}

func (r SandboxRun) validGuestBinding() bool {
	return (r.PodNodeName == "" || sandboxNodeName(r.PodNodeName)) &&
		(r.GuestContainerID == "" || (r.PodNodeName != "" && sandboxContainerID(r.GuestContainerID))) &&
		(r.GuestStopProof == nil || r.guestStopProofMatchesBinding())
}

// BindSandboxGuestRuntime is operator-only: values must come from the original
// UID's verified Pod on the trusted API. It never adopts another node/container.
func (c *Catalog) BindSandboxGuestRuntime(ctx context.Context, a SandboxAddress, uid, node, container string) (SandboxRun, error) {
	if !sandboxUUID(uid) || !sandboxNodeName(node) || (container != "" && !sandboxContainerID(container)) {
		return SandboxRun{}, errBadRequest
	}
	return c.changeSandbox(ctx, a, func(r *SandboxRun) (string, error) {
		if r.GuestStopProof != nil && (r.PodNodeName != node || (container != "" && r.GuestContainerID != container)) {
			return "", ErrConflict
		}
		if r.Collected || r.PodUID != uid || (r.PodNodeName != "" && r.PodNodeName != node) || (r.GuestContainerID != "" && container != "" && r.GuestContainerID != container) {
			return "", ErrConflict
		}
		r.PodNodeName = node
		if container != "" {
			r.GuestContainerID = container
		}
		return "", nil
	})
}

// RecordSandboxGuestStopped is an operator-only attestation, never a guest or
// public HTTP route. For never-scheduled-cas, the caller must first complete a
// UID/resourceVersion/spec/finalizers conditional PATCH on the deleting Pod.
// RecordSandboxTerminal alone deliberately cannot manufacture this evidence.
func (c *Catalog) RecordSandboxGuestStopped(ctx context.Context, a SandboxAddress, proof SandboxGuestStopProof) (SandboxRun, error) {
	if !proof.valid() {
		return SandboxRun{}, errBadRequest
	}
	return c.changeSandbox(ctx, a, func(r *SandboxRun) (string, error) {
		if r.PodUID != proof.PodUID || !r.CreateStarted {
			return "", ErrConflict
		}
		if r.GuestStopProof != nil {
			old, next := *r.GuestStopProof, proof
			next.ResourceVersion = old.ResourceVersion // retain the first authoritative observation
			if !r.GuestStopped() || !reflect.DeepEqual(old, next) {
				return "", ErrConflict
			}
			return "", nil
		}
		// Do not retroactively bless a legacy already-deleted/collected row.
		if r.PodDeleted || r.Collected || (r.PodNodeName != "" && r.PodNodeName != proof.NodeName) || (r.GuestContainerID != "" && r.GuestContainerID != proof.ContainerID) {
			return "", ErrConflict
		}
		r.PodNodeName, r.GuestContainerID = proof.NodeName, proof.ContainerID
		copy := proof
		if proof.ExitCode != nil {
			code := *proof.ExitCode
			copy.ExitCode = &code
		}
		r.GuestStopProof = &copy
		r.CleanupUnknown = false
		return "all", nil
	})
}

// MarkSandboxCleanupUnknown revokes authority while retaining the original
// fence, UUID, deadline and data. API404/NodeReady are not guest stop evidence.
func (c *Catalog) MarkSandboxCleanupUnknown(ctx context.Context, a SandboxAddress) (SandboxRun, error) {
	return c.changeSandbox(ctx, a, func(r *SandboxRun) (string, error) {
		if r.Collected {
			return "", ErrConflict
		}
		r.CleanupUnknown = true
		if !sandboxStopped(*r) {
			r.State = SandboxFailed
		}
		r.ErrorCode = "guest-stop-unconfirmed"
		return "all", nil
	})
}
