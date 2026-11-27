package platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentmoor/internal/task"
	"github.com/google/uuid"
)

// Synthetic controller authority for unit/service fixtures only. Actual node
// status, finalizer races and process shutdown are exercised by node_loss_probe.
func sandboxTestGuestStop(t *testing.T, c *Catalog, a SandboxAddress, uid string, exit int) {
	t.Helper()
	digest := sandboxTestBlob(uid).SHA256
	if _, err := c.RecordSandboxGuestStopped(t.Context(), a, SandboxGuestStopProof{Kind: SandboxGuestTerminated, PodUID: uid, ResourceVersion: "fixture-1", NodeName: "fixture-node", ContainerID: "containerd://" + digest, ContainerName: "guest", ExitCode: &exit}); err != nil {
		t.Fatal("synthetic guest stop-proof rejected", err)
	}
}

// Explicit stopped/deleted guest setup for storage GC positive controls. This
// does not infer shutdown from a failed task, a missing Pod or elapsed time.
func sandboxTestStoppedAndDeletedGuest(t *testing.T, c *Catalog, a SandboxAddress, uid string, exit int) {
	t.Helper()
	if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordSandboxTerminal(t.Context(), a, uid, exit); err != nil {
		t.Fatal(err)
	}
	sandboxTestGuestStop(t, c, a, uid, exit)
	if _, err := c.ConfirmSandboxDeleted(t.Context(), a, uid); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxGuestStopProof(t *testing.T) {
	c := sandboxControllerCatalog(t)
	a := sandboxTestClaim(t, c, scope("stop-proof"))
	p := sandboxTestFence(t, c, a, "synthetic input")
	uid := uuid.NewString()
	if _, err := c.SealSandboxPodTemplate(t.Context(), a, "sandbox", sandboxTestBlob("fixed template").SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("synthetic result"), strings.NewReader("synthetic result")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordSandboxTerminal(t.Context(), a, uid, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConfirmSandboxDeleted(t.Context(), a, uid); !errors.Is(err, ErrNotReady) {
		t.Fatal("old terminal classification manufactured guest stop-proof", err)
	}
	sandboxTestGuestStop(t, c, a, uid, 0)
	r, err := c.ConfirmSandboxDeleted(t.Context(), a, uid)
	if err != nil || !r.GuestStopped() || !r.PodDeleted || r.State != SandboxReady {
		t.Fatal("confirmed stopped guest did not become publishable", err)
	}
	blob, stream, err := c.ReadSandboxOutput(t.Context(), a)
	sandboxTestRead(t, blob, stream, err, "synthetic result")
}

func TestSandboxGuestStopBindingAndLegacyFailClosed(t *testing.T) {
	c := sandboxControllerCatalog(t)
	a := sandboxTestClaim(t, c, scope("stop-legacy"))
	p := sandboxTestFence(t, c, a, "synthetic old input")
	uid := uuid.NewString()
	if _, err := c.SealSandboxPodTemplate(t.Context(), a, "sandbox", sandboxTestBlob("fixed template").SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("synthetic old result"), strings.NewReader("synthetic old result")); err != nil {
		t.Fatal(err)
	}
	// A deliberately seeded pre-upgrade row has old flags, but no new proof.
	legacy, err := c.changeSandbox(t.Context(), a, func(r *SandboxRun) (string, error) {
		r.PodTerminal = true
		r.PodDeleted = true
		r.State = SandboxReady
		r.ExitCode = 0
		return "all", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListSandboxRuns(t.Context(), a.Scope); err != nil {
		t.Fatal("legacy row broke metadata listing", err)
	}
	if _, stream, err := c.ReadSandboxOutput(t.Context(), a); !errors.Is(err, ErrNotReady) || stream != nil {
		t.Fatal("legacy flags authorized new output access", err)
	}
	sandboxTestFinish(t, c, a, true)
	if n, err := c.Collect(t.Context(), a.Scope, time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Fatal("legacy no-proof private data was collected", n, err)
	}
	after, err := c.loadSandbox(t.Context(), a)
	if err != nil || !reflect.DeepEqual(legacy, after) {
		t.Fatal("metadata read/GC rewrote legacy history", err)
	}
	if _, err := c.RecordSandboxGuestStopped(t.Context(), a, SandboxGuestStopProof{Kind: SandboxGuestNeverScheduled, PodUID: uid, ResourceVersion: "1"}); !errors.Is(err, ErrConflict) {
		t.Fatal("legacy deletion flags were retroactively blessed", err)
	}
}

func TestSandboxGuestStopValidationAndAtomicAudit(t *testing.T) {
	c := sandboxControllerCatalog(t)
	a := sandboxTestClaim(t, c, scope("stop-atomic"))
	p := sandboxTestFence(t, c, a, "synthetic input")
	uid := uuid.NewString()
	if _, err := c.SealSandboxPodTemplate(t.Context(), a, "sandbox", sandboxTestBlob("template").SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
		t.Fatal(err)
	}
	code := 143
	good := SandboxGuestStopProof{Kind: SandboxGuestTerminated, PodUID: uid, ResourceVersion: "1", NodeName: "fixture-node", ContainerID: "containerd://" + strings.Repeat("a", 64), ContainerName: "guest", ExitCode: &code}
	for _, mutate := range []func(*SandboxGuestStopProof){func(p *SandboxGuestStopProof) { p.ContainerID = "runc://invalid" }, func(p *SandboxGuestStopProof) { p.NodeName = "" }, func(p *SandboxGuestStopProof) { p.RestartCount = 1 }, func(p *SandboxGuestStopProof) { p.ContainerName = "other" }, func(p *SandboxGuestStopProof) { p.ExitCode = nil }, func(p *SandboxGuestStopProof) { p.ResourceVersion = "" }} {
		bad := good
		mutate(&bad)
		if _, err := c.RecordSandboxGuestStopped(t.Context(), a, bad); err == nil {
			t.Fatal("invalid stop-proof accepted")
		}
	}
	if _, err := c.BindSandboxGuestRuntime(t.Context(), a, uid, good.NodeName, good.ContainerID); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.NodeName = "another-node"
	if _, err := c.RecordSandboxGuestStopped(t.Context(), a, bad); !errors.Is(err, ErrConflict) {
		t.Fatal("stop-proof crossed frozen node binding", err)
	}
	before, err := c.loadSandbox(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.writer.ExecContext(t.Context(), `CREATE TRIGGER reject_guest_stop_audit BEFORE INSERT ON audit_events WHEN json_extract(NEW.event,'$.action')='sandbox.guest_stop_proven' BEGIN SELECT RAISE(ABORT,'synthetic stop audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordSandboxGuestStopped(t.Context(), a, good); err == nil {
		t.Fatal("stop-proof survived rejected audit insert")
	}
	after, err := c.loadSandbox(t.Context(), a)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("failed stop audit committed proof", err)
	}
	for token, purpose := range map[string]SandboxPurpose{p.Capabilities.Input: SandboxInput, p.Capabilities.Output: SandboxOutput, p.Capabilities.Model: SandboxModel} {
		if _, err := c.AuthenticateSandboxCapability(t.Context(), token, purpose); err != nil {
			t.Fatal("failed stop transaction revoked authority", err)
		}
	}
	if _, err := c.writer.ExecContext(context.Background(), "DROP TRIGGER reject_guest_stop_audit"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordSandboxGuestStopped(t.Context(), a, good); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AuthenticateSandboxCapability(t.Context(), p.Capabilities.Model, SandboxModel); !errors.Is(err, ErrSandboxDenied) {
		t.Fatal("proven stopped guest retained model authority", err)
	}
	again := good
	again.ResourceVersion = "2"
	if _, err := c.RecordSandboxGuestStopped(t.Context(), a, again); err != nil {
		t.Fatal("same guest stop observation was not idempotent", err)
	}
}

func TestSandboxGuestStopNeverScheduledAndExitMismatch(t *testing.T) {
	for _, state := range []string{"never-scheduled", "bound-node", "classified-zero-actually-nonzero"} {
		t.Run(state, func(t *testing.T) {
			c := sandboxControllerCatalog(t)
			a := sandboxTestClaim(t, c, scope("stop-"+state))
			p := sandboxTestFence(t, c, a, "input")
			uid := uuid.NewString()
			if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
				t.Fatal(err)
			}
			proof := SandboxGuestStopProof{Kind: SandboxGuestNeverScheduled, PodUID: uid, ResourceVersion: "never-scheduled-cas-1"}
			if state == "bound-node" {
				if _, err := c.BindSandboxGuestRuntime(t.Context(), a, uid, "already-scheduled-node", ""); err != nil {
					t.Fatal(err)
				}
				if _, err := c.RecordSandboxGuestStopped(t.Context(), a, proof); !errors.Is(err, ErrConflict) {
					t.Fatal("scheduled guest accepted a never-scheduled attestation", err)
				}
				if _, err := c.ConfirmSandboxDeleted(t.Context(), a, uid); !errors.Is(err, ErrNotReady) {
					t.Fatal("scheduled guest without proof was deleted", err)
				}
				return
			}
			if state == "classified-zero-actually-nonzero" {
				if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("result"), strings.NewReader("result")); err != nil {
					t.Fatal(err)
				}
				if _, err := c.RecordSandboxTerminal(t.Context(), a, uid, 0); err != nil {
					t.Fatal(err)
				}
				code := 143
				proof = SandboxGuestStopProof{Kind: SandboxGuestTerminated, PodUID: uid, ResourceVersion: "terminal-observation-2", NodeName: "fixture-node", ContainerID: "containerd://" + sandboxTestBlob(uid).SHA256, ContainerName: "guest", ExitCode: &code}
			}
			if _, err := c.RecordSandboxGuestStopped(t.Context(), a, proof); err != nil {
				t.Fatal(err)
			}
			r, err := c.ConfirmSandboxDeleted(t.Context(), a, uid)
			if err != nil || !r.GuestStopped() || !r.PodDeleted || r.State == SandboxReady {
				t.Fatal("non-success guest stop did not remain non-publishable", err)
			}
			if _, stream, err := c.ReadSandboxOutput(t.Context(), a); err == nil || stream != nil {
				t.Fatal("non-success stop proof authorized output", err)
			}
			if _, err := c.FenceSandboxCreate(t.Context(), a, r.Deadline); err == nil {
				t.Fatal("guest stop authorized another native execution")
			}
		})
	}
}

func TestSandboxGuestStopRejectsUnboundedOrForeignProof(t *testing.T) {
	c := sandboxControllerCatalog(t)
	a := sandboxTestClaim(t, c, scope("stop-invalid-boundaries"))
	sandboxTestFence(t, c, a, "input")
	uid, code := uuid.NewString(), 0
	if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
		t.Fatal(err)
	}
	good := SandboxGuestStopProof{Kind: SandboxGuestTerminated, PodUID: uid, ResourceVersion: "1", NodeName: "fixture-node", ContainerID: "containerd://" + sandboxTestBlob(uid).SHA256, ContainerName: "guest", ExitCode: &code}
	before, err := c.loadSandbox(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	var eventCount int
	if err := c.db.QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events WHERE scope=$1", a.Scope.Prefix()).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*SandboxGuestStopProof){
		func(p *SandboxGuestStopProof) { p.Kind = "untrusted-assertion" },
		func(p *SandboxGuestStopProof) { p.PodUID = uuid.NewString() },
		func(p *SandboxGuestStopProof) { p.ResourceVersion = strings.Repeat("1", 257) },
		func(p *SandboxGuestStopProof) { p.ResourceVersion = "1\x00private" },
		func(p *SandboxGuestStopProof) { p.ResourceVersion = "1\n" },
		func(p *SandboxGuestStopProof) { p.NodeName = "node/foreign" },
		func(p *SandboxGuestStopProof) { p.NodeName = strings.Repeat("a", 64) },
		func(p *SandboxGuestStopProof) { p.ContainerID = "containerd://" + strings.Repeat("A", 64) },
		func(p *SandboxGuestStopProof) { p.RestartCount = -1 },
		func(p *SandboxGuestStopProof) { v := 256; p.ExitCode = &v },
		func(p *SandboxGuestStopProof) { v := -1; p.ExitCode = &v },
		func(p *SandboxGuestStopProof) { p.Kind = SandboxGuestNeverScheduled },
	} {
		bad := good
		change(&bad)
		if _, err := c.RecordSandboxGuestStopped(t.Context(), a, bad); err == nil {
			t.Fatal("foreign or malformed proof was accepted")
		}
	}
	for _, address := range []SandboxAddress{{Scope: scope("foreign-stop"), Attempt: a.Attempt}, {Scope: a.Scope, Attempt: uuid.NewString()}} {
		if _, err := c.RecordSandboxGuestStopped(t.Context(), address, good); err == nil {
			t.Fatal("stop proof crossed scope or private attempt")
		}
	}
	after, err := c.loadSandbox(t.Context(), a)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("rejected stop proof changed the frozen Run", err)
	}
	var afterCount int
	if err := c.db.QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events WHERE scope=$1", a.Scope.Prefix()).Scan(&afterCount); err != nil || afterCount != eventCount {
		t.Fatal("rejected proof created an audit event", afterCount, eventCount, err)
	}
}

func TestSandboxGuestStopLegacyRestartKeepsHistoricalAssetReadsAndFinishACK(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("stop-legacy-history")
	h := newAuditLinksHTTP(t, f.c, f.r, s)
	attempt := h.begin(t, "", "")
	a := SandboxAddress{Scope: s, Attempt: attempt}
	p := sandboxTestFence(t, f.c, a, "legacy sealed input")
	uid := uuid.NewString()
	if _, err := f.c.BindSandboxPod(t.Context(), a, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("legacy sealed output"), strings.NewReader("legacy sealed output")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.changeSandbox(t.Context(), a, func(r *SandboxRun) (string, error) {
		r.PodTerminal, r.PodDeleted, r.ExitCode, r.State = true, true, 0, SandboxReady
		return "all", nil
	}); err != nil {
		t.Fatal(err)
	}
	finish, refs := h.receipt(t, "", uuid.NewString(), attempt, "legacy exact bytes")
	var receipt task.Record
	h.json(t, h.worker, "POST", h.route("")+"/finish", finish, &receipt)
	original := make([][]byte, len(refs))
	for i, ref := range refs {
		_, stream, err := h.worker.Content(t.Context(), ref.Key, ref.Version)
		if err != nil {
			t.Fatal(err)
		}
		original[i], err = io.ReadAll(stream)
		if closeErr := stream.Close(); err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
	}
	var rawBefore string
	if err := f.c.db.QueryRowContext(t.Context(), "SELECT record FROM sandbox_runs WHERE scope=$1 AND id=$2", s.Prefix(), p.Run.ID).Scan(&rawBefore); err != nil {
		t.Fatal(err)
	}
	var oldShape map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawBefore), &oldShape); err != nil || oldShape["guest_stop_proof"] != nil || oldShape["pod_node_name"] != nil || oldShape["guest_container_id"] != nil {
		t.Fatal("fixture is not legacy metadata", err)
	}
	count := len(auditEvents(t, f.r, s))
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := OpenCatalog(t.Context(), f.dir, "", f.objects)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ListSandboxRuns(t.Context(), s); err != nil {
		t.Fatal("legacy proof omission was not decode-compatible", err)
	}
	if _, stream, err := c.ReadSandboxOutput(t.Context(), a); !errors.Is(err, ErrNotReady) || stream != nil {
		t.Fatal("legacy flags granted new sandbox output authority", err)
	}
	if n, err := c.Collect(t.Context(), s, time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Fatal("legacy unknown guest private data was collected", n, err)
	}
	next := newAuditLinksHTTP(t, c, f.r, s)
	var ack task.Record
	next.json(t, next.worker, "POST", next.route("")+"/finish", finish, &ack)
	if !reflect.DeepEqual(receipt, ack) || len(auditEvents(t, f.r, s)) != count {
		t.Fatal("legacy successful Finish ACK changed history")
	}
	for i, ref := range refs {
		_, stream, err := next.worker.Content(t.Context(), ref.Key, ref.Version)
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := io.ReadAll(stream)
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil || !reflect.DeepEqual(got, original[i]) {
			t.Fatal("old fixed asset bytes became unreadable", readErr, closeErr)
		}
	}
	var rawAfter string
	if err := c.db.QueryRowContext(t.Context(), "SELECT record FROM sandbox_runs WHERE scope=$1 AND id=$2", s.Prefix(), p.Run.ID).Scan(&rawAfter); err != nil || rawAfter != rawBefore {
		t.Fatal("reopen, legacy reads, GC or Finish ACK rewrote private history", err)
	}
}

func TestSandboxGuestStopCleanupUnknownRevokesEarlierProof(t *testing.T) {
	c := sandboxControllerCatalog(t)
	a := sandboxTestClaim(t, c, scope("stop-proof-invalidated"))
	p := sandboxTestFence(t, c, a, "private input retained")
	uid := uuid.NewString()
	if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("private result retained"), strings.NewReader("private result retained")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordSandboxTerminal(t.Context(), a, uid, 0); err != nil {
		t.Fatal(err)
	}
	sandboxTestGuestStop(t, c, a, uid, 0)
	ready, err := c.ConfirmSandboxDeleted(t.Context(), a, uid)
	if err != nil || !ready.GuestStopped() || ready.State != SandboxReady {
		t.Fatal("missing successful stopped guest positive control", err)
	}
	// A later contradictory trusted observation invalidates the previous
	// authorization. Keep that old evidence readable, without reusing it.
	unknown, err := c.MarkSandboxCleanupUnknown(t.Context(), a)
	if err != nil || !unknown.CleanupUnknown || unknown.GuestStopped() {
		t.Fatal("cleanup-unknown left the earlier proof authoritative", err)
	}
	loaded, err := c.loadSandbox(t.Context(), a)
	if err != nil || loaded.GuestStopProof == nil || !loaded.CleanupUnknown || loaded.GuestStopped() {
		t.Fatal("invalidated proof was unreadable or still authorized", err)
	}
	if _, stream, err := c.ReadSandboxOutput(t.Context(), a); !errors.Is(err, ErrNotReady) || stream != nil {
		t.Fatal("invalidated earlier proof authorized sealed output", err)
	}
	if _, err := c.ConfirmSandboxDeleted(t.Context(), a, uid); !errors.Is(err, ErrNotReady) {
		t.Fatal("invalidated earlier proof authorized deletion", err)
	}
	_, _ = c.RecordSandboxGuestStopped(t.Context(), a, *ready.GuestStopProof)
	loaded, err = c.loadSandbox(t.Context(), a)
	if err != nil || !loaded.CleanupUnknown || loaded.GuestStopped() {
		t.Fatal("replayed earlier evidence silently restored stop authority", err)
	}
	sandboxTestFinish(t, c, a, true)
	if n, err := c.Collect(t.Context(), a.Scope, time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Fatal("invalidated stop proof authorized private data collection", n, err)
	}
	for purpose, blob := range map[SandboxPurpose]SandboxBlob{SandboxInput: loaded.Input, SandboxOutput: loaded.Output} {
		if exists, err := c.objects.Exists(t.Context(), sandboxObjectKey(a.Scope, purpose, blob.SHA256)); err != nil || !exists {
			t.Fatal("unknown guest lost retained private bytes", purpose, exists, err)
		}
	}
}

func TestSandboxGuestStopFrozenProofCannotBeInvalidatedByBindingWrites(t *testing.T) {
	for _, mode := range []string{"runtime-after-never-scheduled", "central-binding-write", "central-proof-write"} {
		t.Run(mode, func(t *testing.T) {
			c := sandboxControllerCatalog(t)
			a := sandboxTestClaim(t, c, scope("stop-frozen-proof-"+mode))
			sandboxTestFence(t, c, a, "retained input")
			uid := uuid.NewString()
			if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
				t.Fatal(err)
			}
			proof := SandboxGuestStopProof{Kind: SandboxGuestNeverScheduled, PodUID: uid, ResourceVersion: "first-never-scheduled-cas"}
			before, err := c.RecordSandboxGuestStopped(t.Context(), a, proof)
			if err != nil || !before.GuestStopped() {
				t.Fatal(err)
			}
			var count int
			if err := c.db.QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events WHERE scope=$1", a.Scope.Prefix()).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if mode == "runtime-after-never-scheduled" {
				_, err = c.BindSandboxGuestRuntime(t.Context(), a, uid, "later-node", "containerd://"+sandboxTestBlob(uid).SHA256)
			} else {
				// The central save guard must protect future internal mutators,
				// not only today's exported binding method.
				_, err = c.changeSandbox(t.Context(), a, func(r *SandboxRun) (string, error) {
					if mode == "central-binding-write" {
						r.PodNodeName = "later-node"
					} else {
						r.GuestStopProof.PodUID = uuid.NewString()
					}
					return "", nil
				})
			}
			if !errors.Is(err, ErrConflict) {
				t.Fatal("binding write invalidated an already frozen stop proof", err)
			}
			after, err := c.loadSandbox(t.Context(), a)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("rejected binding write corrupted the readable Run", err)
			}
			if _, err := c.ListSandboxRuns(t.Context(), a.Scope); err != nil {
				t.Fatal("binding write poisoned scope enumeration", err)
			}
			var afterCount int
			if err := c.db.QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events WHERE scope=$1", a.Scope.Prefix()).Scan(&afterCount); err != nil || afterCount != count {
				t.Fatal("rejected binding write produced audit evidence", afterCount, count, err)
			}
		})
	}
}
