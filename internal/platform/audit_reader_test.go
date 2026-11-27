package platform

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/task"
)

func TestAuditReaderScopePaginationAndLimits(t *testing.T) {
	f := newAuditFixture(t)
	a, b := scope("audit-page-a"), scope("audit-page-b")
	for _, s := range []asset.Scope{a, b} {
		if _, err := f.c.submitTask(t.Context(), taskAddress{Scope: s}, task.Spec{Instruction: "synthetic", Output: "report.txt"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []asset.Scope{a, b} {
		if _, err := f.c.claimTask(t.Context(), taskAddress{Scope: s}, task.Claim{Attempt: uuid.NewString()}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := f.r.Page(t.Context(), a, 0, 1)
	if err != nil || len(first.Events) != 1 || first.Total != 2 || first.Next != first.Events[0].Cursor {
		t.Fatal("first scoped page", first, err)
	}
	second, err := f.r.Page(t.Context(), a, first.Next, 1)
	if err != nil || len(second.Events) != 1 || second.Total != 2 || second.Next <= first.Next || second.Events[0].Scope != a {
		t.Fatal("second scoped page", second, err)
	}
	last, err := f.r.Page(t.Context(), a, second.Next, 1)
	if err != nil || len(last.Events) != 0 || last.Total != 2 || last.Next != second.Next {
		t.Fatal("empty scoped continuation", last, err)
	}
	for _, e := range append(first.Events, second.Events...) {
		if e.Scope != a {
			t.Fatal("cross-scope audit row")
		}
	}
	for _, limit := range []int{-1, AuditMaxLimit + 1} {
		if _, err := f.r.Page(t.Context(), a, 0, limit); !errors.Is(err, ErrAudit) {
			t.Fatal("invalid limit accepted", limit, err)
		}
	}
	if _, err := f.r.Page(t.Context(), a, -1, 1); !errors.Is(err, ErrAudit) {
		t.Fatal("negative cursor accepted", err)
	}
	if _, err := f.r.Page(t.Context(), asset.Scope{}, 0, 1); !errors.Is(err, ErrAudit) {
		t.Fatal("invalid scope accepted", err)
	}
	if _, err := (*AuditReader)(nil).Page(t.Context(), a, 0, 1); !errors.Is(err, ErrAudit) {
		t.Fatal("nil reader accepted", err)
	}

	s := scope("audit-default-limit")
	p := sandboxTestFence(t, f.c, sandboxTestClaim(t, f.c, s), "sealed fixture")
	policy := SandboxModelPolicy{Fingerprint: sandboxTestProfile, MaxRequests: 128, MaxInputBytes: 128, MaxOutputBytes: 128, PerRequestOutputBytes: 1}
	for range 101 {
		if _, err := f.c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 1); err != nil {
			t.Fatal(err)
		}
	}
	page, err := f.r.Page(t.Context(), s, 0, 0)
	if err != nil || len(page.Events) != 100 || page.Total <= 100 {
		t.Fatal("zero limit is not the bounded default 100", len(page.Events), page.Total, err)
	}
	tail, err := f.r.Page(t.Context(), s, page.Next, AuditMaxLimit)
	if err != nil || len(page.Events)+len(tail.Events) != int(page.Total) || tail.Total != page.Total {
		t.Fatal("default pagination skipped or duplicated events", err)
	}
}

func TestAuditReaderRunsAlongsideLiveCatalogWrites(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("audit-live-reader")
	p := sandboxTestFence(t, f.c, sandboxTestClaim(t, f.c, s), "sealed fixture")
	base := len(auditEvents(t, f.r, s))
	policy := SandboxModelPolicy{Fingerprint: sandboxTestProfile, MaxRequests: 64, MaxInputBytes: 64, MaxOutputBytes: 64, PerRequestOutputBytes: 1}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		for range 40 {
			if _, err := f.c.ReserveSandboxModel(ctx, p.Capabilities.Model, policy, 1); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for {
		page, err := f.r.Page(ctx, s, 0, AuditMaxLimit)
		if err != nil || page.Total != int64(len(page.Events)) {
			t.Fatal("reader did not retain a consistent transaction snapshot", err)
		}
		var previous int64
		for _, event := range page.Events {
			if event.Cursor <= previous || event.Scope != s {
				t.Fatal("unordered or foreign live audit row")
			}
			previous = event.Cursor
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal("readonly reader blocked the leased writer", err)
			}
			requireAuditCount(t, f, s, base+40)
			return
		case <-ctx.Done():
			t.Fatal("live writer/reader did not finish", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func TestAuditReaderDoesNotCreateMissingDatabase(t *testing.T) {
	parent := t.TempDir()
	for _, dir := range []string{filepath.Join(parent, "missing-directory"), filepath.Join(parent, "empty-directory")} {
		if strings.Contains(dir, "empty-directory") {
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
		}
		reader, err := OpenAuditReader(t.Context(), dir, "")
		if reader != nil {
			reader.Close()
		}
		if !errors.Is(err, ErrAudit) {
			t.Fatal("missing database was opened or error leaked", err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "catalog.sqlite")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("readonly open created a database", err)
		}
	}
	if _, err := os.Lstat(filepath.Join(parent, "missing-directory")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("readonly open created a directory", err)
	}
	if reader, err := OpenAuditReader(t.Context(), "relative", ""); reader != nil || !errors.Is(err, ErrAudit) {
		t.Fatal("relative audit directory accepted")
	}
}

func TestAuditReaderSupportsLiteralQueryAndFragmentDirectoryNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit ? mode#literal")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c, err := OpenCatalog(t.Context(), dir, "", DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal("catalog cannot use a literal special-character directory", err)
	}
	defer c.Close()
	s := scope("audit-literal-path")
	if _, err := c.submitTask(t.Context(), taskAddress{Scope: s}, task.Spec{Instruction: "synthetic", Output: "report.txt"}); err != nil {
		t.Fatal(err)
	}
	r, err := OpenAuditReader(t.Context(), dir, "")
	if err != nil {
		t.Fatal("reader cannot find the catalog in a literal special-character directory", err)
	}
	defer r.Close()
	if events := auditEvents(t, r, s); len(events) != 1 {
		t.Fatal("wrong literal-path database read")
	}
}

func TestAuditReaderRejectsMalformedStoredEvents(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"unknown":   func(raw string) string { return strings.TrimSuffix(raw, "}") + `,"unexpected":1}` },
		"alias":     func(raw string) string { return strings.Replace(raw, `"action":`, `"Action":`, 1) },
		"duplicate": func(raw string) string { return strings.TrimSuffix(raw, "}") + `,"action":"task.queued"}` },
		"null":      func(raw string) string { return strings.TrimSuffix(raw, "}") + `,"turn_id":null}` },
		"deep": func(raw string) string {
			return strings.TrimSuffix(raw, "}") + `,"unexpected":` + strings.Repeat("[", 20) + "0" + strings.Repeat("]", 20) + "}"
		},
		"oversize": func(raw string) string {
			return strings.TrimSuffix(raw, "}") + `,"unexpected":"` + strings.Repeat("x", auditMaxEventBytes) + `"}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAuditFixture(t)
			s := scope("audit-malformed")
			if _, err := f.c.submitTask(t.Context(), taskAddress{Scope: s}, task.Spec{Instruction: "synthetic", Output: "report.txt"}); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := f.c.db.QueryRowContext(t.Context(), "SELECT event FROM audit_events WHERE scope=$1", s.Prefix()).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if _, err := f.c.db.ExecContext(t.Context(), "UPDATE audit_events SET event=$1 WHERE scope=$2", mutate(raw), s.Prefix()); err != nil {
				t.Fatal(err)
			}
			if page, err := f.r.Page(t.Context(), s, 0, 1); !errors.Is(err, ErrAudit) || len(page.Events) != 0 {
				t.Fatal("invalid stored event was returned", err)
			}
		})
	}
}

func TestAuditCursorExhaustionRollsBackBusinessMutation(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("audit-cursor-overflow")
	if _, err := f.c.db.ExecContext(t.Context(), "INSERT INTO audit_events(cursor,scope,event) VALUES($1,$2,$3)", int64(math.MaxInt64), s.Prefix(), "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.submitTask(t.Context(), taskAddress{Scope: s}, task.Spec{Instruction: "synthetic", Output: "report.txt"}); !errors.Is(err, ErrAudit) {
		t.Fatal("exhausted audit cursor accepted a business mutation", err)
	}
	if _, err := f.c.readStoredTask(t.Context(), taskAddress{Scope: s}); !errors.Is(err, ErrMissing) {
		t.Fatal("cursor exhaustion committed the business write", err)
	}
	var count int
	if err := f.c.db.QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events").Scan(&count); err != nil || count != 1 {
		t.Fatal("cursor exhaustion changed audit rows", count, err)
	}
}

func TestAuditLegacyCatalogUpgradePreservesBusinessWithoutInventingHistory(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("audit-legacy-upgrade")
	a := sandboxTestClaim(t, f.c, s)
	run := sandboxTestBegin(t, f.c, a, "original sealed-input identity")
	var taskRaw, attempt, sandboxRaw string
	if err := f.c.db.QueryRowContext(t.Context(), "SELECT record,attempt FROM tasks WHERE scope=$1", s.Prefix()).Scan(&taskRaw, &attempt); err != nil {
		t.Fatal(err)
	}
	if err := f.c.db.QueryRowContext(t.Context(), "SELECT record FROM sandbox_runs WHERE scope=$1", s.Prefix()).Scan(&sandboxRaw); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite3", filepath.Join(f.dir, "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.ExecContext(t.Context(), "DROP TABLE audit_events"); err != nil {
		t.Fatal(err)
	}
	if err = legacy.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenAuditReader(t.Context(), f.dir, "")
	if reader != nil {
		reader.Close()
	}
	if !errors.Is(err, ErrAudit) {
		t.Fatal("read-only audit open accepted a legacy database without audit schema", err)
	}
	legacy, err = sql.Open("sqlite3", filepath.Join(f.dir, "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	var tables int
	if err = legacy.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='audit_events'").Scan(&tables); err != nil || tables != 0 {
		t.Fatal("read-only audit open recreated the missing schema", tables, err)
	}
	legacy.Close()
	c, err := OpenCatalog(t.Context(), f.dir, "", f.objects)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var gotTask, gotAttempt, gotSandbox string
	if err = c.db.QueryRowContext(t.Context(), "SELECT record,attempt FROM tasks WHERE scope=$1", s.Prefix()).Scan(&gotTask, &gotAttempt); err != nil {
		t.Fatal(err)
	}
	if err = c.db.QueryRowContext(t.Context(), "SELECT record FROM sandbox_runs WHERE scope=$1", s.Prefix()).Scan(&gotSandbox); err != nil {
		t.Fatal(err)
	}
	loaded, err := c.loadSandbox(t.Context(), a)
	if err != nil || gotTask != taskRaw || gotAttempt != attempt || gotSandbox != sandboxRaw || loaded.RecordFingerprint != run.RecordFingerprint {
		t.Fatal("schema upgrade changed original business bytes, attempt or frozen identity", err)
	}
	r, err := OpenAuditReader(t.Context(), f.dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if events := auditEvents(t, r, s); len(events) != 0 {
		t.Fatal("schema upgrade invented historical audit events")
	}
	for range 2 {
		if _, err := c.cancelTask(t.Context(), taskAddress{Scope: s}); err != nil {
			t.Fatal(err)
		}
	}
	events := auditEvents(t, r, s)
	if len(events) != 1 || events[0].Action != "task.cancel_requested" || events[0].PreviousState != task.Running || events[0].State != task.Cancelling || events[0].Cursor != 1 {
		t.Fatal("legacy transition did not append exactly its new event", events)
	}
}
