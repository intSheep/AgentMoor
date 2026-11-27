package platform

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"reflect"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/nativeguest"
	"github.com/intSheep/AgentMoor/internal/task"
)

// This key and selector are provided only by the authenticated platform route.
// No Finish field, header, guest metadata or asset path selects a provider Run.
type sandboxFinishProfileContext struct{}
type sandboxFinishSelector func(asset.Scope) (SandboxExecutionProfile, bool)

func sandboxFinishContext(ctx context.Context, selector func(asset.Scope) (SandboxExecutionProfile, bool)) context.Context {
	return context.WithValue(ctx, sandboxFinishProfileContext{}, sandboxFinishSelector(selector))
}

type sandboxSuccessEvidence struct {
	run     SandboxRun
	profile SandboxExecutionProfile
}

func currentSandboxFinishProfile(ctx context.Context, scope asset.Scope) (SandboxExecutionProfile, error) {
	selector, ok := ctx.Value(sandboxFinishProfileContext{}).(sandboxFinishSelector)
	if !ok || selector == nil {
		return SandboxExecutionProfile{}, ErrConflict
	}
	p, ok := selector(scope)
	if !ok || !sandboxDigest(p.Fingerprint) || p.AdapterID != scope.Adapter || p.AdapterVersion == "" || p.RuntimeVersion == "" {
		return SandboxExecutionProfile{}, ErrConflict
	}
	return p, nil
}

func checkSandboxSuccessRun(a taskAddress, slot taskSlot, r SandboxRun, p SandboxExecutionProfile) error {
	address := SandboxAddress{Scope: a.Scope, TurnID: a.turn, Attempt: slot.attempt}
	if !address.valid() || slot.ID != a.Runtime || slot.TurnID != a.turn || slot.State != task.Running || slot.ExecutionPolicy != task.ExecutionUntrustedIsolated ||
		r.Address() != address || r.ID != sandboxIdentity(address) || r.Source.State != task.Running || r.Source.ExecutionPolicy != task.ExecutionUntrustedIsolated ||
		r.Source.ID != slot.ID || r.Source.TurnID != slot.TurnID || !r.Deadline.Equal(slot.Deadline) || !time.Now().Before(slot.Deadline) ||
		r.State != SandboxReady || r.Collected || r.ErrorCode != "" || r.ProfileFingerprint != p.Fingerprint ||
		!r.Input.valid() || !r.Input.Sealed || r.Input.Size < 1 || !r.Output.valid() || !r.Output.Sealed || r.Output.Size < 1 ||
		!r.CreateStarted || !r.PodTerminal || !r.PodDeleted || r.ExitCode != 0 || !sandboxUUID(r.PodUID) ||
		!r.GuestStopped() || r.GuestStopProof.Kind != SandboxGuestTerminated || r.GuestStopProof.ExitCode == nil || *r.GuestStopProof.ExitCode != 0 ||
		!sandboxDigest(r.PodTemplateSHA) || r.PodNamespace == "" || r.PodName != "sandbox-"+r.ID[:32] {
		return ErrConflict
	}
	fingerprint, err := sandboxRecordFingerprint(address, slot.Record)
	if err != nil || fingerprint != r.RecordFingerprint {
		return ErrConflict
	}
	return nil
}

// Do potentially slow bounded object I/O before taking Catalog.mu. The lock
// holder later rechecks both the real claim and every frozen Run identity.
func (c *Catalog) prepareSandboxSuccess(ctx context.Context, a taskAddress, finish task.Finish) (*sandboxSuccessEvidence, error) {
	slot, err := c.readStoredTask(ctx, a)
	if err != nil {
		return nil, err
	}
	if slot.ExecutionPolicy != task.ExecutionUntrustedIsolated || slot.State != task.Running || finish.Error != "" {
		return nil, nil
	}
	if slot.attempt != finish.Attempt || slot.ID != a.Runtime || slot.TurnID != a.turn || !time.Now().Before(slot.Deadline) {
		return nil, ErrConflict
	}
	p, err := currentSandboxFinishProfile(ctx, a.Scope)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithDeadline(ctx, slot.Deadline)
	defer cancel()
	r, err := c.loadSandbox(ctx, SandboxAddress{Scope: a.Scope, TurnID: a.turn, Attempt: slot.attempt})
	if err != nil {
		return nil, err
	}
	if err = checkSandboxSuccessRun(a, slot, r, p); err != nil {
		return nil, err
	}
	expected, _, err := c.sandboxRequest(ctx, a.Scope, slot.Record, p)
	if err != nil {
		return nil, err
	}
	_, stream, err := c.readSandboxBlob(ctx, r, SandboxInput, r.Input)
	if err != nil {
		return nil, err
	}
	actual, err := nativeguest.InspectRequest(ctx, stream, nativeguest.Limits{MaxInputBytes: r.Input.Size})
	if err == nil {
		// InspectRequest consumes exact EOF; draining also checks the outer
		// reader's independently sealed digest/size before accepting metadata.
		_, err = io.Copy(io.Discard, stream)
	}
	err = errors.Join(err, stream.Close())
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(actual, expected) {
		return nil, ErrConflict
	}
	return &sandboxSuccessEvidence{run: r, profile: p}, nil
}

// Called under Catalog.mu, after refs are verified and before success is
// written. Object data is not reread under the serialization lock.
func (c *Catalog) recheckSandboxSuccess(ctx context.Context, a taskAddress, slot taskSlot, e *sandboxSuccessEvidence) (*SandboxRun, error) {
	if e == nil {
		return nil, ErrConflict
	}
	p, err := currentSandboxFinishProfile(ctx, a.Scope)
	if err != nil || p != e.profile {
		return nil, ErrConflict
	}
	r, err := c.loadSandbox(ctx, SandboxAddress{Scope: a.Scope, TurnID: a.turn, Attempt: slot.attempt})
	if err != nil {
		return nil, err
	}
	if err = checkSandboxSuccessRun(a, slot, r, p); err != nil {
		return nil, err
	}
	old := e.run
	if r.ID != old.ID || r.RecordFingerprint != old.RecordFingerprint || r.ProfileFingerprint != old.ProfileFingerprint ||
		r.Input != old.Input || r.Output != old.Output || r.PodUID != old.PodUID || r.PodName != old.PodName ||
		r.PodNamespace != old.PodNamespace || r.PodTemplateSHA != old.PodTemplateSHA || !r.Deadline.Equal(old.Deadline) {
		return nil, ErrConflict
	}
	return &r, nil
}

func appendSandboxResultAccepted(ctx context.Context, tx *sql.Tx, a taskAddress, slot taskSlot, r SandboxRun) error {
	return appendAudit(ctx, tx, AuditEvent{Scope: a.Scope, Actor: auditActor(ctx, "internal"), Action: "sandbox.result_accepted",
		TaskID: a.Runtime, TurnID: a.turn, AttemptSHA256: auditHash("attempt", slot.attempt), RunID: r.ID, PodUID: r.PodUID,
		Digest: r.Output.SHA256, PreviousState: task.Running, State: task.Succeeded})
}
