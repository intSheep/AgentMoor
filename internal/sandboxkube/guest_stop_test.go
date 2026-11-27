package sandboxkube

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/intSheep/AgentMoor/internal/platform"
)

func TestControllerAPI404WithoutGuestStopRemainsUnknown(t *testing.T) {
	f := newControllerFixture(t)
	before, err := f.c.Start(t.Context(), f.a)
	if err != nil {
		t.Fatal(err)
	}
	caps := sealOutput(t, f)
	f.kube.mu.Lock()
	f.kube.pod = nil
	f.kube.mu.Unlock()
	for range 2 {
		r, err := f.c.Reconcile(t.Context(), f.a)
		if !errors.Is(err, ErrGuestStopUnknown) || !r.CleanupUnknown || r.PodDeleted || r.GuestStopped() || r.State != platform.SandboxFailed || !r.Deadline.Equal(before.Deadline) {
			t.Fatal("API404 manufactured guest stop/deletion proof", err)
		}
		if _, err = f.c.Start(t.Context(), f.a); !errors.Is(err, ErrGuestStopUnknown) {
			t.Fatal("lost fenced execution was restarted", err)
		}
	}
	if f.kube.posts != 1 {
		t.Fatal("404 repeated the create POST")
	}
	for token, purpose := range map[string]platform.SandboxPurpose{caps.Input: platform.SandboxInput, caps.Output: platform.SandboxOutput, caps.Model: platform.SandboxModel} {
		if _, err = f.catalog.AuthenticateSandboxCapability(t.Context(), token, purpose); !errors.Is(err, platform.ErrSandboxDenied) {
			t.Fatal("lost guest retained authority", err)
		}
	}
	if _, body, err := f.catalog.ReadSandboxOutput(t.Context(), f.a); err == nil || body != nil {
		t.Fatal("lost guest output became publishable")
	}
}

func TestControllerPhaseFailedWithoutTerminatedHoldsFinalizer(t *testing.T) {
	f := newControllerFixture(t)
	r, err := f.c.Start(t.Context(), f.a)
	if err != nil {
		t.Fatal(err)
	}
	f.kube.mu.Lock()
	f.kube.keepDeleted = true
	sum := sha256.Sum256([]byte(r.PodUID))
	id := "containerd://" + hex.EncodeToString(sum[:])
	f.kube.pod["status"] = map[string]any{"phase": "Failed", "containerStatuses": []any{map[string]any{"name": "guest", "containerID": id, "restartCount": json.Number("0"), "state": map[string]any{"running": map[string]any{}}}}}
	f.kube.bumpVersion()
	f.kube.mu.Unlock()
	r, err = f.c.Reconcile(t.Context(), f.a)
	if err != nil || !r.PodTerminal || r.GuestStopped() || r.PodDeleted || f.kube.patches != 0 || r.State != platform.SandboxFailed {
		t.Fatal("control-plane Failed phase was used as guest stop-proof", err)
	}
	f.kube.terminal(143)
	f.kube.mu.Lock()
	f.kube.keepDeleted = false
	f.kube.mu.Unlock()
	r, err = f.c.Reconcile(t.Context(), f.a)
	if err != nil || !r.GuestStopped() || !r.PodDeleted || r.State == platform.SandboxReady || r.GuestStopProof.ExitCode == nil || *r.GuestStopProof.ExitCode != 143 {
		t.Fatal("real subsequent termination was not distinguished from failed classification", err)
	}
}

func TestControllerPendingFinalizerCASRejectsBindingRace(t *testing.T) {
	f := newControllerFixture(t)
	f.kube.onCreate = func(pod map[string]any) { delete(pod["spec"].(map[string]any), "nodeName") }
	if _, err := f.c.Start(t.Context(), f.a); err != nil {
		t.Fatal(err)
	}
	f.kube.mu.Lock()
	f.kube.keepDeleted = true
	f.kube.raceBindOnPatch = true
	f.kube.mu.Unlock()
	if _, err := f.c.Cancel(t.Context(), f.a); err == nil {
		t.Fatal("scheduler race did not reject the precise finalizer CAS")
	}
	r, err := f.catalog.SandboxRun(t.Context(), f.a)
	if err != nil || r.GuestStopped() || r.PodDeleted {
		t.Fatal("binding race fabricated never-scheduled proof", err)
	}
	f.kube.mu.Lock()
	finalizers := f.kube.pod["metadata"].(map[string]any)["finalizers"].([]any)
	f.kube.mu.Unlock()
	if len(finalizers) != 1 || finalizers[0] != guestStopFinalizer {
		t.Fatal("failed CAS removed the finalizer")
	}
}

func TestControllerPendingPatchLostACKStaysUnknown(t *testing.T) {
	f := newControllerFixture(t)
	f.kube.onCreate = func(pod map[string]any) { delete(pod["spec"].(map[string]any), "nodeName") }
	if _, err := f.c.Start(t.Context(), f.a); err != nil {
		t.Fatal(err)
	}
	f.kube.mu.Lock()
	f.kube.dropPatchAck = true
	f.kube.mu.Unlock()
	if _, err := f.c.Cancel(t.Context(), f.a); err == nil {
		t.Fatal("actual pending PATCH acknowledgement loss was accepted")
	}
	r, err := f.c.Reconcile(t.Context(), f.a)
	if !errors.Is(err, ErrGuestStopUnknown) || r.GuestStopped() || r.PodDeleted || !r.CleanupUnknown || f.kube.posts != 1 || f.kube.patches != 1 {
		t.Fatal("uncertain pending CAS was retroactively blessed", err)
	}
}

func TestControllerTerminatedPatchLostACKRecoversFromStoredProof(t *testing.T) {
	f := newControllerFixture(t)
	if _, err := f.c.Start(t.Context(), f.a); err != nil {
		t.Fatal(err)
	}
	sealOutput(t, f)
	f.kube.terminal(0)
	f.kube.mu.Lock()
	f.kube.dropPatchAck = true
	f.kube.mu.Unlock()
	if _, err := f.c.Reconcile(t.Context(), f.a); err == nil {
		t.Fatal("actual finalizer PATCH ACK loss was accepted")
	}
	r, err := f.c.Reconcile(t.Context(), f.a)
	if err != nil || !r.GuestStopped() || !r.PodDeleted || r.State != platform.SandboxReady || f.kube.posts != 1 || f.kube.patches != 1 {
		t.Fatal("persisted real termination proof could not recover an uncertain release", err)
	}
}

func TestControllerOnlyRemovesOwnedFinalizer(t *testing.T) {
	f := newControllerFixture(t)
	if _, err := f.c.Start(t.Context(), f.a); err != nil {
		t.Fatal(err)
	}
	sealOutput(t, f)
	f.kube.terminal(0)
	f.kube.mu.Lock()
	f.kube.keepDeleted = true
	meta := f.kube.pod["metadata"].(map[string]any)
	meta["finalizers"] = append(meta["finalizers"].([]any), "foreign.example/hold")
	f.kube.mu.Unlock()
	r, err := f.c.Reconcile(t.Context(), f.a)
	if err != nil || !r.GuestStopped() || r.PodDeleted {
		t.Fatal("foreign finalizer was treated as observed API deletion", err)
	}
	f.kube.mu.Lock()
	finalizers := f.kube.pod["metadata"].(map[string]any)["finalizers"].([]any)
	f.kube.mu.Unlock()
	if len(finalizers) != 1 || !strings.HasPrefix(finalizers[0].(string), "foreign.example/") {
		t.Fatal("another owner's finalizer was removed")
	}
}

func TestControllerContradictoryRuntimeInvalidatesEarlierStopAuthority(t *testing.T) {
	f := newControllerFixture(t)
	if _, err := f.c.Start(t.Context(), f.a); err != nil {
		t.Fatal(err)
	}
	sealOutput(t, f)
	f.kube.terminal(0)
	f.kube.mu.Lock()
	f.kube.keepDeleted = true
	f.kube.mu.Unlock()
	r, err := f.c.Reconcile(t.Context(), f.a)
	if err != nil || !r.GuestStopped() || r.PodDeleted {
		t.Fatal("missing valid prior stop-proof control", err)
	}
	f.kube.mu.Lock()
	status := f.kube.pod["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)
	status["containerID"] = "containerd://" + strings.Repeat("b", 64)
	status["state"].(map[string]any)["terminated"].(map[string]any)["containerID"] = status["containerID"]
	f.kube.bumpVersion()
	f.kube.mu.Unlock()
	r, err = f.c.Reconcile(t.Context(), f.a)
	if !errors.Is(err, ErrGuestStopUnknown) || !r.CleanupUnknown || r.GuestStopped() || r.PodDeleted {
		t.Fatal("changed runtime retained old stop authority", err)
	}
	f.kube.mu.Lock()
	f.kube.pod = nil
	f.kube.mu.Unlock()
	r, err = f.c.Reconcile(t.Context(), f.a)
	if !errors.Is(err, ErrGuestStopUnknown) || r.PodDeleted || r.GuestStopped() {
		t.Fatal("later404 reused contradicted old proof", err)
	}
}
