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
	checkPostgresAuditAssetLinkLeaseLoss(t, c.objects, dsn)
}

func checkPostgresAuditAssetLinkLeaseLoss(t *testing.T, objects Objects, dsn string) {
	t.Helper()
	// Own database in the disposable service: terminating this writer during
	// link N must not kill the shared integration catalog's lease.
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "agentmoor_audit_links_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.ExecContext(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error("cannot clean owned audit-link database", err)
		}
	}()
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
	defer c.Close()
	reader, err := OpenAuditReader(t.Context(), dir, isolatedDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	s := scope("audit-links-lease-" + name[len(name)-8:])
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
