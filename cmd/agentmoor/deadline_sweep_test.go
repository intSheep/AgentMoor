package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/platform"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

func deadlineServeFixture(t *testing.T) (string, asset.Scope, task.Record, string, string) {
	t.Helper()
	dir := t.TempDir()
	authCLI(t, "init", "--data", dir)
	catalog, err := platform.OpenCatalog(t.Context(), dir, "", platform.DiskObjects{Dir: filepath.Join(dir, "objects")})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}
	s := asset.Scope{Tenant: "deadline-serve", Project: "synthetic", Runtime: "original-allocation", Adapter: "codex"}
	record := task.Record{ID: s.Runtime, State: task.Running, Deadline: time.Now().UTC().Add(-time.Minute),
		Spec: task.Spec{Instruction: "synthetic input must not become an audit payload", Input: `keep {"state":"running"} inside this text`, Output: "report.json"}}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	attempt := uuid.NewString()
	db := deadlineServeDB(t, dir, false)
	if _, err := db.ExecContext(t.Context(), "INSERT INTO tasks(scope,record,attempt) VALUES($1,$2,$3)", s.Prefix(), string(raw), attempt); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, s, record, string(raw), attempt
}

func deadlineServeDB(t *testing.T, dir string, readOnly bool) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: filepath.Join(dir, "catalog.sqlite")}
	q := url.Values{"mode": {"rw"}, "_busy_timeout": {"5000"}}
	if readOnly {
		q.Set("mode", "ro")
		q.Set("_query_only", "true")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestServeDefaultDeadlineControllerPersistsWithoutHTTPPolling(t *testing.T) {
	dir, s, original, raw, attempt := deadlineServeFixture(t)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	ready, done := make(authReadyWriter, 2), make(chan error, 1)
	returned := false
	go func() { done <- run(ctx, []string{"serve", "--data", dir, "--listen", "127.0.0.1:0"}, ready) }()
	t.Cleanup(func() {
		stop()
		if !returned {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("owned deadline serve did not stop during cleanup")
			}
		}
	})
	select {
	case <-ready:
	case err := <-done:
		returned = true
		t.Fatal("serve exited before starting its default lifecycle", err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not bind")
	}
	reader, err := platform.OpenAuditReader(t.Context(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var page platform.AuditPage
	for until := time.Now().Add(5 * time.Second); ; {
		page, err = reader.Page(t.Context(), s, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) != 0 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("default serve did not persist expiry without any task GET")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(page.Events) != 1 || page.Events[0].Action != "task.interrupted" || page.Events[0].Actor.Source != "deadline-controller" || page.Events[0].Actor.Role != "system" || page.Events[0].Actor.CredentialSHA256 != "" {
		t.Fatal("default lifecycle used a request identity or an unrelated event", page)
	}
	db := deadlineServeDB(t, dir, true)
	var after, savedAttempt string
	if err := db.QueryRowContext(t.Context(), "SELECT record,attempt FROM tasks WHERE scope=$1", s.Prefix()).Scan(&after, &savedAttempt); err != nil {
		t.Fatal(err)
	}
	expected := strings.Replace(raw, `"state":"running"`, `"state":"interrupted"`, 1)
	if after != expected || savedAttempt != attempt {
		t.Fatal("default lifecycle rewrote frozen JSON or private attempt")
	}
	var decoded task.Record
	if err := json.Unmarshal([]byte(after), &decoded); err != nil {
		t.Fatal(err)
	}
	original.State = task.Interrupted
	if !reflect.DeepEqual(original, decoded) {
		t.Fatal("default lifecycle changed more than state")
	}
	stop()
	select {
	case err := <-done:
		returned = true
		if err != nil {
			t.Fatal("serve did not stop its controller cleanly", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not join its deadline controller")
	}
	// The service owns the lease until its controller has stopped; acquiring
	// it immediately after return also catches shutdown order regressions.
	catalog, err := platform.OpenCatalog(t.Context(), dir, "", platform.DiskObjects{Dir: filepath.Join(dir, "objects")})
	if err != nil {
		t.Fatal("serve returned while retaining the catalog lease", err)
	}
	defer catalog.Close()
	result, err := catalog.SweepExpired(t.Context(), platform.DeadlineSweepCursor{})
	if err != nil || result.Interrupted != 0 {
		t.Fatal("restart repeated the successful lifecycle transition", result, err)
	}
	repeated, err := reader.Page(t.Context(), s, 0, 10)
	if err != nil || !reflect.DeepEqual(page, repeated) {
		t.Fatal("restart duplicated or changed the original audit", err)
	}
}

func TestServeBindFailureJoinsDeadlineControllerBeforeCatalogClose(t *testing.T) {
	dir, _, _, _, _ := deadlineServeFixture(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--data", dir, "--listen", occupied.Addr().String()}, make(authReadyWriter, 2))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve ignored a bind failure")
		}
	case <-ctx.Done():
		t.Fatal("failed startup left a deadline lifecycle running")
	}
	catalog, err := platform.OpenCatalog(t.Context(), dir, "", platform.DiskObjects{Dir: filepath.Join(dir, "objects")})
	if err != nil {
		t.Fatal("failed startup did not release its service lease", err)
	}
	catalog.Close()
}
