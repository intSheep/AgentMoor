//go:build integration

package platform

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func sandboxGuestStopServiceFixture(t *testing.T, c *Catalog, name string) (SandboxCreatePermit, SandboxGuestStopProof) {
	t.Helper()
	a := sandboxTestClaim(t, c, scope(name))
	p := sandboxTestFence(t, c, a, "synthetic private stop-proof input")
	uid, code := uuid.NewString(), 143
	if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
		t.Fatal(err)
	}
	return p, SandboxGuestStopProof{Kind: SandboxGuestTerminated, PodUID: uid, ResourceVersion: "postgres-first-observation", NodeName: "fixture-node", ContainerID: "containerd://" + sandboxTestBlob(uid).SHA256, ContainerName: "guest", ExitCode: &code}
}

func checkSandboxGuestStopPostgres(t *testing.T, c *Catalog, dir, dsn string) func(*Catalog) {
	t.Helper()
	r, err := OpenAuditReader(t.Context(), dir, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	p, proof := sandboxGuestStopServiceFixture(t, c, "stop-proof-postgres")
	a := p.Run.Address()
	before, err := c.loadSandbox(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	events := auditEvents(t, r, a.Scope)
	if _, err := c.writer.ExecContext(t.Context(), `CREATE FUNCTION reject_guest_stop_proof() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.event LIKE '%"action":"sandbox.guest_stop_proven"%' THEN RAISE EXCEPTION 'synthetic stop-proof audit failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_guest_stop_proof BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_guest_stop_proof()`); err != nil {
		t.Fatal(err)
	}
	installed := true
	drop := func() {
		if installed {
			if _, err := c.writer.ExecContext(t.Context(), "DROP TRIGGER IF EXISTS reject_guest_stop_proof ON audit_events; DROP FUNCTION IF EXISTS reject_guest_stop_proof()"); err != nil {
				t.Error(err)
			}
			installed = false
		}
	}
	defer drop()
	if _, err := c.RecordSandboxGuestStopped(t.Context(), a, proof); err == nil {
		t.Fatal("PostgreSQL audit rejection committed a stop proof")
	}
	after, err := c.loadSandbox(t.Context(), a)
	if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(events, auditEvents(t, r, a.Scope)) {
		t.Fatal("rejected stop proof partially changed runtime binding or audit", err)
	}
	for token, purpose := range map[string]SandboxPurpose{p.Capabilities.Input: SandboxInput, p.Capabilities.Output: SandboxOutput, p.Capabilities.Model: SandboxModel} {
		if _, err := c.AuthenticateSandboxCapability(t.Context(), token, purpose); err != nil {
			t.Fatal("failed proof transaction revoked a capability", err)
		}
	}
	drop()
	stored, err := c.RecordSandboxGuestStopped(t.Context(), a, proof)
	if err != nil || !stored.GuestStopped() {
		t.Fatal("same PostgreSQL proof could not retry", err)
	}
	current := auditEvents(t, r, a.Scope)
	if len(current) != len(events)+1 || current[len(current)-1].Action != "sandbox.guest_stop_proven" || current[len(current)-1].PodUID != proof.PodUID {
		t.Fatal("proof and one exact Pod audit did not commit together")
	}
	raw, _ := json.Marshal(current)
	for _, secret := range []string{p.Capabilities.Input, p.Capabilities.Output, p.Capabilities.Model, a.Attempt, "synthetic private stop-proof input"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("stop-proof audit retained a capability, attempt or input body")
		}
	}
	again := proof
	again.ResourceVersion = "postgres-later-observation"
	if _, err := c.RecordSandboxGuestStopped(t.Context(), a, again); err != nil || len(auditEvents(t, r, a.Scope)) != len(current) {
		t.Fatal("same proof replay duplicated logical audit", err)
	}
	return func(reopened *Catalog) {
		got, err := reopened.loadSandbox(t.Context(), a)
		if err != nil || !got.GuestStopped() || !reflect.DeepEqual(stored.GuestStopProof, got.GuestStopProof) {
			t.Fatal("actual PostgreSQL restart lost or rewrote stop proof", err)
		}
		for token, purpose := range map[string]SandboxPurpose{p.Capabilities.Input: SandboxInput, p.Capabilities.Output: SandboxOutput, p.Capabilities.Model: SandboxModel} {
			if _, err := reopened.AuthenticateSandboxCapability(t.Context(), token, purpose); err == nil {
				t.Fatal("restart revived stopped guest authority")
			}
		}
		if len(auditEvents(t, r, a.Scope)) != len(current) {
			t.Fatal("restart or reads duplicated stop-proof audit")
		}
	}
}

func prepareSandboxGuestStopLeaseFailure(t *testing.T, c *Catalog, dir, dsn string) func() {
	t.Helper()
	r, err := OpenAuditReader(t.Context(), dir, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	p, proof := sandboxGuestStopServiceFixture(t, c, "stop-proof-lease-failure")
	a := p.Run.Address()
	before, err := c.loadSandbox(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	count := len(auditEvents(t, r, a.Scope))
	return func() {
		if _, err := c.RecordSandboxGuestStopped(t.Context(), a, proof); err == nil {
			t.Fatal("terminated leased writer committed stop proof")
		}
		after, err := c.loadSandbox(t.Context(), a)
		if err != nil || !reflect.DeepEqual(before, after) || len(auditEvents(t, r, a.Scope)) != count {
			t.Fatal("lost lease changed proof, runtime binding or audit", err)
		}
		var revoked int
		if err := c.db.QueryRowContext(t.Context(), "SELECT COALESCE(sum(revoked),0) FROM sandbox_caps WHERE scope=$1 AND run_id=$2", a.Scope.Prefix(), p.Run.ID).Scan(&revoked); err != nil || revoked != 0 {
			t.Fatal("lost proof transaction partially revoked capabilities", revoked, err)
		}
	}
}
