package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
)

// The acceptance record is also the durable dispatch intent. All state changes
// are serialized by the catalog's existing exclusive writer lease and mutex.
func (c *Catalog) submitTurn(ctx context.Context, a taskAddress, q task.Question) (task.Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, err := c.readTask(ctx, a)
	if err == nil {
		if existing.Parent.SessionID != q.SessionID || existing.Spec.Instruction != q.Message {
			return task.Record{}, ErrConflict
		}
		return existing.Record, nil
	}
	if !errors.Is(err, ErrMissing) {
		return task.Record{}, err
	}
	var seq int64
	var latest string
	err = c.writer.QueryRowContext(ctx, "SELECT seq,id FROM turns WHERE scope=$1 ORDER BY seq DESC LIMIT 1", a.Prefix()).Scan(&seq, &latest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return task.Record{}, err
	}
	if latest != "" {
		last, err := c.readTask(ctx, taskAddress{a.Scope, latest})
		if err != nil {
			return task.Record{}, err
		}
		switch last.State {
		case task.Queued, task.Running, task.Cancelling:
			return task.Record{}, ErrConflict
		}
	}
	cp, err := c.latestCheckpoint(ctx, a.Scope)
	if err != nil {
		return task.Record{}, err
	}
	if q.SessionID != cp.SessionID {
		return task.Record{}, ErrMissing
	}
	parentTask, err := c.readTask(ctx, taskAddress{Scope: a.Scope})
	if err != nil {
		return task.Record{}, err
	}
	spec := task.Spec{Instruction: q.Message, Output: task.ReplyFile}
	if parentTask.Spec.Mode == task.FileTurn {
		spec.Mode, spec.Output = task.FileTurn, parentTask.Spec.Output
	}
	rec := task.Record{
		ID: a.Runtime, TurnID: a.turn, State: task.Queued,
		Deadline: time.Now().UTC().Add(task.Timeout),
		Spec:     spec,
		Parent:   &cp,
		Skills:   slices.Clone(cp.Skills),
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return task.Record{}, err
	}
	_, err = c.writer.ExecContext(ctx, "INSERT INTO turns(scope,id,seq,state,record,attempt) VALUES ($1,$2,$3,$4,$5,'')", a.Prefix(), a.turn, seq+1, rec.State, string(raw))
	return rec, err
}

// Ensure must identify the Job by (scope, turn), even after an ambiguous create.
func (c *Catalog) DispatchQueued(ctx context.Context, s asset.Scope, ensure func(context.Context, asset.Scope, string) (bool, error)) error {
	var id string
	err := c.writer.QueryRowContext(ctx, "SELECT id FROM turns WHERE scope=$1 AND state=$2 ORDER BY seq DESC LIMIT 1", s.Prefix(), task.Queued).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	a := taskAddress{s, id}
	slot, err := c.readTask(ctx, a)
	if err != nil {
		return err
	}
	if slot.State != task.Interrupted {
		if slot.State != task.Queued {
			return nil
		}
		ended, err := ensure(ctx, s, id)
		if err != nil || !ended {
			return err
		}
	}
	_, err = c.changeTask(ctx, a, false, func(row *taskSlot) error {
		if row.State == task.Queued {
			row.State = task.Failed
			row.Error = "worker Job ended before claiming its turn"
		}
		return nil
	})
	return err
}

func (c *Catalog) latestCheckpoint(ctx context.Context, scope asset.Scope) (task.Checkpoint, error) {
	var previous string
	err := c.writer.QueryRowContext(ctx, "SELECT id FROM turns WHERE scope=$1 AND state=$2 ORDER BY seq DESC LIMIT 1", scope.Prefix(), task.Succeeded).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return task.Checkpoint{}, err
	}
	source, err := c.readTask(ctx, taskAddress{scope, previous})
	if err != nil {
		return task.Checkpoint{}, err
	}
	if source.State != task.Succeeded {
		return task.Checkpoint{}, ErrConflict
	}
	cp := task.Checkpoint{Skills: slices.Clone(source.Skills)}
	for _, ref := range source.Assets {
		switch ref.Kind {
		case "session":
			cp.SessionID = ref.ID
			if source.Parent != nil {
				cp.SessionID = source.Parent.SessionID
			}
			cp.Session = ref
		case "workspace":
			cp.Workspace = ref
		}
	}
	if cp.Session.Version <= 0 || cp.Workspace.Version <= 0 {
		return task.Checkpoint{}, ErrNotReady
	}
	return cp, nil
}
