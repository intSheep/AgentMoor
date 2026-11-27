//go:build integration

package platform

import (
	"testing"

	"github.com/intSheep/AgentMoor/internal/task"
)

func checkSandboxProvenancePostgres(t *testing.T, c *Catalog, dir, dsn string) {
	t.Helper()
	r, err := OpenAuditReader(t.Context(), dir, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	previous := c.objects
	defer func() { c.objects = previous }()
	f := newProvenanceFixtureWithCatalog(t, auditFixture{c: c, r: r, dir: dir}, scope("provenance-postgres"))
	before := len(auditEvents(t, r, f.address.Scope))
	if _, err = c.writer.ExecContext(t.Context(), `CREATE FUNCTION reject_accepted_result() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.event LIKE '%"action":"sandbox.result_accepted"%' THEN RAISE EXCEPTION 'synthetic accepted result rejection'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_accepted_result BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_accepted_result()`); err != nil {
		t.Fatal(err)
	}
	drop := func() {
		if _, err = c.writer.ExecContext(t.Context(), "DROP TRIGGER IF EXISTS reject_accepted_result ON audit_events; DROP FUNCTION IF EXISTS reject_accepted_result()"); err != nil {
			t.Error(err)
		}
	}
	defer drop()
	var row task.Record
	if err = f.client.JSON(t.Context(), "POST", f.route(), f.finish, &row); err == nil {
		t.Fatal("PostgreSQL audit rejection committed isolated success")
	}
	stored, err := c.readStoredTask(t.Context(), taskAddress{Scope: f.address.Scope})
	if err != nil || stored.State != task.Running || len(stored.Assets) != 0 || len(auditEvents(t, r, f.address.Scope)) != before {
		t.Fatal("PostgreSQL accepted-result audit did not roll back whole receipt", err)
	}
	drop()
	if err = f.client.JSON(t.Context(), "POST", f.route(), f.finish, &row); err != nil || row.State != task.Succeeded {
		t.Fatal("same PostgreSQL receipt could not retry after rollback", err)
	}
	events := auditEvents(t, r, f.address.Scope)
	accepted := provenanceAccepted(events)
	if len(accepted) != 1 || len(events) != before+6 || len(assetLinkEvents(events)) != 4 || accepted[0].RunID != f.run.ID || accepted[0].Digest != f.run.Output.SHA256 {
		t.Fatal("PostgreSQL accepted Run and all refs were not committed together")
	}
	if err = f.client.JSON(t.Context(), "POST", f.route(), f.finish, &row); err != nil || len(auditEvents(t, r, f.address.Scope)) != len(events) {
		t.Fatal("PostgreSQL success ACK duplicated provenance", err)
	}
}

// Called around the service suite's actual pg_terminate_backend of its leased
// writer, so no extra service owner or pooled-writer fallback is introduced.
func prepareSandboxProvenanceLeaseFailure(t *testing.T, c *Catalog, dir, dsn string) func() {
	t.Helper()
	r, err := OpenAuditReader(t.Context(), dir, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	previous := c.objects
	f := newProvenanceFixtureWithCatalog(t, auditFixture{c: c, r: r, dir: dir}, scope("provenance-postgres-lease"))
	c.objects = previous
	before := len(auditEvents(t, r, f.address.Scope))
	return func() {
		var row task.Record
		if err := f.client.JSON(t.Context(), "POST", f.route(), f.finish, &row); err == nil {
			t.Fatal("lost PostgreSQL lease accepted isolated result")
		}
		stored, err := c.readStoredTask(t.Context(), taskAddress{Scope: f.address.Scope})
		if err != nil || stored.State != task.Running || len(stored.Assets) != 0 || len(auditEvents(t, r, f.address.Scope)) != before {
			t.Fatal("lost lease changed receipt, links or provenance", err)
		}
	}
}
