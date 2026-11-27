package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/task"
)

func sandboxTestBlob(raw string) SandboxBlob {
	h := sha256.Sum256([]byte(raw))
	return SandboxBlob{SHA256: hex.EncodeToString(h[:]), Size: int64(len(raw))}
}

var sandboxTestProfile = sandboxTestBlob("operator-selected immutable runtime/image/limits").SHA256

func sandboxTestClaim(t *testing.T, c *Catalog, scope asset.Scope) SandboxAddress {
	t.Helper()
	a := SandboxAddress{Scope: scope, Attempt: uuid.NewString()}
	if _, err := c.submitTask(t.Context(), taskAddress{Scope: scope}, task.Spec{Instruction: "make a report", Input: "private fixture", Output: "report.json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.claimTask(t.Context(), taskAddress{Scope: scope}, task.Claim{Attempt: a.Attempt}); err != nil {
		t.Fatal(err)
	}
	return a
}

func sandboxTestBegin(t *testing.T, c *Catalog, a SandboxAddress, input string) SandboxRun {
	t.Helper()
	r, err := c.BeginSandbox(t.Context(), a, sandboxTestProfile, sandboxTestBlob(input))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func sandboxTestFence(t *testing.T, c *Catalog, a SandboxAddress, input string) SandboxCreatePermit {
	t.Helper()
	r := sandboxTestBegin(t, c, a, input)
	if _, err := c.SealSandboxInput(t.Context(), a, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	p, err := c.FenceSandboxCreate(t.Context(), a, r.Deadline)
	if err != nil || !p.CreateAllowed {
		t.Fatal(p, err)
	}
	return p
}

func sandboxTestFinish(t *testing.T, c *Catalog, a SandboxAddress, failure bool) {
	t.Helper()
	finish := task.Finish{Attempt: a.Attempt}
	if failure {
		finish.Error = "owned fixture execution failed"
	} else {
		finish.SessionID = uuid.NewString()
		for _, key := range []asset.Key{{Kind: "artifact", ID: "out/report.json"}, {Kind: "workspace", ID: "root"}, {Kind: "session", ID: finish.SessionID}} {
			ch := change("published "+key.Kind, 0)
			ch.Key, ch.SessionID = task.TurnKey(a.TurnID, key), finish.SessionID
			ticket := upload(t, c, a.Scope, ch, "published "+key.Kind)
			if _, err := c.Commit(t.Context(), a.Scope, ticket.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := c.finishTask(t.Context(), taskAddress{a.Scope, a.TurnID}, finish); err != nil {
		t.Fatal(err)
	}
}

func sandboxTestRead(t *testing.T, blob SandboxBlob, stream io.ReadCloser, err error, want string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil || string(data) != want || blob.SHA256 != sandboxTestBlob(want).SHA256 || !blob.Sealed {
		t.Fatal("private sealed bytes differ", string(data), blob, err)
	}
}

func TestSandboxStorageContract(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	checkSandboxStorage(t, c, scope("sandbox-contract"))
}

// Reused against actual PostgreSQL/S3, not only a substitute object store.
func checkSandboxStorage(t *testing.T, c *Catalog, base asset.Scope) {
	t.Helper()
	allocate := func(t *testing.T, name string) SandboxAddress {
		t.Helper()
		s := base
		s.Runtime += "-" + name
		return sandboxTestClaim(t, c, s)
	}
	t.Run("frozen_identity_and_concurrent_create_fence", func(t *testing.T) {
		a := allocate(t, "fence")
		input := "frozen input archive bytes"
		r := sandboxTestBegin(t, c, a, input)
		if r.Source.Spec.Input != "private fixture" || !r.Deadline.Equal(r.Source.Deadline) || r.CreateStarted {
			t.Fatal("intent did not derive the catalog claim", r)
		}
		for _, changed := range []struct {
			profile string
			input   SandboxBlob
		}{{sandboxTestBlob("different profile").SHA256, sandboxTestBlob(input)}, {sandboxTestProfile, sandboxTestBlob("changed input")}} {
			if _, err := c.BeginSandbox(t.Context(), a, changed.profile, changed.input); !errors.Is(err, ErrConflict) {
				t.Fatal("same claim changed frozen identity", err)
			}
		}
		if _, err := c.FenceSandboxCreate(t.Context(), a, r.Deadline); !errors.Is(err, ErrNotReady) {
			t.Fatal("unsealed input authorized a Pod", err)
		}
		if _, err := c.SealSandboxInput(t.Context(), a, strings.NewReader(input)); err != nil {
			t.Fatal(err)
		}
		var created atomic.Int32
		var caps SandboxCapabilities
		var capsMu sync.Mutex
		var workers sync.WaitGroup
		for range 12 {
			workers.Go(func() {
				p, err := c.FenceSandboxCreate(t.Context(), a, r.Deadline)
				if err != nil {
					t.Error(err)
					return
				}
				if p.CreateAllowed {
					created.Add(1)
					capsMu.Lock()
					caps = p.Capabilities
					capsMu.Unlock()
				} else if p.Capabilities != (SandboxCapabilities{}) {
					t.Error("replay reissued raw capabilities")
				}
			})
		}
		workers.Wait()
		if created.Load() != 1 || caps.Input == "" || caps.Input == caps.Output || caps.Model == caps.Output {
			t.Fatal("create permit is not unique/purpose-separated", created.Load())
		}
		var raw string
		if err := c.db.QueryRowContext(t.Context(), "SELECT record FROM sandbox_runs WHERE scope=$1 AND id=$2", a.Scope.Prefix(), r.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		rows, err := c.db.QueryContext(t.Context(), "SELECT token_hash FROM sandbox_caps WHERE scope=$1 AND run_id=$2", a.Scope.Prefix(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for rows.Next() {
			var hash string
			if err := rows.Scan(&hash); err != nil || !sandboxDigest(hash) {
				t.Fatal(hash, err)
			}
			raw += hash
			count++
		}
		rows.Close()
		if count != 3 || strings.Contains(raw, caps.Input) || strings.Contains(raw, caps.Output) || strings.Contains(raw, caps.Model) {
			t.Fatal("raw capability was persisted")
		}
		blob, body, err := c.ReadSandboxInput(t.Context(), caps.Input)
		sandboxTestRead(t, blob, body, err, input)
		for _, pair := range []struct {
			token string
			kind  SandboxPurpose
		}{{caps.Input, SandboxOutput}, {caps.Output, SandboxModel}, {caps.Model, SandboxInput}, {"platform.bearer.token", SandboxInput}} {
			if _, err := c.AuthenticateSandboxCapability(t.Context(), pair.token, pair.kind); !errors.Is(err, ErrSandboxDenied) {
				t.Fatal("wrong-purpose/foreign credential admitted", err)
			}
		}
		if _, err := c.FailSandbox(t.Context(), a, "create-unknown"); err != nil {
			t.Fatal(err)
		}
		if p, err := c.FenceSandboxCreate(t.Context(), a, r.Deadline); err == nil || p.CreateAllowed {
			t.Fatal("ambiguous create was reauthorized", p, err)
		}
	})
	t.Run("output_ack_replay_terminal_and_delete_gate", func(t *testing.T) {
		a := allocate(t, "result")
		p := sandboxTestFence(t, c, a, "input")
		uid := uuid.NewString()
		if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
			t.Fatal(err)
		}
		output := "sealed result bytes"
		if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob(output), strings.NewReader(output)); err != nil {
			t.Fatal(err)
		}
		// Drop the successful return value and retry as if its acknowledgement
		// was lost. Already-sealed bytes must not be replaced or re-read live.
		if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob(output), sandboxPanicReader{}); err != nil {
			t.Fatal("sealed replay did not return original result", err)
		}
		if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("changed"), strings.NewReader("changed")); !errors.Is(err, ErrConflict) {
			t.Fatal("result changed on replay", err)
		}
		for _, pair := range []struct {
			token string
			kind  SandboxPurpose
		}{{p.Capabilities.Input, SandboxInput}, {p.Capabilities.Model, SandboxModel}} {
			if _, err := c.AuthenticateSandboxCapability(t.Context(), pair.token, pair.kind); !errors.Is(err, ErrSandboxDenied) {
				t.Fatal("completed output left input/model authority open", err)
			}
		}
		if _, _, err := c.ReadSandboxOutput(t.Context(), a); !errors.Is(err, ErrNotReady) {
			t.Fatal("published before Pod exit", err)
		}
		if _, err := c.RecordSandboxTerminal(t.Context(), a, uuid.NewString(), 0); !errors.Is(err, ErrConflict) {
			t.Fatal("foreign Pod terminal admitted", err)
		}
		if _, err := c.RecordSandboxTerminal(t.Context(), a, uid, 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.ReadSandboxOutput(t.Context(), a); !errors.Is(err, ErrNotReady) {
			t.Fatal("published before Pod deletion", err)
		}
		if _, err := c.ConfirmSandboxDeleted(t.Context(), a, uuid.NewString()); !errors.Is(err, ErrConflict) {
			t.Fatal("foreign Pod deletion admitted", err)
		}
		if _, err := c.ConfirmSandboxDeleted(t.Context(), a, uid); !errors.Is(err, ErrNotReady) {
			t.Fatal("result classification alone authorized deletion", err)
		}
		sandboxTestGuestStop(t, c, a, uid, 0)
		if _, err := c.ConfirmSandboxDeleted(t.Context(), a, uid); err != nil {
			t.Fatal(err)
		}
		blob, body, err := c.ReadSandboxOutput(t.Context(), a)
		sandboxTestRead(t, blob, body, err, output)
		sandboxTestFinish(t, c, a, false)
		blob, body, err = c.ReadSandboxOutput(t.Context(), a)
		sandboxTestRead(t, blob, body, err, output)
		if _, err := c.AuthenticateSandboxCapability(t.Context(), p.Capabilities.Output, SandboxOutput); !errors.Is(err, ErrSandboxDenied) {
			t.Fatal("Finish acknowledgement recovery reopened guest authority", err)
		}
	})
	t.Run("cancellation_and_expiry", func(t *testing.T) {
		a := allocate(t, "cancel")
		p := sandboxTestFence(t, c, a, "input")
		if _, err := c.cancelTask(t.Context(), taskAddress{Scope: a.Scope}); err != nil {
			t.Fatal(err)
		}
		for kind, token := range map[SandboxPurpose]string{SandboxInput: p.Capabilities.Input, SandboxOutput: p.Capabilities.Output, SandboxModel: p.Capabilities.Model} {
			if _, err := c.AuthenticateSandboxCapability(t.Context(), token, kind); !errors.Is(err, ErrSandboxDenied) {
				t.Fatal("task cancellation did not immediately deny capability", kind, err)
			}
		}
		if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("output"), sandboxPanicReader{}); !errors.Is(err, ErrSandboxDenied) {
			t.Fatal(err)
		}
		if _, err := c.CancelSandbox(t.Context(), a); err != nil {
			t.Fatal(err)
		}
		b := allocate(t, "expiry")
		r := sandboxTestBegin(t, c, b, "input")
		if _, err := c.SealSandboxInput(t.Context(), b, strings.NewReader("input")); err != nil {
			t.Fatal(err)
		}
		if _, err := c.FenceSandboxCreate(t.Context(), b, r.Deadline.Add(time.Second)); !errors.Is(err, errBadRequest) {
			t.Fatal("capability extended past the task deadline", err)
		}
		p, err := c.FenceSandboxCreate(t.Context(), b, time.Now().Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.AuthenticateSandboxCapability(t.Context(), p.Capabilities.Model, SandboxModel); err != nil {
			t.Fatal("expiry positive control", err)
		}
		time.Sleep(1100 * time.Millisecond)
		if _, err := c.AuthenticateSandboxCapability(t.Context(), p.Capabilities.Model, SandboxModel); !errors.Is(err, ErrSandboxDenied) {
			t.Fatal("expired capability admitted", err)
		}
	})
	t.Run("bounds_bad_bytes_and_cross_scope", func(t *testing.T) {
		a := allocate(t, "bounds")
		tooLarge := sandboxTestBlob("input")
		tooLarge.Size = SandboxMaxBytes + 1
		if _, err := c.BeginSandbox(t.Context(), a, sandboxTestProfile, tooLarge); !errors.Is(err, errBadRequest) {
			t.Fatal("oversized input declaration admitted", err)
		}
		sandboxTestBegin(t, c, a, "input")
		if _, err := c.SealSandboxInput(t.Context(), a, strings.NewReader("input plus extra")); !errors.Is(err, errBadRequest) {
			t.Fatal("excess raw input admitted", err)
		}
		if _, err := c.SealSandboxInput(t.Context(), a, strings.NewReader("wrong")); !errors.Is(err, errBadRequest) {
			t.Fatal("wrong input digest admitted", err)
		}
		if _, err := c.SealSandboxInput(t.Context(), a, strings.NewReader("input")); err != nil {
			t.Fatal(err)
		}
		r, _ := c.SandboxRun(t.Context(), a)
		p, err := c.FenceSandboxCreate(t.Context(), a, r.Deadline)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, tooLarge, sandboxPanicReader{}); !errors.Is(err, errBadRequest) {
			t.Fatal("oversized output declaration admitted", err)
		}
		if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("output"), strings.NewReader("wrong!")); !errors.Is(err, errBadRequest) {
			t.Fatal("bad output digest admitted", err)
		}
		for _, dimension := range []string{"tenant", "project", "runtime", "adapter"} {
			other := a
			switch dimension {
			case "tenant":
				other.Scope.Tenant += "-foreign"
			case "project":
				other.Scope.Project += "-foreign"
			case "runtime":
				other.Scope.Runtime += "-foreign"
			case "adapter":
				other.Scope.Adapter += "-foreign"
			}
			if _, err := c.SandboxRun(t.Context(), other); !errors.Is(err, ErrMissing) {
				t.Fatal("cross-scope intent read", dimension, err)
			}
		}
	})
	t.Run("real_put_lost_ack_candidate_and_orphan_collection", func(t *testing.T) {
		a := allocate(t, "put-fault")
		p := sandboxTestFence(t, c, a, "input")
		original := c.objects
		fault := &sandboxLostPut{Objects: original, catalog: c, address: a}
		c.objects = fault
		defer func() { c.objects = original }()
		output := sandboxTestBlob("result")
		if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, output, strings.NewReader("result")); err == nil {
			t.Fatal("owned post-Put failure was hidden")
		}
		r, err := c.SandboxRun(t.Context(), a)
		if err != nil || r.Output.SHA256 != output.SHA256 || r.Output.Sealed || !fault.candidateSeen {
			t.Fatal("failed Put lost its registered immutable candidate", r, err)
		}
		if ok, err := original.Exists(t.Context(), sandboxObjectKey(a.Scope, SandboxOutput, output.SHA256)); err != nil || !ok {
			t.Fatal("post-Put fault did not really store bytes", ok, err)
		}
		if n, err := c.Collect(t.Context(), a.Scope, time.Now().Add(time.Hour)); err != nil || n != 0 {
			t.Fatal("active claim candidate was collected", n, err)
		}
		if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, output, strings.NewReader("result")); err != nil {
			t.Fatal("same frozen candidate did not recover", err)
		}
		if _, err := c.FailSandbox(t.Context(), a, "pod-lost"); err != nil {
			t.Fatal(err)
		}
		sandboxTestStoppedAndDeletedGuest(t, c, a, uuid.NewString(), 255)
		sandboxTestFinish(t, c, a, true)
		if n, err := c.Collect(t.Context(), a.Scope, time.Now().Add(-24*time.Hour)); err != nil || n != 0 {
			t.Fatal("retained private objects disappeared", n, err)
		}
		if n, err := c.Collect(t.Context(), a.Scope, time.Now().Add(time.Second)); err != nil || n != 2 {
			t.Fatal("real orphan positive control did not delete input/output", n, err)
		}
		r, err = c.SandboxRun(t.Context(), a)
		if err != nil || !r.Collected || !r.CreateStarted || r.PodName != p.Run.PodName {
			t.Fatal("collection erased the execution tombstone", r, err)
		}
		if q, err := c.FenceSandboxCreate(t.Context(), a, r.Deadline); err == nil || q.CreateAllowed {
			t.Fatal("collected claim recreated a Pod", q, err)
		}
	})
	t.Run("pinned_parent_shared_objects_and_delete_retry", func(t *testing.T) {
		s := base
		s.Runtime += "-pins-shared"
		ch := change("original skill", 0)
		ch.Key = asset.Key{Kind: "skill", ID: "skills/tool"}
		ticket := upload(t, c, s, ch, "original skill")
		if _, err := c.Commit(t.Context(), s, ticket.ID); err != nil {
			t.Fatal(err)
		}
		pins := []task.SkillPin{{Name: "tool", Source: task.Ref{Key: ch.Key, Version: 1, SHA256: ch.SHA256, ContentPath: ch.Key.ContentPath(1)}}}
		a := SandboxAddress{Scope: s, Attempt: uuid.NewString()}
		if _, err := c.submitTask(t.Context(), taskAddress{Scope: s}, task.Spec{Instruction: "first", Output: "report.json"}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.claimTask(t.Context(), taskAddress{Scope: s}, task.Claim{Attempt: a.Attempt, Skills: pins}); err != nil {
			t.Fatal(err)
		}
		p := sandboxTestFence(t, c, a, "shared input")
		uid := uuid.NewString()
		if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
			t.Fatal(err)
		}
		if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("first output"), strings.NewReader("first output")); err != nil {
			t.Fatal(err)
		}
		if _, err := c.RecordSandboxTerminal(t.Context(), a, uid, 0); err != nil {
			t.Fatal(err)
		}
		sandboxTestGuestStop(t, c, a, uid, 0)
		if _, err := c.ConfirmSandboxDeleted(t.Context(), a, uid); err != nil {
			t.Fatal(err)
		}
		sandboxTestFinish(t, c, a, false)
		cutoff := time.Now()
		parent, err := c.readTask(t.Context(), taskAddress{Scope: s})
		if err != nil {
			t.Fatal(err)
		}
		var session string
		for _, ref := range parent.Assets {
			if ref.Kind == "session" {
				session = ref.ID
			}
		}
		b := SandboxAddress{Scope: s, TurnID: uuid.NewString(), Attempt: uuid.NewString()}
		accepted, err := c.submitTurn(t.Context(), taskAddress{s, b.TurnID}, task.Question{SessionID: session, Message: "continue"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.claimTask(t.Context(), taskAddress{s, b.TurnID}, task.Claim{Attempt: b.Attempt, Skills: pins}); err != nil {
			t.Fatal(err)
		}
		q := sandboxTestFence(t, c, b, "shared input")
		if !reflect.DeepEqual(q.Run.Source.Parent, accepted.Parent) || !reflect.DeepEqual(q.Run.Source.Skills, pins) || q.Run.Source.Parent.SessionID != session {
			t.Fatal("intent did not freeze actual parent and Skill pins", q.Run.Source)
		}
		// A new catalog head must not upgrade either frozen revision.
		ch = change("upgraded skill", 1)
		ch.Key = pins[0].Source.Key
		ticket = upload(t, c, s, ch, "upgraded skill")
		if _, err := c.Commit(t.Context(), s, ticket.ID); err != nil {
			t.Fatal(err)
		}
		r, err := c.SandboxRun(t.Context(), b)
		if err != nil || !reflect.DeepEqual(r.Source.Parent, accepted.Parent) || !reflect.DeepEqual(r.Source.Skills, pins) {
			t.Fatal("new head changed the frozen intent", r, err)
		}
		if _, err := c.FailSandbox(t.Context(), b, "owned-test-end"); err != nil {
			t.Fatal(err)
		}
		sandboxTestStoppedAndDeletedGuest(t, c, b, uuid.NewString(), 255)
		sandboxTestFinish(t, c, b, true)
		// The old root run is eligible, but the same input is still referenced
		// by the retained continuation. Inject one real deletion failure first.
		original := c.objects
		fault := &sandboxLostRemove{Objects: original}
		c.objects = fault
		defer func() { c.objects = original }()
		if n, err := c.Collect(t.Context(), s, cutoff); err == nil || n != 0 || !fault.fired {
			t.Fatal("owned deletion failure was not exercised", n, err)
		}
		root, err := c.SandboxRun(t.Context(), a)
		if err != nil || root.Collected {
			t.Fatal("failed deletion discarded its reference", root, err)
		}
		if n, err := c.Collect(t.Context(), s, cutoff); err != nil || n != 1 {
			t.Fatal("retry did not delete only unshared output", n, err)
		}
		if ok, err := original.Exists(t.Context(), sandboxObjectKey(s, SandboxInput, q.Run.Input.SHA256)); err != nil || !ok {
			t.Fatal("retained continuation lost shared private input", ok, err)
		}
		if n, err := c.Collect(t.Context(), s, time.Now().Add(time.Second)); err != nil || n != 1 {
			t.Fatal("last shared reference did not release its object", n, err)
		}
	})
}

type sandboxPanicReader struct{}

func (sandboxPanicReader) Read([]byte) (int, error) {
	panic("replay must not read another live result")
}

type sandboxLostPut struct {
	Objects
	catalog       *Catalog
	address       SandboxAddress
	fired         bool
	candidateSeen bool
}

type sandboxLostRemove struct {
	Objects
	fired bool
}

func (o *sandboxLostRemove) Remove(ctx context.Context, key string) error {
	if !o.fired && strings.Contains(key, "/sandbox/output/") {
		o.fired = true
		return errors.New("owned fault: remove did not complete")
	}
	return o.Objects.Remove(ctx, key)
}

func (o *sandboxLostPut) Put(ctx context.Context, key, file string, size int64) error {
	if !strings.Contains(key, "/sandbox/output/") || o.fired {
		return o.Objects.Put(ctx, key, file, size)
	}
	o.fired = true
	r, err := o.catalog.loadSandbox(ctx, o.address)
	if err != nil || r.Output.SHA256 == "" || r.Output.Sealed {
		return errors.New("output candidate missing before actual Put")
	}
	o.candidateSeen = true
	if err := o.Objects.Put(ctx, key, file, size); err != nil {
		return err
	}
	return errors.New("owned fault: actual object stored but Put acknowledgement lost")
}

func TestSandboxCreateFenceAndCapabilitiesSurviveCatalogRestart(t *testing.T) {
	dir := t.TempDir()
	objects := DiskObjects{t.TempDir()}
	c, err := OpenCatalog(t.Context(), dir, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.Close() }()
	a := sandboxTestClaim(t, c, scope("sandbox-restart"))
	p := sandboxTestFence(t, c, a, "frozen input")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = OpenCatalog(t.Context(), dir, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	q, err := c.FenceSandboxCreate(t.Context(), a, p.Run.Deadline)
	if err != nil || q.CreateAllowed || q.Capabilities != (SandboxCapabilities{}) || q.Run.PodName != p.Run.PodName {
		t.Fatal("restarted service authorized a second create", q, err)
	}
	blob, body, err := c.ReadSandboxInput(t.Context(), p.Capabilities.Input)
	sandboxTestRead(t, blob, body, err, "frozen input")
}

func TestSandboxReadOnlyWriterRejectsNewMutationBeforePut(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	check := prepareSandboxLeaseFailure(t, c)
	original := c.writer
	connection, err := c.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(t.Context(), "PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	c.writer = connection
	defer func() {
		c.writer = original
		connection.ExecContext(context.Background(), "PRAGMA query_only=OFF")
		connection.Close()
	}()
	check()
}

// Lose the actual writer only after the backing object store accepted Put.
// This is reused with a read-only SQLite connection and a killed PostgreSQL
// lease owner, with real DiskObjects/S3 bytes in each case.
func checkSandboxFinalSealFailure(t *testing.T, c *Catalog, loseWriter func()) SandboxCreatePermit {
	t.Helper()
	a := sandboxTestClaim(t, c, scope("sandbox-final-seal"))
	p := sandboxTestFence(t, c, a, "input")
	original := c.objects
	fault := &sandboxAfterPut{Objects: original, after: loseWriter}
	c.objects = fault
	defer func() { c.objects = original }()
	if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("output"), strings.NewReader("output")); err == nil || !fault.fired {
		t.Fatal("successful Put hid a failed final seal", err, fault.fired)
	}
	r, err := c.loadSandbox(t.Context(), a)
	if err != nil || r.Output.SHA256 != sandboxTestBlob("output").SHA256 || r.Output.Sealed || !r.CreateStarted {
		t.Fatal("final seal failure lost candidate or create fence", r, err)
	}
	if ok, err := original.Exists(t.Context(), sandboxObjectKey(a.Scope, SandboxOutput, r.Output.SHA256)); err != nil || !ok {
		t.Fatal("post-Put writer loss did not really store bytes", ok, err)
	}
	return p
}

type sandboxAfterPut struct {
	Objects
	fired bool
	after func()
}

func (o *sandboxAfterPut) Put(ctx context.Context, key, file string, size int64) error {
	if err := o.Objects.Put(ctx, key, file, size); err != nil {
		return err
	}
	if !o.fired && strings.Contains(key, "/sandbox/output/") {
		o.fired = true
		o.after()
	}
	return nil
}

func TestSandboxFinalSealFailureRetainsCandidate(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writer := c.writer
	readonly, err := c.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readonly.ExecContext(t.Context(), "PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		c.writer = writer
		readonly.ExecContext(context.Background(), "PRAGMA query_only=OFF")
		readonly.Close()
	}()
	p := checkSandboxFinalSealFailure(t, c, func() { c.writer = readonly })
	c.writer = writer
	if r, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("output"), strings.NewReader("output")); err != nil || !r.Output.Sealed {
		t.Fatal("candidate could not be sealed by the healthy writer", r, err)
	}
}

func TestSandboxReadVerifiesStoredBytesAndCancellation(t *testing.T) {
	objects := DiskObjects{t.TempDir()}
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", objects)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a := sandboxTestClaim(t, c, scope("sandbox-corruption"))
	p := sandboxTestFence(t, c, a, "input")
	key := sandboxObjectKey(a.Scope, SandboxInput, p.Run.Input.SHA256)
	for _, bad := range []string{"short", "input-extra", "wrong"} {
		if err := os.WriteFile(filepath.Join(objects.Dir, key), []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		_, stream, err := c.ReadSandboxInput(t.Context(), p.Capabilities.Input)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(stream)
		stream.Close()
		if !errors.Is(err, ErrConflict) {
			t.Fatal("backing store corruption passed digest/size verification", bad, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, stream, err := c.ReadSandboxInput(ctx, p.Capabilities.Input)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	cancel()
	if _, err := stream.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled private stream kept reading", err)
	}
}

// Also called after terminating the real PostgreSQL lease-owning connection.
func prepareSandboxLeaseFailure(t *testing.T, c *Catalog) func() {
	t.Helper()
	a := sandboxTestClaim(t, c, scope("sandbox-lease-input"))
	sandboxTestBegin(t, c, a, "input")
	b := sandboxTestClaim(t, c, scope("sandbox-lease-fence"))
	r := sandboxTestBegin(t, c, b, "input")
	if _, err := c.SealSandboxInput(t.Context(), b, strings.NewReader("input")); err != nil {
		t.Fatal(err)
	}
	d := sandboxTestClaim(t, c, scope("sandbox-lease-output"))
	p := sandboxTestFence(t, c, d, "input")
	return func() {
		t.Helper()
		if _, err := c.SealSandboxInput(t.Context(), a, bytes.NewBufferString("input")); err == nil {
			t.Fatal("read-only/dead writer sealed input")
		}
		if q, err := c.FenceSandboxCreate(t.Context(), b, r.Deadline); err == nil || q.CreateAllowed || q.Capabilities != (SandboxCapabilities{}) {
			t.Fatal("read-only/dead writer authorized Pod create", q, err)
		}
		if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("output"), strings.NewReader("output")); err == nil {
			t.Fatal("read-only/dead writer registered output")
		}
		for _, address := range []SandboxAddress{a, b, d} {
			row, err := c.SandboxRun(t.Context(), address)
			if err != nil || row.Output.SHA256 != "" || (address == a && row.Input.Sealed) || (address == b && row.CreateStarted) {
				t.Fatal("failed writer changed run metadata", row, err)
			}
		}
		if ok, err := c.objects.Exists(t.Context(), sandboxObjectKey(a.Scope, SandboxInput, sandboxTestBlob("input").SHA256)); err != nil || ok {
			t.Fatal("failed writer performed an input object Put", ok, err)
		}
		if ok, err := c.objects.Exists(t.Context(), sandboxObjectKey(d.Scope, SandboxOutput, sandboxTestBlob("output").SHA256)); err != nil || ok {
			t.Fatal("failed writer performed an output object Put", ok, err)
		}
	}
}

func TestSandboxIgnoresUntrustedChangedTaskAndRemovesPartialSpool(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a := sandboxTestClaim(t, c, scope("sandbox-record-drift"))
	p := sandboxTestFence(t, c, a, "input")
	slot, err := c.readTask(t.Context(), taskAddress{Scope: a.Scope})
	if err != nil {
		t.Fatal(err)
	}
	slot.Spec.Instruction = "changed catalog fixture instruction"
	raw, _ := json.Marshal(slot.Record)
	if _, err := c.writer.ExecContext(t.Context(), "UPDATE tasks SET record=$1 WHERE scope=$2", string(raw), a.Scope.Prefix()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AuthenticateSandboxCapability(t.Context(), p.Capabilities.Model, SandboxModel); !errors.Is(err, ErrSandboxDenied) {
		t.Fatal("mutable task drift authorized old capability", err)
	}
	if _, err := c.BeginSandbox(t.Context(), a, sandboxTestProfile, sandboxTestBlob("input")); !errors.Is(err, ErrConflict) {
		t.Fatal("changed task reused frozen intent", err)
	}
	entries, err := filepath.Glob(filepath.Join(c.sandboxTemp, ".upload-*"))
	if err != nil || len(entries) != 0 {
		t.Fatal("private spool leaked", entries, err)
	}
}
