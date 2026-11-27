//go:build integration

package platform

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"agentmoor/internal/task"
)

// All destructive SQL faults use separate, disposable PostgreSQL databases.
// A cancelled/terminated leased writer must not poison the shared services
// Catalog or silently recover through its pooled reader connections.
func checkPostgresDeadlineSweep(t *testing.T, objects Objects, dsn string) {
	t.Helper()
	checkPostgresDeadlineSweepAuditRollback(t, objects, dsn)
	checkPostgresDeadlineSweepLeaseRollback(t, objects, dsn)
	checkPostgresDeadlineSweepFinishBarrier(t, objects, dsn, false)
	checkPostgresDeadlineSweepFinishBarrier(t, objects, dsn, true)
	checkPostgresDeadlineSweepAvailability(t, objects, dsn)
}

// A slow read must not spend the leased writer's I/O cancellation allowance.
// Both delays are ordinary server work: an observed table-lock wait, followed
// by 200ms inside the interruption audit. No backend is explicitly killed.
func checkPostgresDeadlineSweepAvailability(t *testing.T, objects Objects, dsn string) {
	t.Helper()
	c, reader, _ := openOwnedAuditLinksPostgres(t, objects, dsn)
	row := deadlineSweepFixtureRow(t, scope("deadline-pg-availability"), "", task.Running, time.Now().UTC().Add(-time.Hour))
	deadlineSweepInsert(t, c, row)
	var originalPID int
	if err := c.writer.QueryRowContext(t.Context(), "SELECT pg_backend_pid()").Scan(&originalPID); err != nil {
		t.Fatal(err)
	}
	query := fmt.Sprintf(`CREATE FUNCTION delay_owned_deadline_availability() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.scope='%s' AND NEW.event::jsonb->>'action'='task.interrupted' THEN PERFORM pg_sleep(0.2); END IF; RETURN NEW; END $$;
 CREATE TRIGGER delay_owned_deadline_availability BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION delay_owned_deadline_availability()`, row.address.Prefix())
	if _, err := c.writer.ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	blocker, err := c.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.ExecContext(t.Context(), "LOCK TABLE tasks IN ACCESS EXCLUSIVE MODE"); err != nil {
		blocker.Rollback()
		t.Fatal(err)
	}
	var unlocked sync.Once
	unblock := func() {
		unlocked.Do(func() {
			if err := blocker.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Error("cannot release owned availability table lock", err)
			}
		})
	}
	defer unblock()
	type outcome struct {
		result DeadlineSweepResult
		err    error
	}
	done := make(chan outcome, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	start := time.Now()
	received := false
	t.Cleanup(func() {
		unblock()
		cancel()
		if received {
			return
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned availability sweep did not stop during bounded cleanup")
		}
	})
	go func() {
		result, err := c.SweepExpired(ctx, DeadlineSweepCursor{})
		done <- outcome{result, err}
	}()
	// Confirm that this exact keyset read is really blocked on PostgreSQL,
	// rather than sleeping in the test while no production statement runs.
	for until := time.Now().Add(time.Second); ; {
		var waiting bool
		if err := c.db.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE $1)", "%FROM tasks WHERE scope%").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case value := <-done:
			received = true
			t.Fatal("sweep ended before the real PostgreSQL keyset lock wait", value.result, value.err)
		default:
		}
		if time.Now().After(until) {
			t.Fatal("sweep never entered its real PostgreSQL keyset lock wait")
		}
		time.Sleep(5 * time.Millisecond)
	}
	releaseAt := start.Add(deadlineSweepBudget - 100*time.Millisecond)
	if !time.Now().Before(releaseAt) {
		t.Fatal("availability fixture did not observe the read before its release boundary")
	}
	<-time.After(time.Until(releaseAt))
	unblock()
	releasedAfter := time.Since(start)
	var value outcome
	select {
	case value = <-done:
		received = true
	case <-time.After(5 * time.Second):
		t.Fatal("availability sweep did not return after releasing the table")
	}
	cancel()
	observe, stopObserve := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopObserve()
	var currentPID int
	writerErr := c.writer.QueryRowContext(observe, "SELECT pg_backend_pid()").Scan(&currentPID)
	var backendAlive, leaseHeld bool
	for until := time.Now().Add(time.Second); ; {
		if err := c.db.QueryRowContext(observe, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1),
 EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND granted AND classid=193674738 AND objid=1)`, originalPID).Scan(&backendAlive, &leaseHeld); err != nil {
			t.Fatal(err)
		}
		if writerErr == nil || !backendAlive || time.Now().After(until) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	probeScope := scope("deadline-pg-availability-probe")
	_, submitErr := c.submitTask(observe, taskAddress{Scope: probeScope}, task.Spec{Instruction: "ordinary legal task after a delayed sweep", Output: "reply.txt"})
	// Log all actual liveness evidence before the expected baseline failure.
	// In the old shared-2s implementation the table read spends ~1.9s, then
	// client cancellation inside pg_sleep closes the original leased writer.
	t.Logf("DEADLINE_SWEEP_AVAILABILITY keyset_lock_observed=true read_release_ms=%d sweep_error=%t interrupted=%d writer_query_ok=%t same_writer_pid=%t original_backend_alive=%t original_advisory_lease=%t subsequent_submit_ok=%t", releasedAfter.Milliseconds(), value.err != nil, value.result.Interrupted, writerErr == nil, currentPID == originalPID, backendAlive, leaseHeld, submitErr == nil)
	if writerErr != nil || currentPID != originalPID || !backendAlive || !leaseHeld || submitErr != nil {
		t.Fatal("ordinary deadline sweep delay destroyed the platform's leased writer availability")
	}
	// A successful sweep or a safe whole-batch rollback is acceptable. A
	// fresh pass on the same healthy owner must complete the original expiry.
	want := 1
	if value.err == nil && value.result.Interrupted == 1 {
		want = 0
	} else if value.result.Interrupted != 0 {
		t.Fatal("availability delay exposed a partial expiry result", value.result)
	}
	retry, err := c.SweepExpired(t.Context(), DeadlineSweepCursor{})
	if err != nil || retry.Interrupted != want {
		t.Fatal("healthy writer could not complete a fresh expiry pass", retry, want, err)
	}
	deadlineSweepRequireStored(t, c, row, true)
	events := auditEvents(t, reader, row.address.Scope)
	if len(events) != 1 {
		t.Fatal("availability recovery did not retain one atomic expiry", len(events))
	}
	deadlineSweepRequireEvent(t, events[0], row)
	t.Logf("DEADLINE_SWEEP_AVAILABILITY fresh_retry_interrupted=%d final_expiry_events=1 same_owner_usable=true", retry.Interrupted)
}

func checkPostgresDeadlineSweepAuditRollback(t *testing.T, objects Objects, dsn string) {
	t.Helper()
	c, reader, _ := openOwnedAuditLinksPostgres(t, objects, dsn)
	var rows []deadlineSweepRow
	for i := range 2 {
		row := deadlineSweepFixtureRow(t, scope(fmt.Sprintf("deadline-pg-audit-%d", i)), "", task.Running, time.Now().UTC().Add(-time.Hour))
		deadlineSweepInsert(t, c, row)
		rows = append(rows, row)
	}
	if _, err := c.writer.ExecContext(t.Context(), `CREATE FUNCTION reject_owned_deadline_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.event::jsonb->>'action'='task.interrupted' AND (SELECT count(*) FROM audit_events WHERE event::jsonb->>'action'='task.interrupted')=1 THEN RAISE EXCEPTION 'owned second deadline audit rejection'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_owned_deadline_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_owned_deadline_audit()`); err != nil {
		t.Fatal(err)
	}
	result, err := c.SweepExpired(t.Context(), DeadlineSweepCursor{})
	if err == nil || result.Interrupted != 0 || result.Cursor != (DeadlineSweepCursor{}) {
		t.Fatal("real PostgreSQL second audit refusal did not reject the batch", result, err)
	}
	for _, row := range rows {
		deadlineSweepRequireStored(t, c, row, false)
	}
	if len(deadlineSweepRawAudit(t, c)) != 0 {
		t.Fatal("real PostgreSQL audit refusal left a partial first expiry")
	}
	if _, err := c.writer.ExecContext(t.Context(), "DROP TRIGGER reject_owned_deadline_audit ON audit_events; DROP FUNCTION reject_owned_deadline_audit()"); err != nil {
		t.Fatal(err)
	}
	result, err = c.SweepExpired(auditHTTPContext(t.Context(), "owned-deadline-token-not-the-controller", "execute"), DeadlineSweepCursor{})
	if err != nil || result.Interrupted != len(rows) {
		t.Fatal("PostgreSQL whole-batch retry failed", result, err)
	}
	for _, row := range rows {
		deadlineSweepRequireStored(t, c, row, true)
		events := auditEvents(t, reader, row.address.Scope)
		if len(events) != 1 {
			t.Fatal("PostgreSQL retry duplicated expiry history", len(events))
		}
		deadlineSweepRequireEvent(t, events[0], row)
	}
	t.Logf("DEADLINE_SWEEP_PG scenario=second_audit_rejected atomic_rollback=true recovery_interrupted=%d controller_actor=true", result.Interrupted)
}

func checkPostgresDeadlineSweepLeaseRollback(t *testing.T, objects Objects, dsn string) {
	t.Helper()
	c, reader, isolatedDSN := openOwnedAuditLinksPostgres(t, objects, dsn)
	var rows []deadlineSweepRow
	for i := range 2 {
		row := deadlineSweepFixtureRow(t, scope(fmt.Sprintf("deadline-pg-lease-%d", i)), "", task.Cancelling, time.Now().UTC().Add(-time.Hour))
		deadlineSweepInsert(t, c, row)
		rows = append(rows, row)
	}
	var writerPID int
	if err := c.writer.QueryRowContext(t.Context(), "SELECT pg_backend_pid()").Scan(&writerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.writer.ExecContext(t.Context(), `CREATE FUNCTION terminate_owned_deadline_writer() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.event::jsonb->>'action'='task.interrupted' AND (SELECT count(*) FROM audit_events WHERE event::jsonb->>'action'='task.interrupted')=1 THEN PERFORM pg_terminate_backend(pg_backend_pid()); END IF; RETURN NEW; END $$;
 CREATE TRIGGER terminate_owned_deadline_writer BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION terminate_owned_deadline_writer()`); err != nil {
		t.Fatal(err)
	}
	result, err := c.SweepExpired(t.Context(), DeadlineSweepCursor{})
	if err == nil || result.Interrupted != 0 || result.Cursor != (DeadlineSweepCursor{}) {
		t.Fatal("terminated PostgreSQL writer committed part of the sweep", result, err)
	}
	for until := time.Now().Add(5 * time.Second); ; {
		var alive bool
		if err := c.db.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)", writerPID).Scan(&alive); err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		if time.Now().After(until) {
			t.Fatal("deadline writer backend was not actually terminated")
		}
		time.Sleep(time.Millisecond)
	}
	for _, row := range rows {
		deadlineSweepRequireStored(t, c, row, false)
	}
	if len(deadlineSweepRawAudit(t, c)) != 0 {
		t.Fatal("dead PostgreSQL lease left partial expiry audit")
	}
	if result, err := c.SweepExpired(t.Context(), DeadlineSweepCursor{}); err == nil || result.Interrupted != 0 {
		t.Fatal("dead writer silently used the reader pool to commit expiry", result, err)
	}
	if _, err := c.db.ExecContext(t.Context(), "DROP TRIGGER terminate_owned_deadline_writer ON audit_events; DROP FUNCTION terminate_owned_deadline_writer()"); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := OpenCatalog(t.Context(), t.TempDir(), isolatedDSN, objects)
	if err != nil {
		t.Fatal("healthy replacement could not acquire the released deadline writer lease", err)
	}
	defer replacement.Close()
	if result, err := replacement.SweepExpired(t.Context(), DeadlineSweepCursor{}); err != nil || result.Interrupted != len(rows) {
		t.Fatal("healthy lease could not recover the original expiry batch", result, err)
	}
	for _, row := range rows {
		deadlineSweepRequireStored(t, replacement, row, true)
		events := auditEvents(t, reader, row.address.Scope)
		if len(events) != 1 {
			t.Fatal("replacement expiry was not exactly one durable transition", len(events))
		}
		deadlineSweepRequireEvent(t, events[0], row)
	}
	t.Logf("DEADLINE_SWEEP_PG scenario=writer_terminated backend_gone=true atomic_rollback=true replacement_interrupted=%d", len(rows))
}

func checkPostgresDeadlineSweepFinishBarrier(t *testing.T, objects Objects, dsn string, expires bool) {
	t.Helper()
	c, reader, isolatedDSN := openOwnedAuditLinksPostgres(t, objects, dsn)
	f := prepareAuditLinksDeadline(t, c, reader, scope(fmt.Sprint("deadline-pg-finish-", expires)))
	gate, err := c.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	const lock = int64(910027)
	if _, err := gate.ExecContext(t.Context(), "SELECT pg_advisory_lock($1)", lock); err != nil {
		gate.Close()
		t.Fatal(err)
	}
	var released sync.Once
	unblock := func() {
		released.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := gate.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", lock); err != nil {
				t.Error("cannot release owned Finish SQL barrier", err)
			}
			gate.Close()
		})
	}
	defer unblock()
	t.Cleanup(unblock)
	query := fmt.Sprintf(`CREATE FUNCTION wait_owned_deadline_finish() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.scope='%s' AND NEW.event::jsonb->>'action'='turn.succeeded' THEN PERFORM pg_advisory_xact_lock(%d); END IF; RETURN NEW; END $$;
 CREATE TRIGGER wait_owned_deadline_finish BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION wait_owned_deadline_finish()`, f.address.Prefix(), lock)
	if _, err := c.writer.ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	var writerPID int
	if err := c.writer.QueryRowContext(t.Context(), "SELECT pg_backend_pid()").Scan(&writerPID); err != nil {
		t.Fatal(err)
	}
	deadline := 10 * time.Second
	if expires {
		deadline = 1500 * time.Millisecond
	}
	f.setDeadline(t, deadline)
	beforeAudit := deadlineSweepRawAudit(t, c)
	finishDone := make(chan error, 1)
	go func() {
		var record task.Record
		finishDone <- f.f.worker.JSON(t.Context(), "POST", f.f.route(f.address.turn)+"/finish", f.finish, &record)
	}()
	for until := time.Now().Add(time.Second); ; {
		var waiting bool
		if err := gate.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted)", writerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-finishDone:
			t.Fatal("Finish returned before the controlled PostgreSQL advisory wait", err)
		default:
		}
		if time.Now().After(until) {
			t.Fatal("Finish never entered its real PostgreSQL advisory wait")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !time.Now().Before(f.before.Deadline) {
		t.Fatal("Finish did not enter SQL before its original deadline")
	}
	if expires {
		<-time.After(time.Until(f.before.Deadline) + 20*time.Millisecond)
		unblock()
		select {
		case err := <-finishDone:
			if err == nil {
				t.Fatal("SQL wait committed success after the original deadline")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("expired Finish did not return after releasing SQL")
		}
		f.requireRollback(t)
		if !reflect.DeepEqual(beforeAudit, deadlineSweepRawAudit(t, c)) {
			t.Fatal("expired SQL wait changed original stored audit bytes")
		}
		// The old request's timeout may discard its leased connection. Reopen
		// deliberately rather than hiding a dead lease with the reader pool.
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		replacement, err := OpenCatalog(t.Context(), t.TempDir(), isolatedDSN, objects)
		if err != nil {
			t.Fatal(err)
		}
		defer replacement.Close()
		result, err := replacement.SweepExpired(t.Context(), DeadlineSweepCursor{})
		if err != nil || result.Interrupted != 1 {
			t.Fatal("healthy controller did not persist the original expired Turn", result, err)
		}
		after, err := replacement.readStoredTask(t.Context(), f.address)
		want := f.before
		want.State = task.Interrupted
		if err != nil || !reflect.DeepEqual(want, after) {
			t.Fatal("expiry after failed Finish changed its frozen record", err)
		}
		cp, err := replacement.latestCheckpoint(t.Context(), f.address.Scope)
		if err != nil || !reflect.DeepEqual(cp, f.cp) {
			t.Fatal("deadline interruption advanced the old checkpoint", err)
		}
		t.Log("DEADLINE_SWEEP_PG scenario=finish_sql_wait_crossed_deadline finish_rolled_back=true interrupted=1 checkpoint_unchanged=true")
		return
	}
	type sweepResult struct {
		result DeadlineSweepResult
		err    error
	}
	sweepDone := make(chan sweepResult, 1)
	go func() {
		result, err := c.SweepExpired(t.Context(), DeadlineSweepCursor{})
		sweepDone <- sweepResult{result, err}
	}()
	// pg_stat_activity retains the last query while idle. Observing the actual
	// Turn keyset SELECT proves this sweep read its keys while Finish still
	// held Catalog.mu and its SQL transaction was uncommitted.
	for until := time.Now().Add(time.Second); ; {
		var scanned bool
		if err := gate.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND query LIKE $1)", "SELECT substr(scope,1,516),substr(id,1,37) FROM turns WHERE%").Scan(&scanned); err != nil {
			t.Fatal(err)
		}
		if scanned {
			break
		}
		select {
		case outcome := <-sweepDone:
			t.Fatal("sweep returned before the controlled candidate-read boundary", outcome.result, outcome.err)
		default:
		}
		if time.Now().After(until) {
			t.Fatal("sweep never observed Turn keys during the uncommitted Finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	unblock()
	select {
	case err := <-finishDone:
		if err != nil {
			t.Fatal("Finish could not commit before its still-valid original deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Finish did not complete after releasing its SQL barrier")
	}
	select {
	case outcome := <-sweepDone:
		if outcome.err != nil || outcome.result.Interrupted != 0 {
			t.Fatal("sweep overwrote a successful concurrent Finish", outcome.result, outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sweep did not stop waiting for the completed Finish")
	}
	stored, err := c.readStoredTask(t.Context(), f.address)
	if err != nil || stored.State != task.Succeeded || stored.attempt != f.before.attempt || !stored.Deadline.Equal(f.before.Deadline) {
		t.Fatal("fresh re-read did not retain the successful frozen receipt", err)
	}
	events := auditEvents(t, reader, f.address.Scope)
	for _, event := range events {
		if event.Action == "turn.interrupted" {
			t.Fatal("successful concurrent Finish acquired an expiry event")
		}
	}
	afterAudit := deadlineSweepRawAudit(t, c)
	if len(afterAudit) < len(beforeAudit) || !reflect.DeepEqual(beforeAudit, afterAudit[:len(beforeAudit)]) {
		t.Fatal("concurrent sweep rewrote prior stored audit bytes")
	}
	t.Log("DEADLINE_SWEEP_PG scenario=finish_committed_first sql_barrier_observed=true keyset_read_observed=true fresh_record_succeeded=true interrupted=0")
}
