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

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
)

const (
	deadlineSweepRowsPerTable  = 32
	deadlineSweepRecordBytes   = 8 << 20
	deadlineSweepBytesPerTable = 16 << 20
	deadlineSweepBudget        = 2 * time.Second
	deadlineSweepInterval      = time.Second
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

func (c *Catalog) deadlineKeys(ctx context.Context, cursor DeadlineSweepCursor, turns bool) ([]deadlineKey, error) {
	query := "SELECT substr(scope,1,516) FROM tasks WHERE scope>$1 ORDER BY scope LIMIT $2"
	args := []any{cursor.TaskScope, deadlineSweepRowsPerTable}
	if turns {
		query = "SELECT substr(scope,1,516),substr(id,1,37) FROM turns WHERE (scope,id)>($1,$2) ORDER BY scope,id LIMIT $3"
		args = []any{cursor.TurnScope, cursor.TurnID, deadlineSweepRowsPerTable}
	}
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []deadlineKey{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var key deadlineKey
		if turns {
			err = rows.Scan(&key.scope, &key.turn)
		} else {
			err = rows.Scan(&key.scope)
		}
		if err != nil {
			return nil, err
		}
		if _, err := deadlineScope(key.scope); err != nil || len(key.scope) > 515 || turns && !sandboxUUID(key.turn) {
			return nil, ErrDeadlineSweep
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Bytea/blob is scanned directly. Oversized records are identified by their
// original byte length; truncated UTF-8 is never converted back into SQL text
// or passed to the JSON parser. One row is materialized at a time.
func (c *Catalog) deadlineRecord(ctx context.Context, key deadlineKey, budget int64) (taskSlot, []byte, int64, string, error) {
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
	row := c.db.QueryRowContext(ctx, query, args...)
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

// SweepExpired is a trusted lifecycle operation, not an HTTP operation. It
// performs no dispatch, execution, capability cleanup or checkpoint mutation.
// Any failed transition rolls back this entire bounded batch and its audits.
func (c *Catalog) SweepExpired(ctx context.Context, cursor DeadlineSweepCursor) (DeadlineSweepResult, error) {
	result := DeadlineSweepResult{Cursor: cursor}
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
	tasks, err := c.deadlineKeys(ctx, cursor, false)
	if err != nil {
		return result, err
	}
	turns, err := c.deadlineKeys(ctx, cursor, true)
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
		all := true
		for _, key := range group.keys {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			result.Scanned++
			slot, raw, size, state, err := c.deadlineRecord(ctx, key, deadlineSweepBytesPerTable-used)
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
	if len(changes) == 0 {
		result.Cursor = next
		return result, ctx.Err()
	}
	// Deliberately replace any request context with a fixed service identity.
	ctx = context.WithValue(ctx, auditActorContext{}, AuditActor{Source: "deadline-controller", Role: "system"})
	tx, err := c.writer.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		args := []any{string(change.updated), change.key.scope, string(change.raw), change.before.attempt}
		query := "UPDATE tasks SET record=$1 WHERE scope=$2 AND record=$3 AND attempt=$4"
		if change.key.turn != "" {
			args = []any{string(change.updated), task.Interrupted, change.key.scope, change.key.turn, string(change.raw), change.before.attempt, change.before.State}
			query = "UPDATE turns SET record=$1,state=$2 WHERE scope=$3 AND id=$4 AND record=$5 AND attempt=$6 AND state=$7"
		}
		updated, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return result, err
		}
		if n, err := updated.RowsAffected(); err != nil || n != 1 {
			return result, errors.Join(ErrConflict, err)
		}
		scope, _ := deadlineScope(change.key.scope)
		if err := appendAudit(ctx, tx, auditTaskEvent(ctx, taskAddress{scope, change.key.turn}, change.before, change.after)); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	result.Cursor, result.Interrupted = next, len(changes)
	return result, nil
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
