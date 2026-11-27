package platform

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/task"
)

const (
	deadlineSweepRowsPerTable   = 32
	deadlineSweepRecordBytes    = 8 << 20
	deadlineSweepBytesPerTable  = 16 << 20
	deadlineSweepBudget         = 2 * time.Second
	deadlineSweepInterval       = time.Second
	deadlineSweepWritesPerTable = 4
	deadlineSweepWriteBudget    = 2 * time.Second
	deadlineSweepWriteIO        = 4 * time.Second
	deadlineSweepStatementMS    = 250
)

var ErrDeadlineSweep = errors.New("deadline sweep record or cursor is invalid")

// DeadlineSweepCursor belongs to the trusted service, never to a tenant API.
// Cursors are independent keysets over the existing primary keys. Restarting
// from zero is safe; large inventories take multiple bounded passes to revisit.
type DeadlineSweepCursor struct {
	TaskScope, TurnScope, TurnID string
}

type DeadlineSweepResult struct {
	Cursor                     DeadlineSweepCursor
	Scanned, Interrupted       int
	RecordBytes                int64
	TasksWrapped, TurnsWrapped bool
}

type deadlineKey struct{ scope, turn string }
type deadlineChange struct {
	key           deadlineKey
	before, after taskSlot
	raw, updated  []byte
}

// JSON field matching uses Unicode simple folding, including long s/Kelvin
// aliases. Lowercasing alone would not reject every ambiguous State spelling.
func deadlineField(ctx context.Context, key string) (string, error) {
	var folded strings.Builder
	i := 0
	for _, r := range key {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		i++
		lowest := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			lowest = min(lowest, next)
		}
		folded.WriteRune(lowest)
	}
	return folded.String(), nil
}

func deadlineScope(prefix string) (asset.Scope, error) {
	if len(prefix) > 515 {
		return asset.Scope{}, ErrDeadlineSweep
	}
	parts := strings.Split(prefix, "/")
	if len(parts) != 4 {
		return asset.Scope{}, ErrDeadlineSweep
	}
	s := asset.Scope{Tenant: parts[0], Project: parts[1], Runtime: parts[2], Adapter: parts[3]}
	if !s.Valid() || s.Prefix() != prefix {
		return asset.Scope{}, ErrDeadlineSweep
	}
	return s, nil
}

// Keys use byte prefixes and original byte lengths too: SQLite TEXT substr
// stops at NUL and must never turn a corrupt key into a valid allocation prefix.
func (c *Catalog) deadlineKeys(ctx context.Context, reader deadlineSweepReader, cursor DeadlineSweepCursor, turns bool) ([]deadlineKey, error) {
	scopeSize, scopeBytes := "length(CAST(scope AS BLOB))", "substr(CAST(scope AS BLOB),1,516)"
	idSize, idBytes := "length(CAST(id AS BLOB))", "substr(CAST(id AS BLOB),1,37)"
	if c.postgres {
		scopeSize, scopeBytes = "octet_length(scope)", "substring(convert_to(scope,'UTF8') from 1 for 516)"
		idSize, idBytes = "octet_length(id)", "substring(convert_to(id,'UTF8') from 1 for 37)"
	}
	query := "SELECT " + scopeSize + "," + scopeBytes + " FROM tasks WHERE scope>$1 ORDER BY scope LIMIT $2"
	args := []any{cursor.TaskScope, deadlineSweepRowsPerTable}
	if turns {
		query = "SELECT " + scopeSize + "," + scopeBytes + "," + idSize + "," + idBytes + " FROM turns WHERE (scope,id)>($1,$2) ORDER BY scope,id LIMIT $3"
		args = []any{cursor.TurnScope, cursor.TurnID, deadlineSweepRowsPerTable}
	}
	rows, err := reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []deadlineKey{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var scopeRaw, idRaw []byte
		var scopeLength, idLength int64
		if turns {
			err = rows.Scan(&scopeLength, &scopeRaw, &idLength, &idRaw)
		} else {
			err = rows.Scan(&scopeLength, &scopeRaw)
		}
		if err != nil {
			return nil, err
		}
		if scopeLength < 1 || scopeLength > 515 || int64(len(scopeRaw)) != scopeLength || turns && (idLength != 36 || int64(len(idRaw)) != idLength) {
			return nil, ErrDeadlineSweep
		}
		key := deadlineKey{string(scopeRaw), string(idRaw)}
		if _, err := deadlineScope(key.scope); err != nil || turns && !sandboxUUID(key.turn) {
			return nil, ErrDeadlineSweep
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Bytea/blob is scanned directly. Oversized records are identified by their
// original byte length; truncated UTF-8 is never converted back into SQL text
// or passed to the JSON parser. One row is materialized at a time.
func (c *Catalog) deadlineRecord(ctx context.Context, reader deadlineSweepReader, key deadlineKey, budget int64) (taskSlot, []byte, int64, string, error) {
	size := "length(CAST(record AS BLOB))"
	body := "substr(CAST(record AS BLOB),1,$1)"
	if c.postgres {
		size = "octet_length(record)"
		body = "substring(convert_to(record,'UTF8') from 1 for $1)"
	}
	limit := min(budget, int64(deadlineSweepRecordBytes))
	query := "SELECT " + size + ",CASE WHEN " + size + "<=$1 THEN " + body + " ELSE NULL END,substr(attempt,1,65)"
	args := []any{limit, key.scope}
	if key.turn != "" {
		query += ",substr(state,1,16) FROM turns WHERE scope=$2 AND id=$3"
		args = append(args, key.turn)
	} else {
		query += " FROM tasks WHERE scope=$2"
	}
	var slot taskSlot
	var raw []byte
	var length int64
	var columnState string
	row := reader.QueryRowContext(ctx, query, args...)
	var err error
	if key.turn == "" {
		err = row.Scan(&length, &raw, &slot.attempt)
	} else {
		err = row.Scan(&length, &raw, &slot.attempt, &columnState)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return slot, nil, 0, columnState, ErrMissing
	}
	if err != nil {
		return slot, nil, 0, columnState, err
	}
	if length < 1 || length > deadlineSweepRecordBytes || len(slot.attempt) > 64 {
		return slot, nil, length, columnState, ErrDeadlineSweep
	}
	if length > budget {
		return slot, nil, length, columnState, nil
	}
	if int64(len(raw)) != length {
		return slot, nil, length, columnState, ErrDeadlineSweep
	}
	return slot, raw, length, columnState, nil
}

// Preserve every byte outside the top-level state value, including legacy
// omission/formatting and unknown fields. Duplicate members, invalid UTF-8 and
// structurally excessive JSON fail closed instead of being normalized.
func deadlineJSON(ctx context.Context, raw []byte) (task.Record, []byte, error) {
	var record task.Record
	if len(raw) == 0 || len(raw) > deadlineSweepRecordBytes || !utf8.Valid(raw) {
		return record, nil, ErrDeadlineSweep
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, end, nodes := -1, -1, 0
	var walk func(int) error
	walk = func(depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		nodes++
		if depth > 32 || nodes > 8192 {
			return ErrDeadlineSweep
		}
		token, err := decoder.Token()
		if err != nil {
			return ErrDeadlineSweep
		}
		delim, container := token.(json.Delim)
		if !container {
			if depth == 0 {
				return ErrDeadlineSweep
			}
			return nil
		}
		if delim == '[' && depth != 0 {
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			if token, err = decoder.Token(); err != nil || token != json.Delim(']') {
				return ErrDeadlineSweep
			}
			return nil
		}
		if delim != '{' {
			return ErrDeadlineSweep
		}
		seen := map[string]bool{}
		for decoder.More() {
			if err := ctx.Err(); err != nil {
				return err
			}
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok || key == "" {
				return ErrDeadlineSweep
			}
			folded, err := deadlineField(ctx, key)
			if err != nil {
				return err
			}
			if seen[folded] {
				return ErrDeadlineSweep
			}
			seen[folded] = true
			if depth == 0 && key == "state" {
				var value json.RawMessage
				if decoder.Decode(&value) != nil {
					return ErrDeadlineSweep
				}
				end = int(decoder.InputOffset())
				start = end - len(value)
				if start < 0 || end > len(raw) || !bytes.Equal(raw[start:end], value) {
					return ErrDeadlineSweep
				}
			} else if err := walk(depth + 1); err != nil {
				return err
			}
		}
		if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
			return ErrDeadlineSweep
		}
		return nil
	}
	if err := walk(0); err != nil {
		return record, nil, err
	}
	if _, err := decoder.Token(); err != io.EOF || start < 0 {
		return record, nil, ErrDeadlineSweep
	}
	if err := ctx.Err(); err != nil {
		return record, nil, err
	}
	if json.Unmarshal(raw, &record) != nil || !record.Spec.Valid() || task.ValidSkillPins(record.Skills) != nil {
		return record, nil, ErrDeadlineSweep
	}
	switch record.State {
	case task.Queued, task.Running, task.Cancelling, task.Cancelled, task.Succeeded, task.Failed, task.Interrupted:
	default:
		return record, nil, ErrDeadlineSweep
	}
	updated := make([]byte, 0, len(raw)+16)
	updated = append(updated, raw[:start]...)
	updated = append(updated, `"interrupted"`...)
	updated = append(updated, raw[end:]...)
	return record, updated, ctx.Err()
}

func (c *Catalog) lockDeadlineSweep(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// A PostgreSQL maintenance scan borrows one reader with its own socket window.
// No ordinary pooled query inherits this deadline. Failed readers are discarded;
// the independently leased writer remains untouched.
type deadlineSweepReader interface {
	querier
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (c *Catalog) openDeadlineReader(ctx context.Context) (deadlineSweepReader, func() error, error) {
	if !c.postgres {
		return c.db, func() error { return nil }, nil
	}
	deadline, _ := ctx.Deadline()
	connect := context.WithValue(ctx, postgresMaintenanceConnectContext{}, deadline)
	conn, err := c.db.Conn(connect)
	if err != nil {
		return nil, nil, err
	}
	closeReader := func() error {
		err := setPostgresMaintenance(conn, time.Time{})
		closeErr := conn.Close()
		if errors.Is(closeErr, sql.ErrConnDone) {
			closeErr = nil // Raw already discarded an invalid reader.
		}
		return errors.Join(err, closeErr)
	}
	if err := setPostgresMaintenance(conn, deadline); err != nil {
		return nil, nil, errors.Join(err, closeReader())
	}
	return conn, closeReader, nil
}

// SweepExpired is a trusted lifecycle operation, not an HTTP operation. It
// performs no dispatch, execution, capability cleanup or checkpoint mutation.
// Any failed transition rolls back this entire bounded batch and its audits.
func (c *Catalog) SweepExpired(ctx context.Context, cursor DeadlineSweepCursor) (result DeadlineSweepResult, resultErr error) {
	result = DeadlineSweepResult{Cursor: cursor}
	caller := ctx
	ctx, cancel := context.WithTimeout(ctx, deadlineSweepBudget)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	for _, prefix := range []string{cursor.TaskScope, cursor.TurnScope} {
		if prefix != "" {
			if _, err := deadlineScope(prefix); err != nil {
				return result, err
			}
		}
	}
	if (cursor.TurnScope == "") != (cursor.TurnID == "") || cursor.TurnID != "" && !sandboxUUID(cursor.TurnID) {
		return result, ErrDeadlineSweep
	}
	reader, closeReader, err := c.openDeadlineReader(ctx)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeReader != nil {
			resultErr = errors.Join(resultErr, closeReader())
		}
	}()
	tasks, err := c.deadlineKeys(ctx, reader, cursor, false)
	if err != nil {
		return result, err
	}
	turns, err := c.deadlineKeys(ctx, reader, cursor, true)
	if err != nil {
		return result, err
	}
	if err := c.lockDeadlineSweep(ctx); err != nil {
		return result, err
	}
	defer c.mu.Unlock()
	next := cursor
	changes := []deadlineChange{}
	for _, group := range []struct {
		keys  []deadlineKey
		turns bool
	}{{tasks, false}, {turns, true}} {
		var used int64
		writes := 0
		all := true
		for _, key := range group.keys {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			result.Scanned++
			slot, raw, size, state, err := c.deadlineRecord(ctx, reader, key, deadlineSweepBytesPerTable-used)
			if err != nil && !errors.Is(err, ErrMissing) {
				return result, err
			}
			if size > deadlineSweepBytesPerTable-used {
				all = false
				break
			}
			if !errors.Is(err, ErrMissing) {
				used += size
				result.RecordBytes += size
				record, updated, err := deadlineJSON(ctx, raw)
				if err != nil {
					return result, err
				}
				scope, _ := deadlineScope(key.scope)
				if record.ID != scope.Runtime || record.TurnID != key.turn || key.turn != "" && (record.Parent == nil || state != record.State) {
					return result, ErrDeadlineSweep
				}
				slot.Record = record
				if (record.State == task.Running || record.State == task.Cancelling || key.turn != "" && record.State == task.Queued) && !time.Now().Before(record.Deadline) {
					if writes == deadlineSweepWritesPerTable {
						all = false
						break
					}
					after := slot
					after.State = task.Interrupted
					var patched task.Record
					if json.Unmarshal(updated, &patched) != nil || !reflect.DeepEqual(patched, after.Record) {
						return result, ErrDeadlineSweep
					}
					if err := ctx.Err(); err != nil {
						return result, err
					}
					changes = append(changes, deadlineChange{key, slot, after, raw, updated})
					writes++
				}
			}
			if group.turns {
				next.TurnScope, next.TurnID = key.scope, key.turn
			} else {
				next.TaskScope = key.scope
			}
		}
		if all && len(group.keys) < deadlineSweepRowsPerTable {
			if group.turns {
				next.TurnScope, next.TurnID, result.TurnsWrapped = "", "", true
			} else {
				next.TaskScope, result.TasksWrapped = "", true
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// Return the reader before independent write work can consume its window.
	readErr := closeReader()
	closeReader = nil
	if readErr != nil {
		return result, readErr
	}
	if len(changes) == 0 {
		result.Cursor = next
		return result, nil
	}
	if err := c.writeDeadlineSweep(caller, changes); err != nil {
		return result, err
	}
	result.Cursor, result.Interrupted = next, len(changes)
	return result, nil
}

// Read/parse time must not consume the leased writer's I/O deadline. PostgreSQL
// statement/lock timeouts abort ordinary slow work without closing the session
// that owns the advisory lease. The hard context is only an I/O failure fuse.
func (c *Catalog) writeDeadlineSweep(caller context.Context, changes []deadlineChange) (resultErr error) {
	ctx := caller
	duration := deadlineSweepWriteBudget
	if c.postgres {
		ctx, duration = context.WithoutCancel(caller), deadlineSweepWriteIO
	}
	hardEnd := time.Now().Add(duration)
	ctx, cancel := context.WithDeadline(ctx, hardEnd)
	defer cancel()
	if c.postgres {
		// pq begins the transaction before installing its context watcher. The
		// owned socket deadline also bounds BEGIN, COMMIT and ROLLBACK transport.
		defer func() { resultErr = errors.Join(resultErr, c.setPostgresMaintenance(time.Time{})) }()
		if err := c.setPostgresMaintenance(hardEnd); err != nil {
			return err
		}
	}
	softEnd := time.Now().Add(deadlineSweepWriteBudget)
	check := func() error {
		if err := caller.Err(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(softEnd) {
			return context.DeadlineExceeded
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	// Deliberately replace request identity; the caller is a trusted lifecycle.
	ctx = context.WithValue(ctx, auditActorContext{}, AuditActor{Source: "deadline-controller", Role: "system"})
	ctx = context.WithValue(ctx, auditWriteStepContext{}, check)
	tx, err := c.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// This executes before cancel(), so soft budget/caller cancellation performs
	// a normal rollback instead of cancelling lib/pq's leased connection.
	defer tx.Rollback()
	if c.postgres {
		if err := check(); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `SELECT set_config(name,
   (CASE WHEN setting::bigint=0 THEN $1 ELSE LEAST(setting::bigint,$1) END)::text,true)
   FROM pg_settings WHERE name IN ('statement_timeout','lock_timeout')`, deadlineSweepStatementMS)
		if err != nil {
			return err
		}
	}
	for _, change := range changes {
		if err := check(); err != nil {
			return err
		}
		args := []any{string(change.updated), change.key.scope, string(change.raw), change.before.attempt}
		query := "UPDATE tasks SET record=$1 WHERE scope=$2 AND record=$3 AND attempt=$4"
		if change.key.turn != "" {
			args = []any{string(change.updated), task.Interrupted, change.key.scope, change.key.turn, string(change.raw), change.before.attempt, change.before.State}
			query = "UPDATE turns SET record=$1,state=$2 WHERE scope=$3 AND id=$4 AND record=$5 AND attempt=$6 AND state=$7"
		}
		updated, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		if n, err := updated.RowsAffected(); err != nil || n != 1 {
			return errors.Join(ErrConflict, err)
		}
		if err := check(); err != nil {
			return err
		}
		scope, _ := deadlineScope(change.key.scope)
		if err := appendAudit(ctx, tx, auditTaskEvent(ctx, taskAddress{scope, change.key.turn}, change.before, change.after)); err != nil {
			return err
		}
	}
	if err := check(); err != nil {
		return err
	}
	return tx.Commit()
}

// RunDeadlineController owns no goroutines. The caller starts and joins its
// single lifecycle loop before closing Catalog. Reports carry no raw error,
// record, identifier, credential or caller-provided identity.
func (c *Catalog) RunDeadlineController(ctx context.Context, report func()) {
	var cursor DeadlineSweepCursor
	timer := time.NewTimer(deadlineSweepInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		result, err := c.SweepExpired(ctx, cursor)
		if err == nil {
			cursor = result.Cursor
		} else if ctx.Err() == nil && report != nil {
			report()
		}
		timer.Reset(deadlineSweepInterval)
	}
}
