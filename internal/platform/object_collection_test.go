package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

var objectCollectionPrivateFailure = errors.New("synthetic private object fault /host/private-data AGENTMOOR_TOKEN=not-for-audit")

// These faults surround the real DiskObjects operation. In particular an
// after-Remove failure means the bytes really disappeared, not that a mock
// returned a successful-looking result. This is not an HTTP transport test.
type objectCollectionFaultObjects struct {
	Objects
	key                       string
	mode                      string
	fired, rejectPut          bool
	removeCalls, removeACKs   int
	putCalls                  int
	beforeRemove, afterRemove func()
}

func (o *objectCollectionFaultObjects) Remove(ctx context.Context, key string) error {
	if key != o.key {
		return o.Objects.Remove(ctx, key)
	}
	o.removeCalls++
	if o.beforeRemove != nil {
		o.beforeRemove()
	}
	if !o.fired && o.mode == "before-remove" {
		o.fired = true
		return objectCollectionPrivateFailure
	}
	if err := o.Objects.Remove(ctx, key); err != nil {
		return err
	}
	o.removeACKs++
	if o.afterRemove != nil {
		o.afterRemove()
	}
	if !o.fired && o.mode == "lost-remove-ack" {
		o.fired = true
		return objectCollectionPrivateFailure
	}
	return nil
}

func (o *objectCollectionFaultObjects) Put(ctx context.Context, key, src string, size int64) error {
	if key == o.key {
		o.putCalls++
		if o.rejectPut {
			return objectCollectionPrivateFailure
		}
	}
	return o.Objects.Put(ctx, key, src, size)
}

func objectCollectionEvents(t *testing.T, f auditFixture, s asset.Scope) []AuditEvent {
	t.Helper()
	out := []AuditEvent{}
	for _, event := range auditEvents(t, f.r, s) {
		if strings.HasPrefix(event.Action, "object.collection_") {
			out = append(out, event)
		}
	}
	return out
}

func objectCollectionState(t *testing.T, c *Catalog, operation string) string {
	t.Helper()
	var state string
	if err := c.db.QueryRowContext(t.Context(), "SELECT state FROM object_collection_intents WHERE operation_id=$1", operation).Scan(&state); err != nil {
		t.Fatal("missing durable object collection intent", err)
	}
	return state
}

func objectCollectionRejectAudit(t *testing.T, c *Catalog) func() {
	t.Helper()
	if _, err := c.db.ExecContext(t.Context(), `CREATE TRIGGER reject_collection_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'owned collection audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	removed := false
	drop := func() {
		if removed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := c.db.ExecContext(ctx, "DROP TRIGGER reject_collection_audit"); err != nil {
			t.Error(err)
			return
		}
		removed = true
	}
	t.Cleanup(drop)
	return drop
}

func objectCollectionExists(t *testing.T, objects Objects, key string, want bool) {
	t.Helper()
	if exists, err := objects.Exists(t.Context(), key); err != nil || exists != want {
		t.Fatal("physical object existence", exists, want, err)
	}
}

func objectCollectionRestart(t *testing.T, f *auditFixture, objects Objects) {
	t.Helper()
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := OpenCatalog(t.Context(), f.dir, "", objects)
	if err != nil {
		t.Fatal("could not reopen actual catalog", err)
	}
	f.c = c
	t.Cleanup(func() { c.Close() })
}

func objectCollectionRequirePair(t *testing.T, events []AuditEvent, kind, digest, proof string) {
	t.Helper()
	if len(events) != 2 {
		t.Fatalf("collection events=%d, want a plan and a confirmation: %+v", len(events), events)
	}
	plan, confirmed := events[0], events[1]
	var key string
	switch kind {
	case objectAssetBlob:
		key = objectKey(plan.Scope, digest)
	case objectWorkspacePart:
		key = partKey(plan.Scope, digest)
	case objectSandboxInput:
		key = sandboxObjectKey(plan.Scope, SandboxInput, digest)
	case objectSandboxOutput:
		key = sandboxObjectKey(plan.Scope, SandboxOutput, digest)
	default:
		t.Fatal("unexpected physical object kind", kind)
	}
	if plan.Action != "object.collection_planned" || confirmed.Action != "object.collection_confirmed" ||
		plan.OperationID != confirmed.OperationID || plan.ObjectIDSHA256 != confirmed.ObjectIDSHA256 ||
		plan.ObjectKind != kind || confirmed.ObjectKind != kind || plan.Digest != "" || confirmed.Digest != "" ||
		plan.ObjectIDSHA256 != auditHash("object-id/"+kind, key) ||
		plan.CollectionProof != "" || confirmed.CollectionProof != proof || plan.Scope != confirmed.Scope {
		t.Fatal("collection evidence does not identify one physical deletion", events)
	}
	if _, err := uuid.Parse(plan.OperationID); err != nil || !sandboxDigest(plan.ObjectIDSHA256) {
		t.Fatal("collection identity is not bounded", events)
	}
	for _, event := range events {
		raw, err := json.Marshal(event)
		if err != nil || strings.Contains(string(raw), "/objects/") || strings.Contains(string(raw), "/sandbox/") ||
			strings.Contains(string(raw), "/host/private-data") || strings.Contains(string(raw), "AGENTMOOR_TOKEN") ||
			strings.Contains(string(raw), "owned-collector-") {
			t.Fatal("collection audit contains a path, credential or private error", err)
		}
	}
}

func objectCollectionUploadAgain(t *testing.T, c *Catalog, s asset.Scope, ch asset.Change, value string) error {
	t.Helper()
	ticket, err := c.Begin(t.Context(), s, ch)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "frozen-object")
	if err = os.WriteFile(file, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	// An identical, valid re-delivery still reaches the public Catalog Upload
	// boundary even if Begin observed the previous incarnation's bytes.
	return c.Upload(t.Context(), s, ticket.ID, file, ch.SHA256, ch.Size)
}

func TestObjectCollectionFourKindsAuditActualDeletes(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("collection-four-kinds")
	value := "same immutable body in four object namespaces"
	ch := change(value, 0)
	upload(t, f.c, s, ch, value)
	partChange, parts, _, bodies := tarFixture(t, 0, tarFile{"body.txt", value})
	ticket, err := f.c.Begin(t.Context(), s, partChange)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := f.c.PrepareParts(t.Context(), s, ticket.ID, parts)
	if err != nil || len(missing) != 1 || missing[0].SHA256 != ch.SHA256 {
		t.Fatal("fixture must share a body digest, not an object key", missing, err)
	}
	uploadParts(t, f.c, s, ticket.ID, missing, bodies)
	a := sandboxTestClaim(t, f.c, s)
	permit := sandboxTestFence(t, f.c, a, value)
	if _, err = f.c.SealSandboxOutput(t.Context(), permit.Capabilities.Output, sandboxTestBlob(value), strings.NewReader(value)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.c.FailSandbox(t.Context(), a, "pod-lost"); err != nil {
		t.Fatal(err)
	}
	sandboxTestFinish(t, f.c, a, true)
	keys := map[string]string{
		objectAssetBlob:     objectKey(s, ch.SHA256),
		objectWorkspacePart: partKey(s, ch.SHA256),
		objectSandboxInput:  sandboxObjectKey(s, SandboxInput, ch.SHA256),
		objectSandboxOutput: sandboxObjectKey(s, SandboxOutput, ch.SHA256),
	}
	for _, key := range keys {
		objectCollectionExists(t, f.objects, key, true)
	}
	if events := objectCollectionEvents(t, f, s); len(events) != 0 {
		t.Fatal("ordinary object writes invented deletion history", events)
	}
	cutoff := time.Now().Add(time.Hour)
	if n, err := f.c.Collect(t.Context(), s, cutoff); err != nil || n != 4 {
		t.Fatal("all three GC selectors did not actually remove four blobs", n, err)
	}
	byKind := map[string][]AuditEvent{}
	for _, event := range objectCollectionEvents(t, f, s) {
		byKind[event.ObjectKind] = append(byKind[event.ObjectKind], event)
	}
	identities := map[string]bool{}
	for kind, key := range keys {
		objectCollectionExists(t, f.objects, key, false)
		events := byKind[kind]
		objectCollectionRequirePair(t, events, kind, ch.SHA256, "remove_ack")
		if identities[events[0].OperationID] || identities[events[0].ObjectIDSHA256] {
			t.Fatal("same SHA collapsed different physical object namespaces", events)
		}
		identities[events[0].OperationID], identities[events[0].ObjectIDSHA256] = true, true
		if state := objectCollectionState(t, f.c, events[0].OperationID); state != "confirmed" {
			t.Fatal(state)
		}
	}
	if len(byKind) != len(keys) {
		t.Fatal("GC emitted evidence for an unexpected object kind", byKind)
	}
	run, err := f.c.SandboxRun(t.Context(), a)
	if err != nil || !run.Collected || !run.CreateStarted || run.PodName != permit.Run.PodName {
		t.Fatal("physical collection lost its execution tombstone", run, err)
	}
	before := objectCollectionEvents(t, f, s)
	objectCollectionRestart(t, &f, f.objects)
	if n, err := f.c.Collect(t.Context(), s, cutoff); err != nil || n != 0 {
		t.Fatal("restart collected absent objects again", n, err)
	}
	if after := objectCollectionEvents(t, f, s); !reflect.DeepEqual(before, after) {
		t.Fatal("reopening or replay duplicated physical collection evidence")
	}
}

func TestObjectCollectionDeleteFailureAndConfirmationRecovery(t *testing.T) {
	for _, mode := range []string{"before-remove", "lost-remove-ack", "confirmation-audit"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuditFixture(t)
			s := scope("collection-fault-" + mode)
			value := "actual disk object before a selected failure"
			ch := change(value, 0)
			upload(t, f.c, s, ch, value)
			fault := &objectCollectionFaultObjects{Objects: f.objects, key: objectKey(s, ch.SHA256), mode: mode}
			fault.beforeRemove = func() {
				events := objectCollectionEvents(t, f, s)
				if len(events) != 1 || events[0].Action != "object.collection_planned" || objectCollectionState(t, f.c, events[0].OperationID) != "pending" {
					t.Fatal("external Delete ran without its committed plan", events)
				}
			}
			var drop func()
			if mode == "confirmation-audit" {
				fault.afterRemove = func() {
					objectCollectionExists(t, f.objects, fault.key, false)
					if drop == nil {
						drop = objectCollectionRejectAudit(t, f.c)
					}
				}
			}
			f.c.objects = fault
			cutoff := time.Now().Add(time.Hour)
			ctxA := auditHTTPContext(t.Context(), "owned-collector-first", "gc")
			if n, err := f.c.Collect(ctxA, s, cutoff); err == nil || n != 0 {
				t.Fatal("selected Delete/confirmation failure was hidden or counted as success", n, err)
			}
			objectCollectionExists(t, f.objects, fault.key, mode == "before-remove")
			events := objectCollectionEvents(t, f, s)
			if len(events) != 1 || events[0].Action != "object.collection_planned" || objectCollectionState(t, f.c, events[0].OperationID) != "pending" {
				t.Fatal("failed deletion lost its durable pending plan", events)
			}
			wantACKs := 1
			if mode == "before-remove" {
				wantACKs = 0
			}
			if fault.removeCalls != 1 || fault.removeACKs != wantACKs {
				t.Fatal("fault did not surround the actual object operation", fault.removeCalls, fault.removeACKs)
			}
			if drop != nil {
				drop()
				fault.afterRemove = nil
			}
			objectCollectionRestart(t, &f, fault)
			ctxB := auditHTTPContext(t.Context(), "owned-collector-recovery", "gc")
			wantN, proof := 0, "absence"
			if mode == "before-remove" {
				wantN, proof = 1, "remove_ack"
			}
			if n, err := f.c.Collect(ctxB, s, cutoff); err != nil || n != wantN {
				t.Fatal("reopened catalog could not finish the existing physical operation", n, wantN, err)
			}
			after := objectCollectionEvents(t, f, s)
			objectCollectionRequirePair(t, after, objectAssetBlob, ch.SHA256, proof)
			if after[0].OperationID != events[0].OperationID || after[0].Actor != auditActor(ctxA, "internal") || after[1].Actor != auditActor(ctxB, "internal") {
				t.Fatal("recovery changed the operation or attributed both steps to one credential", after)
			}
			if state := objectCollectionState(t, f.c, after[0].OperationID); state != "confirmed" {
				t.Fatal(state)
			}
			wantCalls := 1
			if mode == "before-remove" {
				wantCalls = 2
			}
			if fault.removeCalls != wantCalls || fault.removeACKs != 1 {
				t.Fatal("absence recovery unnecessarily issued another Delete", fault.removeCalls, fault.removeACKs)
			}
			if n, err := f.c.Collect(ctxB, s, cutoff); err != nil || n != 0 || !reflect.DeepEqual(after, objectCollectionEvents(t, f, s)) {
				t.Fatal("confirmed recovery was not idempotent", n, err)
			}
		})
	}
}

func TestObjectCollectionPlanAuditFailurePreventsDelete(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("collection-plan-audit")
	value := "a plan must be durable before external deletion"
	ch := change(value, 0)
	upload(t, f.c, s, ch, value)
	fault := &objectCollectionFaultObjects{Objects: f.objects, key: objectKey(s, ch.SHA256)}
	f.c.objects = fault
	drop := objectCollectionRejectAudit(t, f.c)
	cutoff := time.Now().Add(time.Hour)
	if n, err := f.c.Collect(t.Context(), s, cutoff); err == nil || n != 0 || fault.removeCalls != 0 {
		t.Fatal("audit failure allowed an external Delete", n, fault.removeCalls, err)
	}
	objectCollectionExists(t, f.objects, fault.key, true)
	var intents int
	if err := f.c.db.QueryRowContext(t.Context(), "SELECT count(*) FROM object_collection_intents WHERE scope=$1", s.Prefix()).Scan(&intents); err != nil || intents != 0 || len(objectCollectionEvents(t, f, s)) != 0 {
		t.Fatal("failed audit transaction left a plan or evidence", intents, err)
	}
	drop()
	if n, err := f.c.Collect(t.Context(), s, cutoff); err != nil || n != 1 || fault.removeCalls != 1 {
		t.Fatal("removing actual audit rejection did not recover collection", n, err)
	}
	objectCollectionRequirePair(t, objectCollectionEvents(t, f, s), objectAssetBlob, ch.SHA256, "remove_ack")
}

func TestObjectCollectionConfirmedObjectCanHaveANewIncarnation(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("collection-new-incarnation")
	value := "same bytes can be published after physical collection"
	ch := change(value, 0)
	upload(t, f.c, s, ch, value)
	cutoff := time.Now().Add(time.Hour)
	if n, err := f.c.Collect(t.Context(), s, cutoff); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	first := objectCollectionEvents(t, f, s)
	objectCollectionRequirePair(t, first, objectAssetBlob, ch.SHA256, "remove_ack")
	objectCollectionRestart(t, &f, f.objects)
	if err := objectCollectionUploadAgain(t, f.c, s, ch, value); err != nil {
		t.Fatal(err)
	}
	objectCollectionExists(t, f.objects, objectKey(s, ch.SHA256), true)
	if afterPut := objectCollectionEvents(t, f, s); !reflect.DeepEqual(first, afterPut) {
		t.Fatal("a new Put rewrote confirmed history", afterPut)
	}
	if n, err := f.c.Collect(t.Context(), s, cutoff); err != nil || n != 1 {
		t.Fatal("new physical incarnation was mistaken for an ACK replay", n, err)
	}
	all := objectCollectionEvents(t, f, s)
	if len(all) != 4 {
		t.Fatal("two real object incarnations did not have two deletion pairs", all)
	}
	objectCollectionRequirePair(t, all[2:], objectAssetBlob, ch.SHA256, "remove_ack")
	if all[0].OperationID == all[2].OperationID || all[0].ObjectIDSHA256 != all[2].ObjectIDSHA256 {
		t.Fatal("object identity and per-incarnation operation identity were confused", all)
	}
}

func TestObjectCollectionPutSupersedesPendingBeforeItsOwnFailure(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("collection-put-supersedes")
	value := "old Delete result must not be guessed by the next Put"
	ch := change(value, 0)
	upload(t, f.c, s, ch, value)
	fault := &objectCollectionFaultObjects{Objects: f.objects, key: objectKey(s, ch.SHA256), mode: "lost-remove-ack"}
	f.c.objects = fault
	cutoff := time.Now().Add(time.Hour)
	if n, err := f.c.Collect(t.Context(), s, cutoff); err == nil || n != 0 {
		t.Fatal(n, err)
	}
	plan := objectCollectionEvents(t, f, s)
	if len(plan) != 1 {
		t.Fatal(plan)
	}
	fault.rejectPut = true
	if err := objectCollectionUploadAgain(t, f.c, s, ch, value); err == nil || fault.putCalls != 1 {
		t.Fatal("selected new Put failure was not reached", fault.putCalls, err)
	}
	events := objectCollectionEvents(t, f, s)
	if len(events) != 2 || events[1].Action != "object.collection_superseded" || events[1].OperationID != plan[0].OperationID || events[1].CollectionProof != "" || objectCollectionState(t, f.c, plan[0].OperationID) != "superseded" {
		t.Fatal("new Put did not conservatively retire the old ambiguous Delete", events)
	}
	objectCollectionExists(t, f.objects, fault.key, false)
	if n, err := f.c.Collect(t.Context(), s, cutoff); err != nil || n != 0 || !reflect.DeepEqual(events, objectCollectionEvents(t, f, s)) {
		t.Fatal("failed new Put invented physical deletion evidence", n, err)
	}
	fault.rejectPut = false
	objectCollectionRestart(t, &f, fault)
	if err := objectCollectionUploadAgain(t, f.c, s, ch, value); err != nil {
		t.Fatal(err)
	}
	if n, err := f.c.Collect(t.Context(), s, cutoff); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	all := objectCollectionEvents(t, f, s)
	if len(all) != 4 {
		t.Fatal("superseded and new incarnations were not independently durable", all)
	}
	objectCollectionRequirePair(t, all[2:], objectAssetBlob, ch.SHA256, "remove_ack")
	if all[2].OperationID == plan[0].OperationID {
		t.Fatal("new Put reused an ambiguous old Delete operation")
	}
}

func TestObjectCollectionSupersedeAuditFailurePreventsPut(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("collection-supersede-audit")
	value := "a rejected retirement cannot authorize an external Put"
	ch := change(value, 0)
	upload(t, f.c, s, ch, value)
	fault := &objectCollectionFaultObjects{Objects: f.objects, key: objectKey(s, ch.SHA256), mode: "before-remove"}
	f.c.objects = fault
	cutoff := time.Now().Add(time.Hour)
	if n, err := f.c.Collect(t.Context(), s, cutoff); err == nil || n != 0 {
		t.Fatal(n, err)
	}
	before := objectCollectionEvents(t, f, s)
	if len(before) != 1 {
		t.Fatal(before)
	}
	drop := objectCollectionRejectAudit(t, f.c)
	if err := objectCollectionUploadAgain(t, f.c, s, ch, value); err == nil || fault.putCalls != 0 {
		t.Fatal("failed supersede audit reached external Put", fault.putCalls, err)
	}
	if objectCollectionState(t, f.c, before[0].OperationID) != "pending" || !reflect.DeepEqual(before, objectCollectionEvents(t, f, s)) {
		t.Fatal("failed supersede escaped its audit transaction")
	}
	objectCollectionExists(t, f.objects, fault.key, true)
	drop()
	if err := objectCollectionUploadAgain(t, f.c, s, ch, value); err != nil || fault.putCalls != 1 {
		t.Fatal("same frozen Put did not recover", fault.putCalls, err)
	}
	after := objectCollectionEvents(t, f, s)
	if len(after) != 2 || after[1].Action != "object.collection_superseded" || after[1].OperationID != before[0].OperationID || objectCollectionState(t, f.c, before[0].OperationID) != "superseded" {
		t.Fatal("recovered supersede was not recorded exactly once", after)
	}
}

func TestObjectCollectionPutDoesNotSupersedeAnotherScopeOrKind(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("collection-scope-owner")
	value := "the same body digest is not collection authority"
	ch := change(value, 0)
	upload(t, f.c, s, ch, value)
	fault := &objectCollectionFaultObjects{Objects: f.objects, key: objectKey(s, ch.SHA256), mode: "before-remove"}
	f.c.objects = fault
	if n, err := f.c.Collect(t.Context(), s, time.Now().Add(time.Hour)); err == nil || n != 0 {
		t.Fatal(n, err)
	}
	before := objectCollectionEvents(t, f, s)
	if len(before) != 1 {
		t.Fatal(before)
	}
	for _, dimension := range []string{"tenant", "project", "runtime", "adapter"} {
		other := s
		switch dimension {
		case "tenant":
			other.Tenant = "another-tenant"
		case "project":
			other.Project = "another-project"
		case "runtime":
			other.Runtime = "another-runtime"
		case "adapter":
			other.Adapter = "another-adapter"
		}
		if err := objectCollectionUploadAgain(t, f.c, other, ch, value); err != nil {
			t.Fatal("independent scope could not store its own bytes", dimension, err)
		}
		if events := objectCollectionEvents(t, f, other); len(events) != 0 {
			t.Fatal("another scope inherited the owner's Delete plan", dimension, events)
		}
		if n, err := f.c.Collect(t.Context(), other, time.Now().Add(time.Hour)); err != nil || n != 1 {
			t.Fatal("another scope could not collect its independent object", dimension, n, err)
		}
		events := objectCollectionEvents(t, f, other)
		objectCollectionRequirePair(t, events, objectAssetBlob, ch.SHA256, "remove_ack")
		if events[0].OperationID == before[0].OperationID || events[0].ObjectIDSHA256 == before[0].ObjectIDSHA256 {
			t.Fatal("another scope shared collection identity", dimension)
		}
	}
	for _, kind := range []string{objectWorkspacePart, objectSandboxInput, objectSandboxOutput} {
		f.c.mu.Lock()
		err := f.c.prepareObjectPut(t.Context(), s, kind, ch.SHA256)
		f.c.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	if objectCollectionState(t, f.c, before[0].OperationID) != "pending" || !reflect.DeepEqual(before, objectCollectionEvents(t, f, s)) {
		t.Fatal("a different scope or physical kind retired the owner's Delete plan")
	}
	objectCollectionExists(t, f.objects, fault.key, true)
}

func TestObjectCollectionGuestOutputSupersedeUsesAuthenticatedRun(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("collection-guest-output")
	value := "identical output bytes in two independently authenticated runs"
	a := sandboxTestClaim(t, f.c, s)
	permit := sandboxTestFence(t, f.c, a, "original frozen input")
	uid := uuid.NewString()
	if _, err := f.c.BindSandboxPod(t.Context(), a, uid); err != nil {
		t.Fatal(err)
	}
	output := sandboxTestBlob(value)
	if _, err := f.c.SealSandboxOutput(t.Context(), permit.Capabilities.Output, output, strings.NewReader(value)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.RecordSandboxTerminal(t.Context(), a, uid, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.ConfirmSandboxDeleted(t.Context(), a, uid); err != nil {
		t.Fatal(err)
	}
	sandboxTestFinish(t, f.c, a, false)
	fault := &objectCollectionFaultObjects{Objects: f.objects, key: sandboxObjectKey(s, SandboxOutput, output.SHA256), mode: "lost-remove-ack"}
	f.c.objects = fault
	// Depending on map iteration the independent input can be deleted first;
	// only the selected output's real successful Remove loses its ACK.
	if _, err := f.c.Collect(t.Context(), s, time.Now().Add(time.Hour)); err == nil || !fault.fired || fault.removeACKs != 1 {
		t.Fatal("old output did not reach the real post-Remove fault", err)
	}
	before := objectCollectionEvents(t, f, s)
	var plan AuditEvent
	for _, event := range before {
		if event.ObjectKind == objectSandboxOutput {
			if plan.OperationID != "" || event.Action != "object.collection_planned" {
				t.Fatal("ambiguous old output must have only one pending plan", before)
			}
			plan = event
		}
	}
	if plan.OperationID == "" || objectCollectionState(t, f.c, plan.OperationID) != "pending" {
		t.Fatal("old output has no recoverable collection intent")
	}
	parent, err := f.c.readTask(t.Context(), taskAddress{Scope: s})
	if err != nil {
		t.Fatal(err)
	}
	var session string
	for _, ref := range parent.Assets {
		if ref.Kind == "session" {
			session = ref.ID
		}
	}
	if session == "" {
		t.Fatal("completed root fixture has no published session")
	}
	b := SandboxAddress{Scope: s, TurnID: uuid.NewString(), Attempt: uuid.NewString()}
	if _, err := f.c.submitTurn(t.Context(), taskAddress{s, b.TurnID}, task.Question{SessionID: session, Message: "continue the same session"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.claimTask(t.Context(), taskAddress{s, b.TurnID}, task.Claim{Attempt: b.Attempt}); err != nil {
		t.Fatal(err)
	}
	next := sandboxTestFence(t, f.c, b, "new frozen input")
	bound := next.Run
	bound.Output = output
	guest := auditCapabilityContext(t.Context(), SandboxOutput)
	wrongScope, wrongDigest := bound, bound
	wrongScope.Scope.Tenant = "another-tenant"
	wrongDigest.Output = sandboxTestBlob("another frozen output")
	for _, invalid := range []struct {
		name string
		ctx  context.Context
	}{
		{"missing-run-binding", guest},
		{"wrong-run-scope", objectCollectionRunContext(guest, wrongScope)},
		{"wrong-output-digest", objectCollectionRunContext(guest, wrongDigest)},
		{"wrong-capability-purpose", objectCollectionRunContext(auditCapabilityContext(t.Context(), SandboxModel), bound)},
	} {
		f.c.mu.Lock()
		err := f.c.prepareObjectPut(invalid.ctx, s, objectSandboxOutput, output.SHA256)
		f.c.mu.Unlock()
		if !errors.Is(err, ErrAudit) || objectCollectionState(t, f.c, plan.OperationID) != "pending" || !reflect.DeepEqual(before, objectCollectionEvents(t, f, s)) {
			t.Fatal("invalid guest attribution changed collection authority", invalid.name, err)
		}
	}
	for _, kind := range []string{objectAssetBlob, objectWorkspacePart, objectSandboxInput} {
		if _, err := objectCollectionEvent(objectCollectionRunContext(guest, bound), s, kind, output.SHA256, plan.OperationID, "object.collection_superseded", ""); !errors.Is(err, ErrAudit) {
			t.Fatal("output capability could attribute a different physical object kind", kind, err)
		}
	}
	// The positive control uses the public storage API and its actual output
	// capability. Production must supply the new authenticated run binding.
	sealed, err := f.c.SealSandboxOutput(t.Context(), next.Capabilities.Output, output, strings.NewReader(value))
	if err != nil || !sealed.Output.Sealed || fault.putCalls != 1 {
		t.Fatal("valid guest output did not reach its actual Put", fault.putCalls, err)
	}
	after := objectCollectionEvents(t, f, s)
	if len(after) != len(before)+1 {
		t.Fatal("guest output did not retire exactly one old intent", after)
	}
	event := after[len(after)-1]
	if event.Action != "object.collection_superseded" || event.OperationID != plan.OperationID || event.ObjectKind != objectSandboxOutput ||
		event.ObjectIDSHA256 != plan.ObjectIDSHA256 || event.CollectionProof != "" || event.Digest != "" ||
		event.Actor != (AuditActor{Source: "guest-capability", Role: "output"}) ||
		event.RunID != next.Run.ID || event.AttemptSHA256 != auditHash("attempt", b.Attempt) || event.RunID == permit.Run.ID ||
		objectCollectionState(t, f.c, plan.OperationID) != "superseded" {
		t.Fatal("guest supersede lost its authenticated new-run provenance", event)
	}
	objectCollectionExists(t, f.objects, fault.key, true)
	for _, stored := range auditEvents(t, f.r, s) {
		raw, err := json.Marshal(stored)
		if err != nil || strings.Contains(string(raw), next.Capabilities.Output) || strings.Contains(string(raw), "sbx1.") ||
			strings.Contains(string(raw), auditHash("credential", next.Capabilities.Output)) || strings.Contains(string(raw), b.Attempt) {
			t.Fatal("guest credential or raw attempt escaped into stored audit", err)
		}
	}
}

func TestObjectCollectionPreservesExistingReferenceSelectors(t *testing.T) {
	t.Run("historical-artifact-and-skill", func(t *testing.T) {
		f := newAuditFixture(t)
		s := scope("collection-history")
		checkHistoricalReferencesSurviveGC(t, f.c, f.objects, s)
		objectCollectionRequirePair(t, objectCollectionEvents(t, f, s), objectAssetBlob, change("unreferenced upload", 2).SHA256, "remove_ack")
	})
	t.Run("historical-and-pending-parts", func(t *testing.T) {
		f := newAuditFixture(t)
		s := scope("collection-part-refs")
		checkTarPartsCollection(t, f.c, s)
		objectCollectionRequirePair(t, objectCollectionEvents(t, f, s), objectWorkspacePart, change("orphan body", 0).SHA256, "remove_ack")
	})
	t.Run("active-retained-and-shared-sandbox-blobs", func(t *testing.T) {
		f := newAuditFixture(t)
		s := scope("collection-sandbox-refs")
		checkSandboxStorage(t, f.c, s)
		s.Runtime += "-pins-shared"
		events := objectCollectionEvents(t, f, s)
		if len(events) != 4 {
			t.Fatal("shared private input was deleted per run instead of once per blob", events)
		}
		objectCollectionRequirePair(t, events[:2], objectSandboxOutput, change("first output", 0).SHA256, "remove_ack")
		objectCollectionRequirePair(t, events[2:], objectSandboxInput, change("shared input", 0).SHA256, "remove_ack")
	})
}

func TestObjectCollectionPublicGCUsesTwentyFourHourRetention(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("collection-http-retention")
	old := change("old unreferenced upload", 0)
	recent := change("recent unreferenced upload", 0)
	oldTicket := upload(t, f.c, s, old, "old unreferenced upload")
	recentTicket := upload(t, f.c, s, recent, "recent unreferenced upload")
	for _, item := range []struct {
		op  string
		age time.Duration
	}{{oldTicket.ID, 25 * time.Hour}, {recentTicket.ID, time.Hour}} {
		if _, err := f.c.writer.ExecContext(t.Context(), "UPDATE operations SET updated=$1 WHERE scope=$2 AND op=$3", time.Now().Add(-item.age).UnixNano(), s.Prefix(), item.op); err != nil {
			t.Fatal(err)
		}
	}
	secret := bytes.Repeat([]byte{83}, 32)
	now := time.Now()
	token, err := Issue(secret, Grant{Scope: s, Read: true, Collect: true, Expires: now.Add(time.Hour).Unix()}, now)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&API{Catalog: f.c, Secret: secret, Temp: t.TempDir()}).Handler())
	defer server.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/gc", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	var result struct {
		Removed int `json:"removed"`
	}
	if response.StatusCode != http.StatusOK || readErr != nil || closeErr != nil || json.Unmarshal(raw, &result) != nil || result.Removed != 1 {
		t.Fatal("public GC did not apply its actual retention policy", response.StatusCode, string(raw), readErr, closeErr)
	}
	objectCollectionExists(t, f.objects, objectKey(s, old.SHA256), false)
	objectCollectionExists(t, f.objects, objectKey(s, recent.SHA256), true)
	events := objectCollectionEvents(t, f, s)
	objectCollectionRequirePair(t, events, objectAssetBlob, old.SHA256, "remove_ack")
	for _, event := range events {
		if event.Actor != auditActor(auditHTTPContext(t.Context(), token, "gc"), "internal") {
			t.Fatal("actual HTTP GC did not identify the authenticated collector", event.Actor)
		}
		encoded, err := json.Marshal(event)
		if err != nil || strings.Contains(string(encoded), token) {
			t.Fatal("actual HTTP credential escaped into audit", err)
		}
	}
}

func TestObjectCollectionCutoffIsStrictlyOlder(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("collection-exact-cutoff")
	value := "object exactly on the cutoff remains retained"
	ch := change(value, 0)
	ticket := upload(t, f.c, s, ch, value)
	cutoff := time.Now().Add(-24 * time.Hour)
	if _, err := f.c.writer.ExecContext(t.Context(), "UPDATE operations SET updated=$1 WHERE scope=$2 AND op=$3", cutoff.UnixNano(), s.Prefix(), ticket.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := f.c.Collect(t.Context(), s, cutoff); err != nil || n != 0 || len(objectCollectionEvents(t, f, s)) != 0 {
		t.Fatal("equal cutoff was treated as expired", n, err)
	}
	objectCollectionExists(t, f.objects, objectKey(s, ch.SHA256), true)
	if n, err := f.c.Collect(t.Context(), s, cutoff.Add(time.Nanosecond)); err != nil || n != 1 {
		t.Fatal("strictly older positive control did not collect", n, err)
	}
	objectCollectionRequirePair(t, objectCollectionEvents(t, f, s), objectAssetBlob, ch.SHA256, "remove_ack")
}

func TestObjectCollectionNeverUploadedCandidateDoesNotInventDeletion(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("collection-never-uploaded")
	ch := change("candidate metadata does not prove physical bytes existed", 0)
	if _, err := f.c.Begin(t.Context(), s, ch); err != nil {
		t.Fatal(err)
	}
	if n, err := f.c.Collect(t.Context(), s, time.Now().Add(time.Hour)); err != nil || n != 0 || len(objectCollectionEvents(t, f, s)) != 0 {
		t.Fatal("absent candidate invented a physical deletion", n, err)
	}
	var intents int
	if err := f.c.db.QueryRowContext(t.Context(), "SELECT count(*) FROM object_collection_intents WHERE scope=$1", s.Prefix()).Scan(&intents); err != nil || intents != 0 {
		t.Fatal("absent candidate invented a durable Delete plan", intents, err)
	}
}
