package sandboxkube

import (
	"encoding/json"
	"strings"

	"agentmoor/internal/platform"
)

func podResourceVersion(v string) bool {
	if len(v) == 0 || len(v) > 256 {
		return false
	}
	for _, c := range v {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
func guestContainerID(id string) bool {
	return strings.HasPrefix(id, "containerd://") && len(id) == len("containerd://")+64 && strings.Trim(strings.TrimPrefix(id, "containerd://"), "0123456789abcdef") == ""
}
func inspectFinalizers(r platform.SandboxRun, metadata map[string]any, p *observedPod) error {
	values, ok := metadata["finalizers"].([]any)
	if !ok && metadata["finalizers"] != nil {
		return ErrPod
	}
	if len(values) > 32 {
		return ErrPod
	}
	seen := map[string]bool{}
	for _, value := range values {
		name, ok := value.(string)
		if !ok || name == "" || len(name) > 253 || seen[name] {
			return ErrPod
		}
		seen[name] = true
		p.finalizers = append(p.finalizers, name)
		if name == guestStopFinalizer {
			p.ownFinalizer = true
		}
	}
	if !p.ownFinalizer && !r.GuestStopped() {
		return ErrPod
	}
	// Foreign finalizers cannot change the execution template and are preserved
	// exactly by the conditional removal of only our own lifecycle finalizer.
	metadata["finalizers"] = []any{guestStopFinalizer}
	return nil
}

func inspectGuestStatus(full map[string]any, p *observedPod) error {
	spec, ok := full["spec"].(map[string]any)
	if !ok {
		return ErrPod
	}
	p.casSpec = spec
	p.nodeName, _ = spec["nodeName"].(string)
	if p.nodeName != "" && !dnsName(p.nodeName, 253) {
		return ErrPod
	}
	status, _ := full["status"].(map[string]any)
	phase, _ := status["phase"].(string)
	p.failed = phase == "Failed"
	if phase == "Succeeded" || p.failed {
		p.terminal, p.exit = true, 255
	}
	values, present := status["containerStatuses"]
	statuses, ok := values.([]any)
	if !ok && present && values != nil {
		return ErrPod
	}
	p.neverScheduled = p.deleting && p.nodeName == "" && len(statuses) == 0 && podResourceVersion(p.resourceVersion)
	if len(statuses) == 0 {
		if phase == "Succeeded" {
			return ErrPod
		}
		return nil // result-failure classification is not a guest stop-proof
	}
	if len(statuses) != 1 {
		return ErrPod
	}
	guest, ok := statuses[0].(map[string]any)
	if !ok || guest["name"] != "guest" || guest["restartCount"] != json.Number("0") {
		return ErrPod
	}
	p.containerID, _ = guest["containerID"].(string)
	if p.containerID != "" && !guestContainerID(p.containerID) {
		return ErrPod
	}
	state, ok := guest["state"].(map[string]any)
	if !ok || len(state) != 1 {
		return ErrPod
	}
	terminated, stopped := state["terminated"].(map[string]any)
	if !stopped {
		if phase == "Succeeded" {
			return ErrPod
		}
		return nil // Running guest can coexist with control-plane PhaseFailed.
	}
	exit, ok := number(terminated["exitCode"])
	if !ok || exit < 0 || exit > 255 || p.nodeName == "" || !guestContainerID(p.containerID) || !podResourceVersion(p.resourceVersion) {
		return ErrPod
	}
	if id, ok := terminated["containerID"]; ok && id != p.containerID {
		return ErrPod
	}
	if phase == "Succeeded" && exit != 0 {
		return ErrPod
	}
	code := int(exit)
	p.terminal, p.exit = true, code
	if p.failed && code == 0 {
		p.exit = 255
	} // infrastructure failure is not successful output
	p.stopProof = &platform.SandboxGuestStopProof{Kind: platform.SandboxGuestTerminated, PodUID: p.uid, ResourceVersion: p.resourceVersion,
		NodeName: p.nodeName, ContainerID: p.containerID, ContainerName: "guest", RestartCount: 0, ExitCode: &code}
	return nil
}
