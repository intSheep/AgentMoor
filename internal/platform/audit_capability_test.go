package platform

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
)

func TestAuditCapabilityPreservesAuthenticatedHTTPActor(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("audit-preserved-http")
	p := sandboxTestFence(t, f.c, sandboxTestClaim(t, f.c, s), "input")
	ctx := auditHTTPContext(t.Context(), "synthetic authenticated platform credential", "execute")
	want := auditActor(ctx, "internal")
	if _, err := f.c.ReserveSandboxModel(ctx, p.Capabilities.Model, sandboxModelTestPolicy(), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.SealSandboxOutput(ctx, p.Capabilities.Output, sandboxTestBlob("sealed output"), strings.NewReader("sealed output")); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, event := range auditEvents(t, f.r, s) {
		switch event.Action {
		case "model.reserved", "sandbox.output_declared", "sandbox.output_sealed":
			seen++
			if event.Actor != want {
				t.Fatal("verified platform HTTP credential was replaced by delegated actor", event.Action)
			}
		}
	}
	if seen != 3 {
		t.Fatal("missing delegated-operation positive control", seen)
	}
}

func TestAuditModelCancellationAfterAuthorityRollsBackEventAndBudget(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("audit-authority-cancel")
	p := sandboxTestFence(t, f.c, sandboxTestClaim(t, f.c, s), "input")
	before := len(auditEvents(t, f.r, s))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	conn, err := f.c.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	previous := f.c.writer
	f.c.writer = conn
	defer func() { f.c.writer = previous; conn.Close() }()
	var entered atomic.Bool
	if err = conn.Raw(func(raw any) error {
		return raw.(*sqlite3.SQLiteConn).RegisterFunc("cancel_model_audit", func() int64 {
			entered.Store(true)
			cancel()
			return 0
		}, false)
	}); err != nil {
		t.Fatal(err)
	}
	// The real SQLite callback runs only when appendAudit reaches INSERT,
	// after capability authentication and the tentative budget write. The
	// cancelled context must still roll back both writes before any forward.
	if _, err = conn.ExecContext(t.Context(), `CREATE TRIGGER cancel_model_audit BEFORE INSERT ON audit_events
 BEGIN SELECT cancel_model_audit(); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.c.ReserveSandboxModel(ctx, p.Capabilities.Model, sandboxModelTestPolicy(), 2); err == nil {
		t.Fatal("cancelled authority returned a model reservation")
	}
	if !entered.Load() {
		t.Fatal("cancellation happened before the audit insertion boundary")
	}
	if _, err = f.c.readSandboxModelBudget(t.Context(), p.Run); !errors.Is(err, ErrMissing) {
		t.Fatal("cancelled reservation committed budget", err)
	}
	requireAuditCount(t, f, s, before)
	// SQLite may discard the interrupted connection. The catalog's real
	// writer remains its file-lease-protected pool; restore that test fixture
	// before checking a subsequent request.
	f.c.writer = previous
	if _, err = f.c.db.ExecContext(t.Context(), "DROP TRIGGER cancel_model_audit"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, sandboxModelTestPolicy(), 2); err != nil {
		t.Fatal("rollback damaged subsequent authorized reservation", err)
	}
	events := auditEvents(t, f.r, s)
	if len(events) != before+1 || events[len(events)-1].Actor != (AuditActor{Source: "guest-capability", Role: "model"}) {
		t.Fatal("positive control did not commit one delegated actor")
	}
}

func TestAuditCapabilityActorCannotUseOtherPurposesOrActions(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("audit-strict-cap-actor")
	p := sandboxTestFence(t, f.c, sandboxTestClaim(t, f.c, s), "input")
	if _, err := f.c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, sandboxModelTestPolicy(), 2); err != nil {
		t.Fatal(err)
	}
	events := auditEvents(t, f.r, s)
	e := events[len(events)-1]
	for _, actor := range []AuditActor{{Source: "guest-capability", Role: "input"}, {Source: "guest-capability", Role: "system"}, {Source: "guest-capability", Role: "output"}, {Source: "guest-capability", Role: "model", CredentialSHA256: sandboxTestProfile}, {Source: "guest-Capability", Role: "model"}} {
		changed := e
		changed.Actor = actor
		if changed.valid() {
			t.Fatal("invalid delegated actor was accepted", actor.Source, actor.Role)
		}
	}
	e.Action = "sandbox.deleted"
	e.Component = "sandbox-controller"
	e.State = SandboxReady
	if e.valid() {
		t.Fatal("delegated model actor could impersonate controller deletion")
	}
}
