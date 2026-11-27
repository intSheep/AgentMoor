package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/task"
)

type taskAddress struct {
	asset.Scope
	turn string
}

type taskSlot struct {
	task.Record
	attempt string
	// Present only after finishTask validates the successful receipt. Never
	// persisted in the task or inferred from an arbitrary asset ID/path.
	linkedSessionID string
	acceptedSandbox *SandboxRun // ephemeral verified source; never stored in Record
}

// Each logical runtime is one allocation. A claim token is never exposed by
// the read API. A crashed worker's expired allocation cannot be claimed again.
func (c *Catalog) readTask(ctx context.Context, s taskAddress) (taskSlot, error) {
	slot, err := c.readStoredTask(ctx, s)
	if err != nil {
		return slot, err
	}
	if (slot.State == task.Running || slot.State == task.Cancelling || (s.turn != "" && slot.State == task.Queued)) && !time.Now().Before(slot.Deadline) {
		slot.State = task.Interrupted
	}
	return slot, nil
}

func (c *Catalog) readStoredTask(ctx context.Context, s taskAddress) (taskSlot, error) {
	var slot taskSlot
	var raw string
	// HTTP polling may overlap or be cancelled independently of a mutation.
	// Keep those reads off the connection owning the exclusive writer lease.
	row := c.db.QueryRowContext
	var res *sql.Row
	if s.turn == "" {
		res = row(ctx, "SELECT record,attempt FROM tasks WHERE scope=$1", s.Prefix())
	} else {
		res = row(ctx, "SELECT record,attempt FROM turns WHERE scope=$1 AND id=$2", s.Prefix(), s.turn)
	}
	err := res.Scan(&raw, &slot.attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return slot, ErrMissing
	}
	if err != nil {
		return slot, err
	}
	if err = json.Unmarshal([]byte(raw), &slot.Record); err != nil {
		return slot, err
	}
	return slot, nil
}

// The service owns one catalog writer. Reading the head revisions, checking
// a transition, and publishing its new record happen under the same lock.
func (c *Catalog) changeTask(ctx context.Context, s taskAddress, create bool, change func(*taskSlot) error) (task.Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	before, err := c.readStoredTask(ctx, s)
	if err != nil && !(create && errors.Is(err, ErrMissing)) {
		return task.Record{}, err
	}
	slot := before
	if (slot.State == task.Running || slot.State == task.Cancelling || (s.turn != "" && slot.State == task.Queued)) && !time.Now().Before(slot.Deadline) {
		slot.State = task.Interrupted
	}
	if err = change(&slot); err != nil {
		return task.Record{}, err
	}
	b, err := json.Marshal(slot.Record)
	if err != nil {
		return task.Record{}, err
	}
	// A first successful Finish must commit within the allocation's original
	// deadline, including time spent waiting on state/audit SQL. Completed
	// acknowledgement replays and failure/cancellation keep their semantics.
	newSuccess := before.State == task.Running && slot.State == task.Succeeded
	if newSuccess {
		if slot.ExecutionPolicy == task.ExecutionUntrustedIsolated && slot.acceptedSandbox == nil {
			return task.Record{}, ErrConflict
		}
		if !time.Now().Before(before.Deadline) {
			return task.Record{}, ErrConflict
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, before.Deadline)
		defer cancel()
	}
	tx, err := c.writer.BeginTx(ctx, nil)
	if err != nil {
		return task.Record{}, err
	}
	defer tx.Rollback()
	if s.turn != "" {
		_, err = tx.ExecContext(ctx, "UPDATE turns SET record=$1,attempt=$2,state=$3 WHERE scope=$4 AND id=$5", string(b), slot.attempt, slot.State, s.Prefix(), s.turn)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO tasks(scope,record,attempt) VALUES ($1,$2,$3)
 ON CONFLICT(scope) DO UPDATE SET record=excluded.record,attempt=excluded.attempt`, s.Prefix(), string(b), slot.attempt)
	}
	if err != nil {
		return task.Record{}, err
	}
	if before.State != slot.State {
		if err = appendAudit(ctx, tx, auditTaskEvent(ctx, s, before, slot)); err != nil {
			return task.Record{}, err
		}
		if slot.State == task.Succeeded {
			if slot.acceptedSandbox != nil {
				if err = appendSandboxResultAccepted(ctx, tx, s, slot, *slot.acceptedSandbox); err != nil {
					return task.Record{}, err
				}
			}
			if err = appendAssetLinks(ctx, tx, s, slot); err != nil {
				return task.Record{}, err
			}
		}
	}
	if newSuccess && !time.Now().Before(before.Deadline) {
		return task.Record{}, ErrConflict
	}
	return slot.Record, tx.Commit()
}

func (c *Catalog) submitTask(ctx context.Context, s taskAddress, spec task.Spec) (task.Record, error) {
	return c.changeTask(ctx, s, true, func(slot *taskSlot) error {
		if slot.State == "" {
			slot.Record = task.Record{ID: s.Runtime, State: task.Queued, Spec: spec}
			return nil
		}
		if spec != slot.Spec {
			return ErrConflict
		}
		return nil
	})
}
func (c *Catalog) claimTask(ctx context.Context, s taskAddress, claim task.Claim) (task.Record, error) {
	policy := claim.ExecutionPolicy
	if policy == "" {
		policy = task.ExecutionTrustedLocal
	}
	if uuid.Validate(claim.Attempt) != nil || task.ValidSkillPins(claim.Skills) != nil {
		return task.Record{}, errBadRequest
	}
	if policy != task.ExecutionTrustedLocal && policy != task.ExecutionUntrustedIsolated {
		return task.Record{}, errBadRequest
	}
	return c.changeTask(ctx, s, false, func(slot *taskSlot) error {
		if slot.State == task.Running && slot.attempt == claim.Attempt {
			previous := slot.ExecutionPolicy
			if previous == "" {
				previous = task.ExecutionTrustedLocal
			}
			if !slices.Equal(slot.Skills, claim.Skills) || previous != policy {
				return ErrConflict
			}
			return nil
		}
		if slot.State != task.Queued {
			return ErrConflict
		}
		// Continuations inherit the successful checkpoint's exact choices. A
		// replacement worker cannot upgrade or omit a pin via changed config.
		if slot.Parent != nil && !slices.Equal(slot.Skills, claim.Skills) {
			return ErrConflict
		}
		for _, pin := range claim.Skills {
			var raw string
			ref := pin.Source
			err := c.writer.QueryRowContext(ctx, "SELECT metadata FROM revisions WHERE scope=$1 AND kind=$2 AND asset_id=$3 AND version=$4", s.Prefix(), ref.Kind, ref.ID, ref.Version).Scan(&raw)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotReady
			}
			if err != nil {
				return err
			}
			var m asset.Metadata
			if err = json.Unmarshal([]byte(raw), &m); err != nil {
				return err
			}
			if m.SHA256 != ref.SHA256 {
				return ErrConflict
			}
		}
		slot.Skills = slices.Clone(claim.Skills)
		slot.ExecutionPolicy = policy
		slot.State = task.Running
		slot.attempt = claim.Attempt
		slot.Deadline = time.Now().UTC().Add(task.Timeout)
		return nil
	})
}
func (c *Catalog) cancelTask(ctx context.Context, s taskAddress) (task.Record, error) {
	return c.changeTask(ctx, s, false, func(slot *taskSlot) error {
		switch slot.State {
		case task.Queued:
			slot.State = task.Cancelled
		case task.Running:
			slot.State = task.Cancelling
		case task.Cancelling, task.Cancelled:
		default:
			return ErrConflict
		}
		return nil
	})
}
func (c *Catalog) finishTask(ctx context.Context, s taskAddress, finish task.Finish) (task.Record, error) {
	if (finish.Error == "") == (finish.SessionID == "") || len(finish.Error) > 256 || len(finish.Artifacts) > task.MaxArtifacts || (finish.Error != "" && len(finish.Artifacts) != 0) {
		return task.Record{}, errBadRequest
	}
	evidence, err := c.prepareSandboxSuccess(ctx, s, finish)
	if err != nil {
		return task.Record{}, err
	}
	if evidence != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, evidence.run.Deadline)
		defer cancel()
	}
	return c.changeTask(ctx, s, false, func(slot *taskSlot) error {
		if slot.attempt == "" || slot.attempt != finish.Attempt {
			return ErrConflict
		}
		switch slot.State {
		case task.Cancelled, task.Cancelling:
			slot.State = task.Cancelled
			return nil
		case task.Failed:
			if slot.Error == finish.Error {
				return nil
			}
			return ErrConflict
		case task.Succeeded:
			for _, ref := range slot.Assets {
				if ref.Key == task.TurnKey(s.turn, asset.Key{Kind: "session", ID: finish.SessionID}) {
					return nil
				}
			}
			return ErrConflict
		case task.Running:
		default:
			return ErrConflict
		}
		if finish.Error != "" {
			slot.State = task.Failed
			slot.Error = finish.Error
			return nil
		}
		if slot.Parent != nil && slot.Parent.SessionID != finish.SessionID {
			return ErrConflict
		}
		refs, err := c.taskAssets(ctx, s, slot.Spec.Output, finish.SessionID, finish.Artifacts)
		if err != nil {
			return err
		}
		if slot.ExecutionPolicy == task.ExecutionUntrustedIsolated {
			slot.acceptedSandbox, err = c.recheckSandboxSuccess(ctx, s, *slot, evidence)
			if err != nil {
				return err
			}
		}
		slot.Assets = refs
		slot.linkedSessionID = finish.SessionID
		slot.State = task.Succeeded
		return nil
	})
}

func (c *Catalog) taskAssets(ctx context.Context, s taskAddress, output, session string, artifacts []task.Ref) ([]task.Ref, error) {
	primary := task.TurnKey(s.turn, asset.Key{Kind: "artifact", ID: "out/" + output})
	keys := []asset.Key{primary}
	declared := make(map[asset.Key]task.Ref, len(artifacts))
	if len(artifacts) > 0 {
		prefix := task.TurnKey(s.turn, asset.Key{Kind: "artifact", ID: "out"}).ID + "/"
		for _, ref := range artifacts {
			if !ref.Key.Valid() || ref.Kind != "artifact" || !strings.HasPrefix(ref.ID, prefix) || ref.Version <= 0 || ref.ContentPath != ref.Key.ContentPath(ref.Version) {
				return nil, errBadRequest
			}
			if _, exists := declared[ref.Key]; exists {
				return nil, errBadRequest
			}
			declared[ref.Key] = ref
			if ref.Key != primary {
				keys = append(keys, ref.Key)
			}
		}
		if _, exists := declared[primary]; !exists {
			return nil, ErrNotReady
		}
		sort.Slice(keys[1:], func(i, j int) bool { return keys[i+1].ID < keys[j+1].ID })
	}
	keys = append(keys, task.TurnKey(s.turn, asset.Key{Kind: "workspace", ID: "root"}), task.TurnKey(s.turn, asset.Key{Kind: "session", ID: session}))
	var refs []task.Ref
	for _, k := range keys {
		if !k.Valid() {
			return nil, errBadRequest
		}
		var m asset.Metadata
		var v int64
		var err error
		if requested, pinned := declared[k]; pinned {
			var raw string
			v = requested.Version
			err = c.writer.QueryRowContext(ctx, "SELECT metadata FROM revisions WHERE scope=$1 AND kind=$2 AND asset_id=$3 AND version=$4", s.Prefix(), k.Kind, k.ID, v).Scan(&raw)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrNotReady
			}
			if err == nil {
				err = json.Unmarshal([]byte(raw), &m)
			}
			if err == nil && m.SHA256 != requested.SHA256 {
				return nil, ErrConflict
			}
		} else {
			m, v, err = head(ctx, c.writer, s.Prefix(), k)
		}
		if err != nil {
			return nil, err
		}
		if v == 0 || (k.Kind == "artifact" && m.SessionID != session) {
			return nil, ErrNotReady
		}
		refs = append(refs, task.Ref{Key: k, Version: v, SHA256: m.SHA256, ContentPath: k.ContentPath(v)})
	}
	return refs, nil
}
