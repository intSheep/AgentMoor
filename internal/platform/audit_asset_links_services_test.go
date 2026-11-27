//go:build integration

package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentmoor/internal/task"
	"github.com/google/uuid"
)

func checkPostgresAuditAssetLinks(t *testing.T, c *Catalog, dir, dsn string) {
	t.Helper()
	reader, err := OpenAuditReader(t.Context(), dir, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, n := range []int{1, 2, 4} {
		checkAuditAssetLinksRollback(t, c, reader, scope(fmt.Sprint("audit-links-pg-", n)), n, true)
	}
	checkPostgresAuditAssetLinkDeadline(t, c.objects, dsn)
	checkPostgresAuditAssetLinkLeaseLoss(t, c.objects, dsn)
}

func checkPostgresAuditAssetLinkDeadline(t *testing.T, objects Objects, dsn string) {
	t.Helper()
	c, reader, _ := openOwnedAuditLinksPostgres(t, objects, dsn)
	f := prepareAuditLinksDeadline(t, c, reader, scope("audit-link-pg-deadline"))
	threshold := len(assetLinkEvents(f.events)) + 1
	query := fmt.Sprintf("CREATE FUNCTION wait_owned_asset_link_deadline() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.scope='%s' AND NEW.event::jsonb->>'action'='asset.linked' AND (SELECT count(*) FROM audit_events WHERE scope=NEW.scope AND event::jsonb->>'action'='asset.linked')=%d THEN PERFORM pg_sleep(4); END IF; RETURN NEW; END $$; CREATE TRIGGER wait_owned_asset_link_deadline BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION wait_owned_asset_link_deadline()", f.address.Prefix(), threshold)
	if _, err := c.writer.ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := c.db.ExecContext(ctx, "DROP TRIGGER wait_owned_asset_link_deadline ON audit_events; DROP FUNCTION wait_owned_asset_link_deadline()"); err != nil {
			t.Error(err)
		}
	}()
	var writerPID int
	if err := c.writer.QueryRowContext(t.Context(), "SELECT pg_backend_pid()").Scan(&writerPID); err != nil {
		t.Fatal(err)
	}
	f.setDeadline(t, 2*time.Second)
	done := make(chan error, 1)
	go func() {
		var record task.Record
		done <- f.f.worker.JSON(t.Context(), "POST", f.f.route(f.address.turn)+"/finish", f.finish, &record)
	}()
	// Observe the real leased PostgreSQL backend sleeping inside link 2;
	// a mere client timeout before SQL executes does not satisfy this test.
	for until := time.Now().Add(5 * time.Second); ; {
		var wait sql.NullString
		if err := c.db.QueryRowContext(t.Context(), "SELECT wait_event FROM pg_stat_activity WHERE pid=$1", writerPID).Scan(&wait); err != nil {
			t.Fatal(err)
		}
		if wait.String == "PgSleep" {
			break
		}
		select {
		case <-done:
			t.Fatal("Finish did not reach the real PostgreSQL second-link sleep")
		default:
		}
		if time.Now().After(until) {
			t.Fatal("PostgreSQL second-link sleep never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !time.Now().Before(f.before.Deadline) {
		t.Fatal("fixture did not enter PostgreSQL sleep before the original deadline")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("PostgreSQL SQL sleep committed success after the original deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expired PostgreSQL transaction did not finish")
	}
	if time.Now().Before(f.before.Deadline) {
		t.Fatal("Finish failed before the controlled original deadline")
	}
	f.requireRollback(t)
}

func openOwnedAuditLinksPostgres(t *testing.T, objects Objects, dsn string) (*Catalog, *AuditReader, string) {
	t.Helper()
	// Cancellation or backend termination can discard the leased connection.
	// Keep both negative controls off the shared integration Catalog.
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	name := "agentmoor_audit_links_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.ExecContext(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error("cannot clean owned audit-link database", err)
		}
	})
	endpoint, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.Path = "/" + name
	isolatedDSN := endpoint.String()
	dir := t.TempDir()
	c, err := OpenCatalog(t.Context(), dir, isolatedDSN, objects)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	reader, err := OpenAuditReader(t.Context(), dir, isolatedDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	return c, reader, isolatedDSN
}

func checkPostgresAuditAssetLinkLeaseLoss(t *testing.T, objects Objects, dsn string) {
	t.Helper()
	c, reader, isolatedDSN := openOwnedAuditLinksPostgres(t, objects, dsn)
	s := scope("audit-links-lease-" + uuid.NewString()[:8])
	f := newAuditLinksHTTP(t, c, reader, s)
	defer f.server.Close()
	session := "ses_private_lease_loss"
	attempt := f.begin(t, "", session)
	finish, _ := f.receipt(t, "", session, attempt, "prior")
	var record task.Record
	f.json(t, f.worker, "POST", f.route("")+"/finish", finish, &record)
	var oldCP task.Checkpoint
	f.json(t, f.submit, "GET", f.route("")+"/checkpoint", nil, &oldCP)
	turn := uuid.NewString()
	attempt = f.begin(t, turn, session)
	finish, refs := f.receipt(t, turn, session, attempt, "losing-lease")
	before := auditEvents(t, reader, s)
	address := taskAddress{s, turn}
	saved, err := c.readStoredTask(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	var writerPID int
	if err = c.writer.QueryRowContext(t.Context(), "SELECT pg_backend_pid()").Scan(&writerPID); err != nil {
		t.Fatal(err)
	}
	threshold := len(assetLinkEvents(before)) + 1 // After success plus the first link.
	query := fmt.Sprintf("CREATE FUNCTION terminate_owned_link_writer() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.scope='%s' AND NEW.event::jsonb->>'action'='asset.linked' AND (SELECT count(*) FROM audit_events WHERE scope=NEW.scope AND event::jsonb->>'action'='asset.linked')=%d THEN PERFORM pg_terminate_backend(pg_backend_pid()); END IF; RETURN NEW; END $$; CREATE TRIGGER terminate_owned_link_writer BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION terminate_owned_link_writer()", s.Prefix(), threshold)
	if _, err = c.writer.ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	if err = f.worker.JSON(t.Context(), "POST", f.route(turn)+"/finish", finish, &record); err == nil {
		t.Fatal("lost leased connection authorized success")
	}
	for until := time.Now().Add(5 * time.Second); ; {
		var exists bool
		if err = c.db.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)", writerPID).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			break
		}
		if time.Now().After(until) {
			t.Fatal("owned writer did not actually terminate")
		}
		time.Sleep(time.Millisecond)
	}
	after, err := c.readStoredTask(t.Context(), address)
	if err != nil || !reflect.DeepEqual(saved, after) {
		t.Fatal("lease loss changed task", err)
	}
	if !reflect.DeepEqual(before, auditEvents(t, reader, s)) {
		t.Fatal("lease loss committed partial link audit")
	}
	var cp task.Checkpoint
	f.json(t, f.submit, "GET", f.route("")+"/checkpoint", nil, &cp)
	if !reflect.DeepEqual(cp, oldCP) {
		t.Fatal("lease loss advanced checkpoint")
	}
	if err = f.worker.JSON(t.Context(), "POST", f.route(turn)+"/finish", finish, &record); err == nil {
		t.Fatal("dead writer silently fell back to read pool")
	}
	if _, err = c.db.ExecContext(t.Context(), "DROP TRIGGER terminate_owned_link_writer ON audit_events; DROP FUNCTION terminate_owned_link_writer()"); err != nil {
		t.Fatal(err)
	}
	replacement, err := OpenCatalog(t.Context(), t.TempDir(), isolatedDSN, objects)
	if err != nil {
		t.Fatal("fresh owner cannot acquire released database lease", err)
	}
	defer replacement.Close()
	if record, err = replacement.finishTask(t.Context(), address, finish); err != nil || record.State != task.Succeeded {
		t.Fatal("fresh owner cannot complete frozen receipt", err)
	}
	events := auditEvents(t, reader, s)
	requireLinks(t, events, address, attempt, session, "", refs)
	oldJSON, _ := json.Marshal(before)
	newJSON, _ := json.Marshal(events[:len(before)])
	if string(oldJSON) != string(newJSON) {
		t.Fatal("replacement rewrote previous audit history")
	}
	if _, err = replacement.finishTask(t.Context(), address, finish); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, auditEvents(t, reader, s)) {
		t.Fatal("replacement acknowledgement replay duplicated links")
	}
}
