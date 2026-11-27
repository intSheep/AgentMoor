//go:build integration

package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/intSheep/AgentMoor/internal/task"
	"github.com/lib/pq"
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
	checkPostgresDeadlineSweepCallerCancellation(t, objects, dsn)
	checkPostgresDeadlineSweepLowerStatementTimeout(t, objects, dsn)
	checkPostgresDeadlineSweepLowerLockTimeout(t, objects, dsn)
	checkPostgresDeadlineSweepSoftWriteBudget(t, objects, dsn)
	checkPostgresMaintenanceBlackholes(t, objects, dsn)
}

type deadlineSweepPGOutcome struct {
	result DeadlineSweepResult
	err    error
}

func deadlineSweepPGPID(t *testing.T, c *Catalog) int {
	t.Helper()
	var pid int
	if err := c.writer.QueryRowContext(t.Context(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal("cannot observe owned deadline writer PID", err)
	}
	return pid
}

func deadlineSweepPGSQLCode(err error) string {
	var server *pq.Error
	if errors.As(err, &server) {
		return string(server.Code)
	}
	return ""
}

func deadlineSweepPGHealthy(t *testing.T, c *Catalog, originalPID int, scenario string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var currentPID int
	writerErr := c.writer.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&currentPID)
	var alive, lease bool
	if err := c.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1),
 EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND granted AND classid=193674738 AND objid=1)`, originalPID).Scan(&alive, &lease); err != nil {
		t.Fatal(err)
	}
	_, submitErr := c.submitTask(ctx, taskAddress{Scope: scope("deadline-health-" + scenario)}, task.Spec{Instruction: "ordinary legal mutation after bounded maintenance", Output: "reply.txt"})
	t.Logf("DEADLINE_SWEEP_WRITE_GUARD scenario=%s writer_query_ok=%t same_writer_pid=%t original_backend_alive=%t original_advisory_lease=%t subsequent_submit_ok=%t", scenario, writerErr == nil, currentPID == originalPID, alive, lease, submitErr == nil)
	if writerErr != nil || currentPID != originalPID || !alive || !lease || submitErr != nil {
		t.Fatal("ordinary maintenance cancellation or server timeout poisoned the leased writer", scenario)
	}
}

func deadlineSweepPGAuditDelay(t *testing.T, c *Catalog, milliseconds int) func() {
	t.Helper()
	query := fmt.Sprintf(`CREATE FUNCTION delay_owned_deadline_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.event::jsonb->>'action' IN ('task.interrupted','turn.interrupted') THEN PERFORM pg_sleep(%d.0/1000.0); END IF; RETURN NEW; END $$;
 CREATE TRIGGER delay_owned_deadline_guard BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION delay_owned_deadline_guard()`, milliseconds)
	if _, err := c.writer.ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	var removed sync.Once
	drop := func() {
		removed.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := c.db.ExecContext(ctx, "DROP TRIGGER delay_owned_deadline_guard ON audit_events; DROP FUNCTION delay_owned_deadline_guard()"); err != nil {
				t.Error("cannot remove owned deadline delay", err)
			}
		})
	}
	t.Cleanup(drop)
	return drop
}

func deadlineSweepPGWait(t *testing.T, c *Catalog, pid int, event, eventType string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for {
		var observed bool
		if err := c.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
 WHERE pid=$1 AND ($2='' OR wait_event=$2) AND ($3='' OR wait_event_type=$3) AND query LIKE $4)`, pid, event, eventType, "%audit_events%").Scan(&observed); err != nil {
			t.Fatal("cannot observe the real owned audit SQL boundary", err)
		}
		if observed {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("owned deadline writer never reached the required real audit SQL wait", event, eventType)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func deadlineSweepPGRollback(t *testing.T, c *Catalog, rows []deadlineSweepRow) {
	t.Helper()
	for _, row := range rows {
		deadlineSweepRequireStored(t, c, row, false)
	}
	if len(deadlineSweepRawAudit(t, c)) != 0 {
		t.Fatal("failed maintenance write left any partial audit or transition")
	}
}

func deadlineSweepPGRecover(t *testing.T, c *Catalog, reader *AuditReader, rows []deadlineSweepRow, scenario string) {
	t.Helper()
	result, err := c.SweepExpired(t.Context(), DeadlineSweepCursor{})
	if err != nil || result.Interrupted != len(rows) {
		t.Fatal("same healthy owner could not complete a fresh bounded pass", scenario, result, err)
	}
	for _, row := range rows {
		deadlineSweepRequireStored(t, c, row, true)
		events := auditEvents(t, reader, row.address.Scope)
		if len(events) != 1 {
			t.Fatal("fresh pass did not preserve one atomic expiry per allocation", scenario, len(events))
		}
		deadlineSweepRequireEvent(t, events[0], row)
		raw, err := json.Marshal(events[0])
		if err != nil || len(raw) > auditMaxEventBytes {
			t.Fatal("cannot encode a bounded actual deadline audit event", err)
		}
		t.Logf("DEADLINE_SWEEP_EVENT %s", raw)
	}
	t.Logf("DEADLINE_SWEEP_WRITE_GUARD scenario=%s fresh_retry_interrupted=%d final_expiry_events=%d", scenario, result.Interrupted, len(rows))
}

func checkPostgresDeadlineSweepCallerCancellation(t *testing.T, objects Objects, dsn string) {
	t.Helper()
	c, reader, _ := openOwnedAuditLinksPostgres(t, objects, dsn)
	row := deadlineSweepFixtureRow(t, scope("deadline-caller-cancel"), "", task.Running, time.Now().UTC().Add(-time.Hour))
	deadlineSweepInsert(t, c, row)
	pid := deadlineSweepPGPID(t, c)
	drop := deadlineSweepPGAuditDelay(t, c, 200)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan deadlineSweepPGOutcome, 1)
	received := false
	t.Cleanup(func() {
		cancel()
		if !received {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("cancelled deadline sweep did not join during bounded cleanup")
			}
		}
	})
	go func() {
		result, err := c.SweepExpired(ctx, DeadlineSweepCursor{})
		done <- deadlineSweepPGOutcome{result, err}
	}()
	deadlineSweepPGWait(t, c, pid, "PgSleep", "")
	cancel()
	var outcome deadlineSweepPGOutcome
	select {
	case outcome = <-done:
		received = true
	case <-time.After(5 * time.Second):
		t.Fatal("caller cancellation did not stop the bounded write")
	}
	if !errors.Is(outcome.err, context.Canceled) || outcome.result.Interrupted != 0 || outcome.result.Cursor != (DeadlineSweepCursor{}) {
		t.Fatal("caller cancellation did not roll back the whole batch", outcome.result, outcome.err)
	}
	deadlineSweepPGRollback(t, c, []deadlineSweepRow{row})
	t.Log("DEADLINE_SWEEP_WRITE_GUARD scenario=caller_cancel audit_pg_sleep_observed=true caller_cancelled=true atomic_rollback=true")
	deadlineSweepPGHealthy(t, c, pid, "caller-cancel")
	drop()
	deadlineSweepPGRecover(t, c, reader, []deadlineSweepRow{row}, "caller-cancel")
}

func checkPostgresDeadlineSweepLowerStatementTimeout(t *testing.T, objects Objects, dsn string) {
	t.Helper()
	c, reader, _ := openOwnedAuditLinksPostgres(t, objects, dsn)
	row := deadlineSweepFixtureRow(t, scope("deadline-lower-statement"), "", task.Running, time.Now().UTC().Add(-time.Hour))
	deadlineSweepInsert(t, c, row)
	pid := deadlineSweepPGPID(t, c)
	drop := deadlineSweepPGAuditDelay(t, c, 150)
	if _, err := c.writer.ExecContext(t.Context(), "SET statement_timeout='80ms'"); err != nil {
		t.Fatal(err)
	}
	result, err := c.SweepExpired(t.Context(), DeadlineSweepCursor{})
	code := deadlineSweepPGSQLCode(err)
	var setting int
	settingErr := c.writer.QueryRowContext(t.Context(), "SELECT setting::integer FROM pg_settings WHERE name='statement_timeout'").Scan(&setting)
	t.Logf("DEADLINE_SWEEP_WRITE_GUARD scenario=lower_statement_timeout original_ms=80 audit_sleep_ms=150 sqlstate=%s interrupted=%d setting_after_ms=%d", code, result.Interrupted, setting)
	if code != "57014" || result.Interrupted != 0 || result.Cursor != (DeadlineSweepCursor{}) || settingErr != nil || setting != 80 {
		t.Fatal("maintenance widened the original statement timeout or lost the writer", result, setting, settingErr, err)
	}
	deadlineSweepPGRollback(t, c, []deadlineSweepRow{row})
	deadlineSweepPGHealthy(t, c, pid, "lower-statement")
	drop()
	deadlineSweepPGRecover(t, c, reader, []deadlineSweepRow{row}, "lower-statement")
}

func checkPostgresDeadlineSweepLowerLockTimeout(t *testing.T, objects Objects, dsn string) {
	t.Helper()
	c, reader, _ := openOwnedAuditLinksPostgres(t, objects, dsn)
	row := deadlineSweepFixtureRow(t, scope("deadline-lower-lock"), "", task.Running, time.Now().UTC().Add(-time.Hour))
	deadlineSweepInsert(t, c, row)
	pid := deadlineSweepPGPID(t, c)
	if _, err := c.writer.ExecContext(t.Context(), "SET lock_timeout='50ms'"); err != nil {
		t.Fatal(err)
	}
	blocker, err := c.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.ExecContext(t.Context(), "LOCK TABLE audit_events IN ACCESS EXCLUSIVE MODE"); err != nil {
		blocker.Rollback()
		t.Fatal(err)
	}
	var released sync.Once
	unblock := func() {
		released.Do(func() {
			if err := blocker.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Error("cannot release owned audit table lock", err)
			}
		})
	}
	defer unblock()
	done := make(chan deadlineSweepPGOutcome, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	received := false
	t.Cleanup(func() {
		unblock()
		cancel()
		if !received {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("lock-timeout sweep did not join during bounded cleanup")
			}
		}
	})
	go func() {
		result, err := c.SweepExpired(ctx, DeadlineSweepCursor{})
		done <- deadlineSweepPGOutcome{result, err}
	}()
	deadlineSweepPGWait(t, c, pid, "", "Lock")
	var outcome deadlineSweepPGOutcome
	select {
	case outcome = <-done:
		received = true
	case <-time.After(5 * time.Second):
		t.Fatal("original lock timeout did not stop the actual audit SQL wait")
	}
	unblock()
	cancel()
	code := deadlineSweepPGSQLCode(outcome.err)
	var setting int
	settingErr := c.writer.QueryRowContext(t.Context(), "SELECT setting::integer FROM pg_settings WHERE name='lock_timeout'").Scan(&setting)
	t.Logf("DEADLINE_SWEEP_WRITE_GUARD scenario=lower_lock_timeout original_ms=50 audit_table_lock_observed=true sqlstate=%s interrupted=%d setting_after_ms=%d", code, outcome.result.Interrupted, setting)
	if code != "55P03" || outcome.result.Interrupted != 0 || outcome.result.Cursor != (DeadlineSweepCursor{}) || settingErr != nil || setting != 50 {
		t.Fatal("maintenance widened the original lock timeout or lost the writer", outcome.result, setting, settingErr, outcome.err)
	}
	deadlineSweepPGRollback(t, c, []deadlineSweepRow{row})
	deadlineSweepPGHealthy(t, c, pid, "lower-lock")
	deadlineSweepPGRecover(t, c, reader, []deadlineSweepRow{row}, "lower-lock")
}

func checkPostgresDeadlineSweepSoftWriteBudget(t *testing.T, objects Objects, dsn string) {
	t.Helper()
	c, reader, _ := openOwnedAuditLinksPostgres(t, objects, dsn)
	var rows []deadlineSweepRow
	for i := range deadlineSweepWritesPerTable {
		root := deadlineSweepFixtureRow(t, scope(fmt.Sprintf("a-deadline-soft-%d", i)), "", task.Running, time.Now().UTC().Add(-time.Hour))
		turn := deadlineSweepFixtureRow(t, scope(fmt.Sprintf("z-deadline-soft-%d", i)), uuid.NewString(), task.Running, time.Now().UTC().Add(-time.Hour))
		for _, row := range []deadlineSweepRow{root, turn} {
			deadlineSweepInsert(t, c, row)
			rows = append(rows, row)
		}
	}
	pid := deadlineSweepPGPID(t, c)
	// PostgreSQL sequences retain observations across rollback. They prove
	// that the soft budget expired in real writes, not during keyset reads.
	if _, err := c.writer.ExecContext(t.Context(), `CREATE SEQUENCE owned_deadline_update_calls; CREATE SEQUENCE owned_deadline_audit_calls;
 CREATE FUNCTION slow_owned_deadline_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.record::jsonb->>'state'='interrupted' THEN PERFORM nextval('owned_deadline_update_calls'); PERFORM pg_sleep(0.1); END IF; RETURN NEW; END $$;
 CREATE TRIGGER slow_owned_deadline_update BEFORE UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION slow_owned_deadline_update();
 CREATE TRIGGER slow_owned_deadline_update BEFORE UPDATE ON turns FOR EACH ROW EXECUTE FUNCTION slow_owned_deadline_update();
 CREATE FUNCTION slow_owned_deadline_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.event::jsonb->>'action' IN ('task.interrupted','turn.interrupted') THEN PERFORM nextval('owned_deadline_audit_calls'); PERFORM pg_sleep(0.2); END IF; RETURN NEW; END $$;
 CREATE TRIGGER slow_owned_deadline_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION slow_owned_deadline_audit()`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result, err := c.SweepExpired(t.Context(), DeadlineSweepCursor{})
	elapsed := time.Since(start)
	var updates, audits int64
	var updateCalled, auditCalled bool
	if queryErr := c.db.QueryRowContext(t.Context(), "SELECT last_value,is_called FROM owned_deadline_update_calls").Scan(&updates, &updateCalled); queryErr != nil {
		t.Fatal(queryErr)
	}
	if queryErr := c.db.QueryRowContext(t.Context(), "SELECT last_value,is_called FROM owned_deadline_audit_calls").Scan(&audits, &auditCalled); queryErr != nil {
		t.Fatal(queryErr)
	}
	t.Logf("DEADLINE_SWEEP_WRITE_GUARD scenario=soft_write_budget elapsed_ms=%d update_calls=%d audit_calls=%d sqlstate=%s interrupted=%d", elapsed.Milliseconds(), updates, audits, deadlineSweepPGSQLCode(err), result.Interrupted)
	if !errors.Is(err, context.DeadlineExceeded) || deadlineSweepPGSQLCode(err) != "" || result.Interrupted != 0 || result.Cursor != (DeadlineSweepCursor{}) ||
		elapsed < deadlineSweepWriteBudget || !updateCalled || !auditCalled {
		t.Fatal("real sub-statement delays did not exhaust the soft write budget safely", result, err)
	}
	deadlineSweepPGRollback(t, c, rows)
	deadlineSweepPGHealthy(t, c, pid, "soft-budget")
	if _, err := c.writer.ExecContext(t.Context(), `DROP TRIGGER slow_owned_deadline_update ON tasks; DROP TRIGGER slow_owned_deadline_update ON turns; DROP FUNCTION slow_owned_deadline_update();
 DROP TRIGGER slow_owned_deadline_audit ON audit_events; DROP FUNCTION slow_owned_deadline_audit(); DROP SEQUENCE owned_deadline_update_calls; DROP SEQUENCE owned_deadline_audit_calls`); err != nil {
		t.Fatal(err)
	}
	deadlineSweepPGRecover(t, c, reader, rows, "soft-budget")
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
	started := make(chan time.Time, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	var start time.Time
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
		started <- time.Now()
		result, err := c.SweepExpired(ctx, DeadlineSweepCursor{})
		done <- outcome{result, err}
	}()
	select {
	case start = <-started:
	case <-ctx.Done():
		t.Fatal("availability sweep did not start within its bounded caller context")
	}
	// Confirm that this exact keyset read is really blocked on PostgreSQL,
	// rather than sleeping in the test while no production statement runs.
	for until := time.Now().Add(time.Second); ; {
		var waiting bool
		if err := c.db.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE $1)", "% FROM tasks WHERE scope>%").Scan(&waiting); err != nil {
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
		if err := gate.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND query LIKE $1)", "% FROM turns WHERE (scope,id)>%").Scan(&scanned); err != nil {
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
