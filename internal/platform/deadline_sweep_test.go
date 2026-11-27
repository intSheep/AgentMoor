package platform

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/task"
)

type deadlineSweepRow struct {
	address taskAddress
	raw     string
	attempt string
	state   string
	seq     int64
}

type deadlineSweepAuditRow struct {
	cursor int64
	scope  string
	raw    string
}

func deadlineSweepStateToken(state string) string {
	quoted, _ := json.Marshal(state)
	if state == task.Running {
		quoted = []byte(`"runn\u0069ng"`)
	}
	return "\"state\" \t:  " + string(quoted)
}

// A synthetic legacy row deliberately retains noncanonical whitespace,
// escaped strings, a nested state, unknown members, pins and immutable refs.
// Only the top-level state value may change; a marshal/unmarshal roundtrip
// would lose bytes even when its decoded task.Record appeared equivalent.
func deadlineSweepFixtureRow(t *testing.T, s asset.Scope, turn, state string, deadline time.Time) deadlineSweepRow {
	t.Helper()
	ref := func(kind, id string) task.Ref {
		key := asset.Key{Kind: kind, ID: id}
		return task.Ref{Key: key, Version: 7, SHA256: strings.Repeat("a", 64), ContentPath: key.ContentPath(7)}
	}
	pin := task.SkillPin{Name: "original-rules", Source: ref("skill", "catalog/original/SKILL.md")}
	parent := &task.Checkpoint{SessionID: "ses_private_parent", Session: ref("session", "ses_private_parent"), Workspace: ref("workspace", "root"), Skills: []task.SkillPin{pin}}
	record := task.Record{ID: s.Runtime, TurnID: turn, State: state, Deadline: deadline,
		ExecutionPolicy: task.ExecutionUntrustedIsolated,
		Spec:            task.Spec{Mode: task.FileTurn, Instruction: "private synthetic deadline instruction", Input: `private original input {"state":"running"}`, Output: "report.json"},
		Parent:          parent, Skills: []task.SkillPin{pin}, Assets: []task.Ref{ref("artifact", "out/report.json"), parent.Workspace, parent.Session}, Error: "private retained legacy note"}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"state":"`+state+`"`), []byte(deadlineSweepStateToken(state)), 1)
	text := " \n" + strings.TrimSuffix(string(raw), "}") + ",\n\"legacy_extension\" : {\"state\":\"running\",\"escaped\":\"original\\u0020bytes\",\"nested\":[1,true,null]}\n}\t\n"
	attempt := uuid.NewString()
	if state == task.Queued {
		attempt = ""
	}
	return deadlineSweepRow{address: taskAddress{s, turn}, raw: text, attempt: attempt, state: state, seq: 1}
}

func deadlineSweepInsert(t *testing.T, c *Catalog, row deadlineSweepRow) {
	t.Helper()
	var err error
	if row.address.turn == "" {
		_, err = c.writer.ExecContext(t.Context(), "INSERT INTO tasks(scope,record,attempt) VALUES($1,$2,$3)", row.address.Prefix(), row.raw, row.attempt)
	} else {
		_, err = c.writer.ExecContext(t.Context(), "INSERT INTO turns(scope,id,seq,state,record,attempt) VALUES($1,$2,$3,$4,$5,$6)", row.address.Prefix(), row.address.turn, row.seq, row.state, row.raw, row.attempt)
	}
	if err != nil {
		t.Fatal("cannot insert owned legacy deadline fixture", err)
	}
}

func deadlineSweepStored(t *testing.T, c *Catalog, row deadlineSweepRow) (string, string, string) {
	t.Helper()
	var raw, attempt, state string
	var err error
	if row.address.turn == "" {
		err = c.db.QueryRowContext(t.Context(), "SELECT record,attempt FROM tasks WHERE scope=$1", row.address.Prefix()).Scan(&raw, &attempt)
	} else {
		err = c.db.QueryRowContext(t.Context(), "SELECT record,attempt,state FROM turns WHERE scope=$1 AND id=$2", row.address.Prefix(), row.address.turn).Scan(&raw, &attempt, &state)
	}
	if err != nil {
		t.Fatal("cannot read stored deadline fixture", err)
	}
	return raw, attempt, state
}

func deadlineSweepRequireStored(t *testing.T, c *Catalog, row deadlineSweepRow, interrupted bool) {
	t.Helper()
	want, state := row.raw, row.state
	if interrupted {
		want = strings.Replace(want, deadlineSweepStateToken(row.state), "\"state\" \t:  \"interrupted\"", 1)
		state = task.Interrupted
	}
	raw, attempt, columnState := deadlineSweepStored(t, c, row)
	if raw != want || attempt != row.attempt || (row.address.turn != "" && columnState != state) {
		t.Fatal("sweep changed bytes beyond the original top-level state token, attempt or turn index", row.address.Scope, row.address.turn)
	}
}

func deadlineSweepRawAudit(t *testing.T, c *Catalog) []deadlineSweepAuditRow {
	t.Helper()
	rows, err := c.db.QueryContext(t.Context(), "SELECT cursor,scope,event FROM audit_events ORDER BY cursor")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []deadlineSweepAuditRow{}
	for rows.Next() {
		var row deadlineSweepAuditRow
		if err := rows.Scan(&row.cursor, &row.scope, &row.raw); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func deadlineSweepRequireEvent(t *testing.T, event AuditEvent, row deadlineSweepRow) {
	t.Helper()
	action := "task.interrupted"
	if row.address.turn != "" {
		action = "turn.interrupted"
	}
	wantAttempt := ""
	if row.attempt != "" {
		wantAttempt = auditHash("attempt", row.attempt)
	}
	if event.Action != action || event.Scope != row.address.Scope || event.TaskID != row.address.Runtime || event.TurnID != row.address.turn ||
		event.PreviousState != row.state || event.State != task.Interrupted || event.AttemptSHA256 != wantAttempt ||
		event.Actor != (AuditActor{Source: "deadline-controller", Role: "system"}) {
		t.Fatal("sweep event lost its frozen allocation or trusted controller identity", event)
	}
	raw, err := json.Marshal(event)
	if err != nil || strings.Contains(string(raw), "private synthetic") || strings.Contains(string(raw), "private original") ||
		strings.Contains(string(raw), "ses_private_parent") || strings.Contains(string(raw), "owned-deadline-token") ||
		(row.attempt != "" && strings.Contains(string(raw), row.attempt)) {
		t.Fatal("sweep event copied task content or credential material", err)
	}
}

func TestDeadlineSweepStateTablePreservesOriginalJSONAndAuditBytes(t *testing.T) {
	f := newAuditFixture(t)
	history := scope("deadline-history")
	if _, err := f.c.submitTask(t.Context(), taskAddress{Scope: history}, task.Spec{Instruction: "prior synthetic task", Output: "reply.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.db.ExecContext(t.Context(), "UPDATE audit_events SET event=$1 || event || $2", " \n", "\t "); err != nil {
		t.Fatal(err)
	}
	beforeAudit := deadlineSweepRawAudit(t, f.c)
	var previousRaw string
	if err := f.c.db.QueryRowContext(t.Context(), "SELECT record FROM tasks WHERE scope=$1", history.Prefix()).Scan(&previousRaw); err != nil {
		t.Fatal(err)
	}
	var rows []deadlineSweepRow
	eligible := make(map[string]bool)
	wantInterrupted := 0
	states := []string{task.Queued, task.Running, task.Cancelling, task.Succeeded, task.Failed, task.Cancelled, task.Interrupted}
	for _, turn := range []bool{false, true} {
		for _, state := range states {
			for _, when := range []string{"past", "future", "zero"} {
				s := scope(fmt.Sprintf("deadline-%t-%s-%s", turn, state, when))
				id := ""
				if turn {
					id = uuid.NewString()
				}
				deadline := time.Time{}
				if when == "past" {
					deadline = time.Now().UTC().Add(-time.Hour)
				} else if when == "future" {
					deadline = time.Now().UTC().Add(time.Hour)
				}
				row := deadlineSweepFixtureRow(t, s, id, state, deadline)
				deadlineSweepInsert(t, f.c, row)
				rows = append(rows, row)
				if when != "future" && (state == task.Running || state == task.Cancelling || (turn && state == task.Queued)) {
					eligible[s.Prefix()] = true
					wantInterrupted++
				}
			}
		}
	}
	ctx := auditHTTPContext(t.Context(), "owned-deadline-token-must-not-be-actor", "execute")
	var result DeadlineSweepResult
	var err error
	var cursor DeadlineSweepCursor
	interrupted, tasksWrapped, turnsWrapped := 0, false, false
	previousAudit := beforeAudit
	for pass := range len(rows) + 2 {
		result, err = f.c.SweepExpired(ctx, cursor)
		if err != nil || result.Scanned > 2*deadlineSweepRowsPerTable || result.RecordBytes > 2*deadlineSweepBytesPerTable || result.Interrupted > 2*deadlineSweepWritesPerTable {
			t.Fatal("state matrix sweep exceeded one bounded pass", pass, result, err)
		}
		audits := deadlineSweepRawAudit(t, f.c)
		if len(audits) != len(previousAudit)+result.Interrupted || !reflect.DeepEqual(previousAudit, audits[:len(previousAudit)]) {
			t.Fatal("one pass rewrote prior audit bytes or miscounted atomic transitions", pass)
		}
		taskChanges, turnChanges := 0, 0
		for _, stored := range audits[len(previousAudit):] {
			var event AuditEvent
			if json.Unmarshal([]byte(stored.raw), &event) != nil {
				t.Fatal("cannot decode actual new expiry event")
			}
			if event.Action == "task.interrupted" {
				taskChanges++
			} else if event.Action == "turn.interrupted" {
				turnChanges++
			} else {
				t.Fatal("sweep emitted a non-expiry action", event.Action)
			}
		}
		if taskChanges > deadlineSweepWritesPerTable || turnChanges > deadlineSweepWritesPerTable {
			t.Fatal("one table consumed another table's transition allowance", taskChanges, turnChanges)
		}
		previousAudit, cursor = audits, result.Cursor
		interrupted += result.Interrupted
		tasksWrapped, turnsWrapped = tasksWrapped || result.TasksWrapped, turnsWrapped || result.TurnsWrapped
		if tasksWrapped && turnsWrapped {
			break
		}
	}
	if !tasksWrapped || !turnsWrapped || interrupted != wantInterrupted {
		t.Fatal("bounded passes did not finish the original state matrix once", interrupted, wantInterrupted, tasksWrapped, turnsWrapped)
	}
	var preservedHistory string
	if err := f.c.db.QueryRowContext(t.Context(), "SELECT record FROM tasks WHERE scope=$1", history.Prefix()).Scan(&preservedHistory); err != nil || preservedHistory != previousRaw {
		t.Fatal("revisited small-table history changed its literal record bytes", err)
	}
	for _, row := range rows {
		changed := eligible[row.address.Prefix()]
		deadlineSweepRequireStored(t, f.c, row, changed)
		events := auditEvents(t, f.r, row.address.Scope)
		if !changed {
			if len(events) != 0 {
				t.Fatal("future or terminal allocation acquired an expiry event", events)
			}
			continue
		}
		if len(events) != 1 {
			t.Fatal("one transition did not append exactly one event", events)
		}
		deadlineSweepRequireEvent(t, events[0], row)
		if row.address.turn != "" && row.state == task.Queued {
			calls := 0
			if err := f.c.DispatchQueued(t.Context(), row.address.Scope, func(context.Context, asset.Scope, string) (bool, error) { calls++; return false, nil }); err != nil || calls != 0 {
				t.Fatal("persisted queued-Turn expiry dispatched a new Job", calls, err)
			}
		}
	}
	afterAudit := deadlineSweepRawAudit(t, f.c)
	if len(afterAudit) != len(beforeAudit)+wantInterrupted || !reflect.DeepEqual(beforeAudit, afterAudit[:len(beforeAudit)]) {
		t.Fatal("sweep rewrote literal stored historical audit JSON")
	}
	if result, err = f.c.SweepExpired(t.Context(), DeadlineSweepCursor{}); err != nil || result.Interrupted != 0 || !reflect.DeepEqual(afterAudit, deadlineSweepRawAudit(t, f.c)) {
		t.Fatal("repeated sweep invented another expiry", result, err)
	}
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCatalog(t.Context(), f.dir, "", f.objects)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if result, err = reopened.SweepExpired(t.Context(), DeadlineSweepCursor{}); err != nil || result.Interrupted != 0 || !reflect.DeepEqual(afterAudit, deadlineSweepRawAudit(t, reopened)) {
		t.Fatal("actual catalog restart repeated a persisted expiry", result, err)
	}
}

func TestDeadlineSweepExpiredTurnKeepsCheckpointAndCannotDispatchOrReclaim(t *testing.T) {
	f := newAuditFixture(t)
	fixture := prepareAuditLinksDeadline(t, f.c, f.r, scope("deadline-old-checkpoint"))
	fixture.setDeadline(t, -time.Hour)
	var before string
	if err := f.c.db.QueryRowContext(t.Context(), "SELECT record FROM tasks WHERE scope=$1", fixture.address.Prefix()).Scan(&before); err != nil {
		t.Fatal(err)
	}
	result, err := f.c.SweepExpired(t.Context(), DeadlineSweepCursor{})
	if err != nil || result.Interrupted != 1 {
		t.Fatal(result, err)
	}
	after, err := f.c.readStoredTask(t.Context(), fixture.address)
	want := fixture.before
	want.State = task.Interrupted
	if err != nil || !reflect.DeepEqual(after, want) {
		t.Fatal("expiry changed original deadline, attempt, pins, parent or assets", err)
	}
	cp, err := f.c.latestCheckpoint(t.Context(), fixture.address.Scope)
	if err != nil || !reflect.DeepEqual(cp, fixture.cp) {
		t.Fatal("expiry advanced or changed the last successful checkpoint", err)
	}
	var root string
	if err := f.c.db.QueryRowContext(t.Context(), "SELECT record FROM tasks WHERE scope=$1", fixture.address.Prefix()).Scan(&root); err != nil || root != before {
		t.Fatal("expiry rewrote the successful parent receipt", err)
	}
	calls := 0
	if err := f.c.DispatchQueued(t.Context(), fixture.address.Scope, func(context.Context, asset.Scope, string) (bool, error) { calls++; return false, nil }); err != nil || calls != 0 {
		t.Fatal("sweep scheduled another Job", calls, err)
	}
	if _, err := f.c.claimTask(t.Context(), fixture.address, task.Claim{Attempt: uuid.NewString()}); !errors.Is(err, ErrConflict) {
		t.Fatal("expired allocation acquired a new execution attempt", err)
	}
	if _, err := f.c.finishTask(t.Context(), fixture.address, fixture.finish); !errors.Is(err, ErrConflict) {
		t.Fatal("expired frozen receipt became successful", err)
	}
}

func TestDeadlineSweepKeysetProgressesAcrossNoChangePages(t *testing.T) {
	f := newAuditFixture(t)
	const count = 33
	var rows []deadlineSweepRow
	firstBytes := int64(0)
	for i := range count {
		deadline := time.Now().UTC().Add(time.Hour)
		if i == count-1 {
			deadline = time.Now().UTC().Add(-time.Hour)
		}
		root := deadlineSweepFixtureRow(t, scope(fmt.Sprintf("a-deadline-page-%02d", i)), "", task.Running, deadline)
		turn := deadlineSweepFixtureRow(t, scope("z-deadline-turn-pages"), fmt.Sprintf("00000000-0000-4000-8000-%012d", i), task.Running, deadline)
		turn.seq = int64(i + 1)
		for _, row := range []deadlineSweepRow{root, turn} {
			deadlineSweepInsert(t, f.c, row)
			rows = append(rows, row)
			if i < deadlineSweepRowsPerTable {
				firstBytes += int64(len(row.raw))
			}
		}
	}
	first, err := f.c.SweepExpired(t.Context(), DeadlineSweepCursor{})
	if err != nil || first.Scanned != 2*deadlineSweepRowsPerTable || first.Interrupted != 0 || first.RecordBytes != firstBytes || first.TasksWrapped || first.TurnsWrapped || first.Cursor.TaskScope == "" || first.Cursor.TurnScope == "" || first.Cursor.TurnID == "" {
		t.Fatal("no-change page did not respect bounds or advance both keysets", first, err)
	}
	second, err := f.c.SweepExpired(t.Context(), first.Cursor)
	if err != nil || second.Scanned != 2 || second.Interrupted != 2 {
		t.Fatal("keyset skipped the expired rows after unchanged pages", second, err)
	}
	for i, row := range rows {
		deadlineSweepRequireStored(t, f.c, row, i >= len(rows)-2)
	}
	third, err := f.c.SweepExpired(t.Context(), second.Cursor)
	if err != nil || third.Scanned > 2*deadlineSweepRowsPerTable || third.Interrupted != 0 {
		t.Fatal("wrapped scan repeated a completed interruption", third, err)
	}
}

func TestDeadlineSweepAuditRejectionRollsBackTheWholeBatch(t *testing.T) {
	f := newAuditFixture(t)
	var rows []deadlineSweepRow
	for i := range 3 {
		row := deadlineSweepFixtureRow(t, scope(fmt.Sprintf("deadline-atomic-%d", i)), "", task.Running, time.Now().UTC().Add(-time.Hour))
		deadlineSweepInsert(t, f.c, row)
		rows = append(rows, row)
	}
	if _, err := f.c.db.ExecContext(t.Context(), `CREATE TRIGGER reject_second_expiry BEFORE INSERT ON audit_events WHEN json_extract(NEW.event,'$.action')='task.interrupted' AND (SELECT count(*) FROM audit_events WHERE json_extract(event,'$.action')='task.interrupted')=1 BEGIN SELECT RAISE(ABORT,'owned second expiry audit rejection'); END`); err != nil {
		t.Fatal(err)
	}
	result, err := f.c.SweepExpired(t.Context(), DeadlineSweepCursor{})
	if err == nil || result.Interrupted != 0 || result.Cursor != (DeadlineSweepCursor{}) {
		t.Fatal("failed batch exposed partially committed transitions or cursor", result, err)
	}
	for _, row := range rows {
		deadlineSweepRequireStored(t, f.c, row, false)
	}
	if len(deadlineSweepRawAudit(t, f.c)) != 0 {
		t.Fatal("second audit rejection left the first interruption committed")
	}
	if _, err := f.c.db.ExecContext(t.Context(), "DROP TRIGGER reject_second_expiry"); err != nil {
		t.Fatal(err)
	}
	if result, err = f.c.SweepExpired(t.Context(), DeadlineSweepCursor{}); err != nil || result.Interrupted != len(rows) || len(deadlineSweepRawAudit(t, f.c)) != len(rows) {
		t.Fatal("whole-batch retry could not commit each expiry once", result, err)
	}
}

func TestDeadlineSweepMalformedRowRollsBackEarlierTask(t *testing.T) {
	for _, mode := range []string{"scope", "nul-scope", "record-id", "turn-id", "nul-turn-id", "turn-state", "duplicate-key", "case-folded-state", "escaped-duplicate-state", "unicode-state-alias", "nested-duplicate", "invalid-utf8", "deep", "many-nodes", "tail", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuditFixture(t)
			good := deadlineSweepFixtureRow(t, scope("a-deadline-good"), "", task.Running, time.Now().UTC().Add(-time.Hour))
			bad := deadlineSweepFixtureRow(t, scope("z-deadline-bad"), "", task.Running, time.Now().UTC().Add(-time.Hour))
			switch mode {
			case "scope":
				bad.address.Scope.Tenant = "bad/tenant"
			case "nul-scope":
				bad.address.Scope.Adapter += "\x00suffix"
			case "record-id":
				bad.raw = strings.Replace(bad.raw, `"id":"logical-1"`, `"id":"another-runtime"`, 1)
			case "turn-id", "nul-turn-id", "turn-state":
				bad = deadlineSweepFixtureRow(t, bad.address.Scope, uuid.NewString(), task.Running, time.Now().UTC().Add(-time.Hour))
				if mode == "turn-id" {
					bad.address.turn = uuid.NewString()
				} else if mode == "nul-turn-id" {
					bad.address.turn += "\x00suffix"
				} else {
					bad.state = task.Queued
				}
			case "duplicate-key":
				bad.raw = strings.Replace(bad.raw, `"legacy_extension"`, `"state":"succeeded","legacy_extension"`, 1)
			case "case-folded-state":
				bad.raw = strings.Replace(bad.raw, `"legacy_extension"`, `"State":"succeeded","legacy_extension"`, 1)
			case "escaped-duplicate-state":
				bad.raw = strings.Replace(bad.raw, `"legacy_extension"`, `"st\u0061te":"succeeded","legacy_extension"`, 1)
			case "unicode-state-alias":
				bad.raw = strings.Replace(bad.raw, `"legacy_extension"`, `"ſtate":"succeeded","legacy_extension"`, 1)
			case "nested-duplicate":
				bad.raw = strings.Replace(bad.raw, `"nested":[1,true,null]`, `"nested":{"x":1,"x":2}`, 1)
			case "invalid-utf8":
				bad.raw = strings.Replace(bad.raw, "original\\u0020bytes", "invalid"+string([]byte{0xff})+"utf8", 1)
			case "deep":
				bad.raw = strings.Replace(bad.raw, `"nested":[1,true,null]`, `"nested":`+strings.Repeat("[", 128)+"0"+strings.Repeat("]", 128), 1)
			case "many-nodes":
				bad.raw = strings.Replace(bad.raw, `"nested":[1,true,null]`, `"nested":[`+strings.Repeat("0,", 1500000)+"0]", 1)
			case "tail":
				bad.raw += "{}"
			case "oversized":
				bad.raw = deadlineSweepPad(t, bad.raw, deadlineSweepRecordBytes+1)
			}
			deadlineSweepInsert(t, f.c, good)
			deadlineSweepInsert(t, f.c, bad)
			result, err := f.c.SweepExpired(t.Context(), DeadlineSweepCursor{})
			if err == nil || result.Interrupted != 0 || result.Cursor != (DeadlineSweepCursor{}) {
				t.Fatal("malformed row did not fail the entire bounded batch", mode, result, err)
			}
			deadlineSweepRequireStored(t, f.c, good, false)
			deadlineSweepRequireStored(t, f.c, bad, false)
			if len(deadlineSweepRawAudit(t, f.c)) != 0 {
				t.Fatal("bad row committed an earlier expiry event")
			}
		})
	}
}

func deadlineSweepPad(t *testing.T, raw string, size int) string {
	t.Helper()
	raw = strings.TrimSpace(raw)
	base := strings.TrimSuffix(raw, "}") + `,"unknown_padding":"` + `"}`
	if size < len(base) {
		t.Fatal("padding target is too small")
	}
	return strings.TrimSuffix(raw, "}") + `,"unknown_padding":"` + strings.Repeat("x", size-len(base)) + `"}`
}

func TestDeadlineSweepPerTableByteBudgetStopsBeforeBodyAndCanResume(t *testing.T) {
	for _, turn := range []bool{false, true} {
		t.Run(fmt.Sprint("turn-", turn), func(t *testing.T) {
			f := newAuditFixture(t)
			var rows []deadlineSweepRow
			for i := range 3 {
				id := ""
				if turn {
					id = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
				}
				row := deadlineSweepFixtureRow(t, scope(fmt.Sprintf("deadline-bytes-%d", i)), id, task.Running, time.Now().UTC().Add(time.Hour))
				row.raw = deadlineSweepPad(t, row.raw, 6<<20)
				deadlineSweepInsert(t, f.c, row)
				rows = append(rows, row)
			}
			result, err := f.c.SweepExpired(t.Context(), DeadlineSweepCursor{})
			if err != nil || result.Interrupted != 0 || result.Scanned != 3 || result.RecordBytes != 12<<20 || result.RecordBytes > 2*deadlineSweepBytesPerTable {
				t.Fatal("per-table byte limit fetched an over-budget body or lost metadata accounting", result, err)
			}
			if (turn && (result.TurnsWrapped || result.Cursor.TurnScope != rows[1].address.Prefix() || result.Cursor.TurnID != rows[1].address.turn)) ||
				(!turn && (result.TasksWrapped || result.Cursor.TaskScope != rows[1].address.Prefix())) {
				t.Fatal("byte limit advanced past the body it did not examine", result)
			}
			next, err := f.c.SweepExpired(t.Context(), result.Cursor)
			if err != nil || next.Interrupted != 0 || next.RecordBytes != 6<<20 {
				t.Fatal("next bounded batch did not consume the postponed body", next, err)
			}
			for _, row := range rows {
				deadlineSweepRequireStored(t, f.c, row, false)
			}
		})
	}
}

func TestDeadlineSweepCancellationWhileCatalogMutexIsHeld(t *testing.T) {
	f := newAuditFixture(t)
	row := deadlineSweepFixtureRow(t, scope("deadline-mutex-cancel"), "", task.Running, time.Now().UTC().Add(-time.Hour))
	deadlineSweepInsert(t, f.c, row)
	f.c.mu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	start := time.Now()
	result, err := f.c.SweepExpired(ctx, DeadlineSweepCursor{})
	cancel()
	f.c.mu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second || result.Interrupted != 0 || result.Cursor != (DeadlineSweepCursor{}) {
		t.Fatal("catalog mutex wait ignored cancellation", result, err)
	}
	deadlineSweepRequireStored(t, f.c, row, false)
	if len(deadlineSweepRawAudit(t, f.c)) != 0 {
		t.Fatal("cancelled mutex wait wrote expiry history")
	}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if result, err := f.c.SweepExpired(ctx, DeadlineSweepCursor{}); !errors.Is(err, context.Canceled) || result.Interrupted != 0 {
		t.Fatal("already-cancelled sweep did not refuse work", result, err)
	}
}

func TestDeadlineSweepCASFailureRollsBackTheWholeBatch(t *testing.T) {
	f := newAuditFixture(t)
	first := deadlineSweepFixtureRow(t, scope("a-deadline-cas"), "", task.Running, time.Now().UTC().Add(-time.Hour))
	second := deadlineSweepFixtureRow(t, scope("z-deadline-cas"), "", task.Running, time.Now().UTC().Add(-time.Hour))
	deadlineSweepInsert(t, f.c, first)
	deadlineSweepInsert(t, f.c, second)
	if _, err := f.c.db.ExecContext(t.Context(), "CREATE TRIGGER ignore_second_expiry BEFORE UPDATE ON tasks WHEN OLD.scope='"+second.address.Prefix()+"' BEGIN SELECT RAISE(IGNORE); END"); err != nil {
		t.Fatal(err)
	}
	result, err := f.c.SweepExpired(t.Context(), DeadlineSweepCursor{})
	if err == nil || result.Interrupted != 0 || result.Cursor != (DeadlineSweepCursor{}) || len(deadlineSweepRawAudit(t, f.c)) != 0 {
		t.Fatal("zero-row conditional update committed an earlier transition", result, err)
	}
	deadlineSweepRequireStored(t, f.c, first, false)
	deadlineSweepRequireStored(t, f.c, second, false)
}

func TestDeadlineSweepOneEscapedStateKeyPreservesItsOriginalBytes(t *testing.T) {
	f := newAuditFixture(t)
	row := deadlineSweepFixtureRow(t, scope("deadline-escaped-state"), "", task.Running, time.Now().UTC().Add(-time.Hour))
	row.raw = strings.Replace(row.raw, `"state"`+" \t:", `"st\u0061te"`+" \t:", 1)
	deadlineSweepInsert(t, f.c, row)
	result, err := f.c.SweepExpired(t.Context(), DeadlineSweepCursor{})
	if err != nil || result.Interrupted != 1 {
		t.Fatal("one unambiguous escaped state key could not expire", result, err)
	}
	raw, attempt, _ := deadlineSweepStored(t, f.c, row)
	want := strings.Replace(row.raw, `"runn\u0069ng"`, `"interrupted"`, 1)
	if raw != want || attempt != row.attempt {
		t.Fatal("escaped state key, payload state text or original whitespace was rewritten")
	}
}

type deadlineSweepBeginTrap struct {
	*sql.DB
	begins int
}

func (w *deadlineSweepBeginTrap) BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error) {
	w.begins++
	return nil, errors.New("owned deadline transaction trap")
}

func TestDeadlineSweepNoExpiredRowsNeverStartsAWriteTransaction(t *testing.T) {
	f := newAuditFixture(t)
	row := deadlineSweepFixtureRow(t, scope("deadline-no-write"), "", task.Running, time.Now().UTC().Add(time.Hour))
	deadlineSweepInsert(t, f.c, row)
	trap := &deadlineSweepBeginTrap{DB: f.c.db}
	f.c.writer = trap
	result, err := f.c.SweepExpired(t.Context(), DeadlineSweepCursor{})
	if err != nil || result.Scanned != 1 || result.Interrupted != 0 || trap.begins != 0 {
		t.Fatal("unchanged scan opened a leased write transaction", result, trap.begins, err)
	}
	deadlineSweepRequireStored(t, f.c, row, false)
	// Positive control: the same trap is actually reached for a real expiry.
	expired := deadlineSweepFixtureRow(t, row.address.Scope, "", task.Running, time.Now().UTC().Add(-time.Hour))
	if _, err := f.c.db.ExecContext(t.Context(), "UPDATE tasks SET record=$1,attempt=$2 WHERE scope=$3", expired.raw, expired.attempt, expired.address.Prefix()); err != nil {
		t.Fatal(err)
	}
	if result, err := f.c.SweepExpired(t.Context(), DeadlineSweepCursor{}); err == nil || result.Interrupted != 0 || trap.begins != 1 {
		t.Fatal("transaction trap did not reject the real expiry", result, trap.begins, err)
	}
	deadlineSweepRequireStored(t, f.c, expired, false)
	if len(deadlineSweepRawAudit(t, f.c)) != 0 {
		t.Fatal("failed write transaction left audit evidence")
	}
}

func TestDeadlineSweepCurrentMaximumFieldEnvelopeFitsRecordLimit(t *testing.T) {
	f := newAuditFixture(t)
	s := asset.Scope{Tenant: strings.Repeat("t", 128), Project: strings.Repeat("p", 128), Runtime: strings.Repeat("r", 128), Adapter: strings.Repeat("a", 128)}
	// U+0001 is legal SafePath content, costs six JSON bytes and three URL
	// bytes. Real ContentPath values therefore contribute to the bound too.
	ref := func(kind, prefix string) task.Ref {
		key := asset.Key{Kind: kind, ID: prefix + strings.Repeat("\x01", 4096-len(prefix))}
		if !key.Valid() || len(key.ID) != 4096 {
			t.Fatal("maximum reference fixture is not a legal Key")
		}
		return task.Ref{Key: key, Version: math.MaxInt64, SHA256: strings.Repeat("f", 64), ContentPath: key.ContentPath(math.MaxInt64)}
	}
	pins := make([]task.SkillPin, task.MaxSkills)
	for i := range pins {
		prefix := fmt.Sprintf("original-rules-%02d-", i)
		pins[i] = task.SkillPin{Name: prefix + strings.Repeat("x", 64-len(prefix)), Source: ref("skill", fmt.Sprintf("catalog/skills/%02d/", i))}
	}
	assets := make([]task.Ref, task.MaxArtifacts+2)
	for i := range task.MaxArtifacts {
		assets[i] = ref("artifact", fmt.Sprintf("out/%03d-", i))
	}
	assets[task.MaxArtifacts] = ref("workspace", "root/")
	assets[task.MaxArtifacts+1] = ref("session", "original-session/")
	parent := &task.Checkpoint{SessionID: strings.Repeat("\x01", 128), Session: assets[task.MaxArtifacts+1], Workspace: assets[task.MaxArtifacts], Skills: pins}
	record := task.Record{ID: s.Runtime, State: task.Failed, ExecutionPolicy: task.ExecutionUntrustedIsolated,
		Deadline: time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.FixedZone("maximum-encoded-offset", 23*60*60+59*60)),
		Spec:     task.Spec{Mode: task.FileTurn, Instruction: strings.Repeat("\x01", 8192), Input: strings.Repeat("\x01", 65536), Output: strings.Repeat("\x01", 128)},
		Parent:   parent, Assets: assets, Skills: pins, Error: strings.Repeat("\x01", 256)}
	// This uses every current per-field maximum simultaneously, including
	// Key-level bounds for workspace/session refs that real Finish narrows.
	// It is a conservative envelope, not a claim that one receipt can create
	// every independent maximum. No arbitrary unknown padding is added.
	if !s.Valid() || !record.Spec.Valid() || !asset.SafePath(parent.SessionID) || len(record.Assets) != task.MaxArtifacts+2 ||
		task.ValidSkillPins(record.Skills) != nil || task.ValidSkillPins(parent.Skills) != nil {
		t.Fatal("maximum field envelope violates the current task schema")
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) >= deadlineSweepRecordBytes {
		t.Fatal("current legal field envelope no longer fits the sweep record limit", len(raw), deadlineSweepRecordBytes, err)
	}
	row := deadlineSweepRow{address: taskAddress{Scope: s}, raw: string(raw), attempt: uuid.NewString(), state: record.State}
	deadlineSweepInsert(t, f.c, row)
	trap := &deadlineSweepBeginTrap{DB: f.c.db}
	f.c.writer = trap
	result, err := f.c.SweepExpired(t.Context(), DeadlineSweepCursor{})
	if err != nil || result.Scanned != 1 || result.RecordBytes != int64(len(raw)) || result.Interrupted != 0 || trap.begins != 0 {
		t.Fatal("current maximum field envelope was rejected or rewritten", result, trap.begins, err)
	}
	deadlineSweepRequireStored(t, f.c, row, false)
	if len(deadlineSweepRawAudit(t, f.c)) != 0 {
		t.Fatal("terminal maximum record acquired an expiry event")
	}
	t.Logf("DEADLINE_SWEEP_MAX_RECORD assets=%d record_pins=%d parent_pins=%d json_bytes=%d limit_bytes=%d scanned=1 write_transactions=0 original_bytes=true", len(assets), len(record.Skills), len(parent.Skills), len(raw), deadlineSweepRecordBytes)
}
