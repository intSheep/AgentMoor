//go:build integration

package platform

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"agentmoor/internal/task"
)

func TestPostgresAndS3(t *testing.T) {
	endpoint, dsn := os.Getenv("AGENTMOOR_TEST_S3"), os.Getenv("AGENTMOOR_TEST_PG")
	if endpoint == "" || dsn == "" {
		t.Fatal("use sh tests/services.sh")
	}
	objects, err := NewS3(endpoint, "agentmoor-test", "disposable-test", "disposable-test-password", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Minute); ; {
		err = objects.client.MakeBucket(t.Context(), objects.bucket, minio.MakeBucketOptions{})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
	}
	dir := t.TempDir()
	c, err := OpenCatalog(t.Context(), dir, dsn, objects)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.Close() }()
	checkTarPartsStorage(t, c, scope("integration-parts"))
	checkTarPartsCollection(t, c, scope("integration-parts-gc"))
	checkTurnQueue(t, c, scope("followup"))
	checkHistoricalReferencesSurviveGC(t, c, objects, scope("history-integration"))
	s := scope("integration")
	spec := task.Spec{Instruction: "summarize rows", Input: "one,two", Output: "summary.txt"}
	if _, err := c.submitTask(t.Context(), taskAddress{Scope: s}, spec); err != nil {
		t.Fatal(err)
	}
	cancelled, err := c.cancelTask(t.Context(), taskAddress{Scope: s})
	if err != nil || cancelled.State != task.Cancelled {
		t.Fatal(cancelled, err)
	}
	ch := change("persisted", 0)
	ticket := upload(t, c, s, ch, "persisted")
	if _, err = c.Commit(t.Context(), s, ticket.ID); err != nil {
		t.Fatal(err)
	}
	revision, body, err := c.Read(t.Context(), s, ch.Key, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(body)
	body.Close()
	if err != nil || string(b) != "persisted" || revision.Version != 1 {
		t.Fatal(string(b), revision, err)
	}
	abandoned := change("expired", 1)
	upload(t, c, s, abandoned, "expired")
	if n, err := c.Collect(t.Context(), s, time.Now().Add(time.Second)); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	checkSandboxStorage(t, c, scope("sandbox-integration"))
	checkSandboxPreparedInput(t, c, scope("sandbox-input-integration"))
	checkSandboxModelBudget(t, c, scope("model-budget-integration"))
	checkPostgresAudit(t, c, dir, dsn)
	checkPostgresAuditAssetLinks(t, c, dir, dsn)
	c = checkObjectCollectionAuditServices(t, c, objects, dir, dsn)
	checkModelRestart := prepareSandboxModelRestart(t, c, scope("model-budget-integration-restart"))
	// Exercise actual PostgreSQL persistence and S3 bytes across service restart.
	a := sandboxTestClaim(t, c, scope("sandbox-integration-restart"))
	p := sandboxTestFence(t, c, a, "frozen input")
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = OpenCatalog(t.Context(), dir, dsn, objects)
	if err != nil {
		t.Fatal(err)
	}
	checkModelRestart(c)
	q, err := c.FenceSandboxCreate(t.Context(), a, p.Run.Deadline)
	if err != nil || q.CreateAllowed || q.Capabilities != (SandboxCapabilities{}) || q.Run.PodName != p.Run.PodName {
		t.Fatal("PostgreSQL restart authorized a second create", q, err)
	}
	blob, stream, err := c.ReadSandboxInput(t.Context(), p.Capabilities.Input)
	sandboxTestRead(t, blob, stream, err, "frozen input")
	checkPostgresTaskReaderIsolation(t, c)
	checkSandboxLostLease := prepareSandboxLeaseFailure(t, c)
	checkModelLostLease := prepareSandboxModelLeaseFailure(t, c)
	var auditBeforeLeaseLoss int64
	checkSandboxFinalSealFailure(t, c, func() {
		if err := c.db.QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events").Scan(&auditBeforeLeaseLoss); err != nil {
			t.Fatal(err)
		}
		terminatePostgresWriter(t, c)
	})
	checkPostgresMutationDeniedAfterLeaseLoss(t, c)
	checkSandboxLostLease()
	checkModelLostLease()
	var auditAfterLeaseLoss int64
	if err := c.db.QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events").Scan(&auditAfterLeaseLoss); err != nil || auditAfterLeaseLoss != auditBeforeLeaseLoss {
		t.Fatal("lost writer lease changed committed audit", auditBeforeLeaseLoss, auditAfterLeaseLoss, err)
	}
}

func checkPostgresTaskReaderIsolation(t *testing.T, c *Catalog) {
	t.Helper()
	a := taskAddress{Scope: scope("reader-isolation")}
	spec := task.Spec{Instruction: "read concurrently", Input: "one", Output: "report.json"}
	if _, err := c.submitTask(t.Context(), a, spec); err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		readers.Go(func() {
			<-start
			for range 1000 {
				row, err := c.readTask(t.Context(), a)
				if err != nil || row.Spec != spec {
					t.Errorf("concurrent task read: %+v %v", row, err)
					return
				}
			}
		})
	}
	close(start)
	readers.Wait()
	if t.Failed() {
		return
	}

	secret := bytes.Repeat([]byte{7}, 32)
	token, err := Issue(secret, Grant{Scope: a.Scope, Read: true, Submit: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	api := (&API{Catalog: c, Secret: secret, Temp: t.TempDir(), TaskAdapters: map[string]bool{"codex": true}}).Handler()
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			defer close(finished)
		}
		api.ServeHTTP(w, r)
	}))
	defer server.Close()
	blocker, err := c.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err = blocker.ExecContext(t.Context(), "LOCK TABLE tasks IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	route := server.URL + "/v1/tasks/" + a.Runtime
	request, err := http.NewRequestWithContext(ctx, "GET", route, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	returned := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		returned <- err
	}()
	// Cancel only once PostgreSQL confirms the HTTP read is actually in SQL.
	// Cancelling before that point would not exercise pq's connection cleanup.
	waiting := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		err = c.db.QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
 WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT record,attempt FROM tasks%')`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !waiting {
		t.Fatal("HTTP task read never entered the PostgreSQL lock wait")
	}
	cancel()
	select {
	case err = <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("HTTP read was not cancelled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP read did not stop after cancellation")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled SQL read did not finish")
	}
	if err = blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if row, err := c.readTask(t.Context(), a); err != nil || row.Spec != spec {
		t.Fatal("cancelled reader damaged subsequent reads", row, err)
	}
	spec.Input = "different input"
	body, _ := json.Marshal(spec)
	request, err = http.NewRequestWithContext(t.Context(), "PUT", route, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatal("conflicting submission after read cancellation", response.StatusCode)
	}
	// The conflict path only reads. A fresh mutation must also still succeed,
	// proving that cancellation preserved the writer connection and its lease.
	fresh := a
	fresh.Runtime = "after-cancelled-read"
	if _, err := c.submitTask(t.Context(), fresh, spec); err != nil {
		t.Fatal("cancelled reader damaged the leased writer", err)
	}
}

func terminatePostgresWriter(t *testing.T, c *Catalog) {
	t.Helper()
	var pid int
	if err := c.writer.QueryRowContext(t.Context(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err := c.db.QueryRowContext(t.Context(), "SELECT pg_terminate_backend($1)", pid).Scan(&terminated); err != nil || !terminated {
		t.Fatal("could not terminate the owned writer connection", terminated, err)
	}
	// pg_terminate_backend acknowledges delivery of the signal. Wait until the
	// session has actually gone before asserting that a fresh write fails.
	for deadline := time.Now().Add(5 * time.Second); ; {
		var exists bool
		if err := c.db.QueryRowContext(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid=$1)", pid).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminated writer session remained active")
		}
		time.Sleep(time.Millisecond)
	}
}

func checkPostgresMutationDeniedAfterLeaseLoss(t *testing.T, c *Catalog) {
	t.Helper()
	a := taskAddress{Scope: scope("lost-lease")}
	if _, err := c.submitTask(t.Context(), a, task.Spec{Instruction: "must not be saved", Output: "report.json"}); err == nil {
		t.Fatal("mutation replaced the lost lease with an ordinary pooled writer")
	}
	var saved int
	if err := c.db.QueryRowContext(t.Context(), "SELECT count(*) FROM tasks WHERE scope=$1", a.Prefix()).Scan(&saved); err != nil || saved != 0 {
		t.Fatal("lease loss published task state", saved, err)
	}
	if err := c.db.QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events WHERE scope=$1", a.Prefix()).Scan(&saved); err != nil || saved != 0 {
		t.Fatal("lease loss published an audit event without its business state", saved, err)
	}
}

func checkPostgresAudit(t *testing.T, c *Catalog, dir, dsn string) {
	t.Helper()
	s := scope("audit-integration")
	reader, err := OpenAuditReader(t.Context(), dir, dsn)
	if err != nil {
		t.Fatal("read-only audit connection competed with live lease", err)
	}
	defer reader.Close()
	if _, err = c.writer.ExecContext(t.Context(), `CREATE FUNCTION reject_audit_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic audit rejection'; END $$;
CREATE TRIGGER reject_audit_insert BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_audit_insert()`); err != nil {
		t.Fatal(err)
	}
	drop := func() {
		if _, err = c.writer.ExecContext(t.Context(), "DROP TRIGGER reject_audit_insert ON audit_events; DROP FUNCTION reject_audit_insert()"); err != nil {
			t.Fatal(err)
		}
	}
	a := taskAddress{Scope: s}
	spec := task.Spec{Instruction: "synthetic private content must not enter audit", Output: "report.txt"}
	if _, err = c.submitTask(t.Context(), a, spec); err == nil {
		drop()
		t.Fatal("PostgreSQL audit rejection committed task")
	}
	if _, err = c.readTask(t.Context(), a); !errors.Is(err, ErrMissing) {
		drop()
		t.Fatal("PostgreSQL audit rejection did not roll back task", err)
	}
	page, err := reader.Page(t.Context(), s, 0, 0)
	if err != nil || page.Total != 0 || len(page.Events) != 0 {
		drop()
		t.Fatal("failed PostgreSQL transaction left audit", page, err)
	}
	drop()
	for range 2 {
		if _, err = c.submitTask(t.Context(), a, spec); err != nil {
			t.Fatal(err)
		}
	}
	page, err = reader.Page(t.Context(), s, 0, 1)
	if err != nil || page.Total != 1 || len(page.Events) != 1 || page.Events[0].Action != "task.queued" {
		t.Fatal("PostgreSQL audit ACK retry duplicated event", page, err)
	}
	tx, err := reader.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), "DELETE FROM audit_events WHERE scope=$1", s.Prefix()); err == nil {
		tx.Rollback()
		t.Fatal("PostgreSQL read-only audit transaction allowed mutation")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}
