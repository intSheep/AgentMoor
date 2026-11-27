package platform

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/task"
	"github.com/mattn/go-sqlite3"
)

type auditLinksDeadlineFixture struct {
	f       *auditLinksHTTP
	address taskAddress
	finish  task.Finish
	before  taskSlot
	events  []AuditEvent
	cp      task.Checkpoint
}

func prepareAuditLinksDeadline(t *testing.T, c *Catalog, reader *AuditReader, s asset.Scope) auditLinksDeadlineFixture {
	t.Helper()
	f := newAuditLinksHTTP(t, c, reader, s)
	session := "ses_private_original_deadline"
	attempt := f.begin(t, "", session)
	finish, _ := f.receipt(t, "", session, attempt, "base")
	var record task.Record
	f.json(t, f.worker, "POST", f.route("")+"/finish", finish, &record)
	// A previously successful operation remains replayable after its deadline;
	// the new transaction deadline must apply only to a fresh success.
	base, err := c.readStoredTask(t.Context(), taskAddress{Scope: s})
	if err != nil {
		t.Fatal(err)
	}
	base.Deadline = time.Now().UTC().Add(-time.Second)
	raw, err := json.Marshal(base.Record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.writer.ExecContext(t.Context(), "UPDATE tasks SET record=$1 WHERE scope=$2", string(raw), s.Prefix()); err != nil {
		t.Fatal(err)
	}
	previous := auditEvents(t, reader, s)
	f.json(t, f.worker, "POST", f.route("")+"/finish", finish, &record)
	if !record.Deadline.Equal(base.Deadline) || !reflect.DeepEqual(previous, auditEvents(t, reader, s)) {
		t.Fatal("successful replay reset the old deadline or appended audit")
	}
	var cp task.Checkpoint
	f.json(t, f.submit, "GET", f.route("")+"/checkpoint", nil, &cp)
	turn := uuid.NewString()
	attempt = f.begin(t, turn, session)
	finish, _ = f.receipt(t, turn, session, attempt, "deadline")
	return auditLinksDeadlineFixture{f: f, address: taskAddress{s, turn}, finish: finish, events: auditEvents(t, reader, s), cp: cp}
}

func (f *auditLinksDeadlineFixture) setDeadline(t *testing.T, duration time.Duration) {
	t.Helper()
	before, err := f.f.c.readStoredTask(t.Context(), f.address)
	if err != nil {
		t.Fatal(err)
	}
	before.Deadline = time.Now().UTC().Add(duration)
	raw, err := json.Marshal(before.Record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.f.c.writer.ExecContext(t.Context(), "UPDATE turns SET record=$1 WHERE scope=$2 AND id=$3", string(raw), f.address.Prefix(), f.address.turn); err != nil {
		t.Fatal(err)
	}
	f.before = before
}

func (f auditLinksDeadlineFixture) requireRollback(t *testing.T) {
	t.Helper()
	after, err := f.f.c.readStoredTask(t.Context(), f.address)
	if err != nil || !reflect.DeepEqual(f.before, after) {
		t.Fatal("deadline changed the task or refreshed its original deadline", err)
	}
	if !reflect.DeepEqual(f.events, auditEvents(t, f.f.reader, f.address.Scope)) {
		t.Fatal("expired transaction committed partial task/link audit")
	}
	var cp task.Checkpoint
	f.f.json(t, f.f.submit, "GET", f.f.route("")+"/checkpoint", nil, &cp)
	if !reflect.DeepEqual(f.cp, cp) {
		t.Fatal("expired success advanced the old checkpoint")
	}
	var record task.Record
	if err = f.f.worker.JSON(t.Context(), "POST", f.f.route(f.address.turn)+"/finish", f.finish, &record); err == nil {
		t.Fatal("expired Finish retry acquired a fresh deadline")
	}
	if !reflect.DeepEqual(f.events, auditEvents(t, f.f.reader, f.address.Scope)) {
		t.Fatal("expired Finish retry appended links")
	}
}

func TestAuditAssetLinksSQLiteWaitCannotCommitPastOriginalDeadline(t *testing.T) {
	base := newAuditFixture(t)
	f := prepareAuditLinksDeadline(t, base.c, base.r, scope("audit-link-sqlite-deadline"))
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	conn, err := base.c.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	defer unblock()
	base.c.writer = conn // Keep this fixture's function on its actual writer.
	if err := conn.Raw(func(raw any) error {
		return raw.(*sqlite3.SQLiteConn).RegisterFunc("wait_owned_asset_link", func() int {
			close(entered)
			<-release
			return 0
		}, false)
	}); err != nil {
		t.Fatal(err)
	}
	// Block only link 2, after task success plus link 1 were actually inserted.
	if _, err := base.c.writer.ExecContext(t.Context(), "CREATE TRIGGER wait_owned_asset_link BEFORE INSERT ON audit_events WHEN NEW.scope='"+f.address.Prefix()+"' AND json_extract(NEW.event,'$.action')='asset.linked' AND (SELECT count(*) FROM audit_events WHERE scope=NEW.scope AND json_extract(event,'$.action')='asset.linked')=5 BEGIN SELECT wait_owned_asset_link(); END"); err != nil {
		t.Fatal(err)
	}
	f.setDeadline(t, 1500*time.Millisecond)
	done := make(chan error, 1)
	go func() {
		var record task.Record
		done <- f.f.worker.JSON(t.Context(), "POST", f.f.route(f.address.turn)+"/finish", f.finish, &record)
	}()
	select {
	case <-entered:
	case <-done:
		t.Fatal("Finish did not reach the controlled SQLite second-link wait")
	case <-time.After(5 * time.Second):
		t.Fatal("SQLite second-link wait never started")
	}
	if !time.Now().Before(f.before.Deadline) {
		t.Fatal("fixture did not start SQL wait before the original deadline")
	}
	<-time.After(time.Until(f.before.Deadline) + 20*time.Millisecond)
	unblock()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("SQLite SQL wait committed success after the original deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expired SQLite transaction did not finish")
	}
	f.requireRollback(t)
}
