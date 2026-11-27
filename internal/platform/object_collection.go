package platform

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"agentmoor/internal/asset"
	"github.com/google/uuid"
)

const (
	objectAssetBlob     = "asset_blob"
	objectWorkspacePart = "workspace_part"
	objectSandboxInput  = "sandbox_input"
	objectSandboxOutput = "sandbox_output"
)

const objectCollectionSchema = `
CREATE TABLE IF NOT EXISTS object_collection_intents (
 operation_id TEXT PRIMARY KEY, scope TEXT NOT NULL, object_kind TEXT NOT NULL,
 digest TEXT NOT NULL, state TEXT NOT NULL, updated BIGINT NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS object_collection_pending
 ON object_collection_intents(scope,object_kind,digest) WHERE state='pending';
`

func objectCollectionKey(s asset.Scope, kind, digest string) (string, error) {
	if !s.Valid() || !sandboxDigest(digest) {
		return "", ErrAudit
	}
	switch kind {
	case objectAssetBlob:
		return objectKey(s, digest), nil
	case objectWorkspacePart:
		return partKey(s, digest), nil
	case objectSandboxInput:
		return sandboxObjectKey(s, SandboxInput, digest), nil
	case objectSandboxOutput:
		return sandboxObjectKey(s, SandboxOutput, digest), nil
	}
	return "", ErrAudit
}

type objectCollectionRunKey struct{}
type objectCollectionRun struct{ scope, digest, id, attempt string }

// Only the authenticated output writer supplies this attribution. Neither the
// capability value nor its hash is copied into an event or deletion intent.
func objectCollectionRunContext(ctx context.Context, r SandboxRun) context.Context {
	return context.WithValue(ctx, objectCollectionRunKey{}, objectCollectionRun{
		r.Scope.Prefix(), r.Output.SHA256, r.ID, auditHash("attempt", r.Attempt)})
}

func objectCollectionEvent(ctx context.Context, s asset.Scope, kind, digest, id, action, proof string) (AuditEvent, error) {
	key, err := objectCollectionKey(s, kind, digest)
	if err != nil {
		return AuditEvent{}, err
	}
	e := AuditEvent{Scope: s, Actor: auditActor(ctx, "internal"), TaskID: s.Runtime,
		Action: action, OperationID: id, ObjectKind: kind,
		ObjectIDSHA256: auditHash("object-id/"+kind, key), CollectionProof: proof}
	if e.Actor.Source == "guest-capability" {
		r, ok := ctx.Value(objectCollectionRunKey{}).(objectCollectionRun)
		if !ok || r.scope != s.Prefix() || r.digest != digest || kind != objectSandboxOutput {
			return AuditEvent{}, ErrAudit
		}
		e.RunID, e.AttemptSHA256 = r.id, r.attempt
	}
	return e, nil
}

func (c *Catalog) pendingObjectCollection(ctx context.Context, s asset.Scope, kind, digest string) (string, error) {
	var id string
	err := c.writer.QueryRowContext(ctx, `SELECT operation_id FROM object_collection_intents
 WHERE scope=$1 AND object_kind=$2 AND digest=$3 AND state='pending'`, s.Prefix(), kind, digest).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err == nil && !sandboxUUID(id) {
		return "", ErrAudit
	}
	return id, err
}

// finishObjectCollection changes SQL and the corresponding audit event in one
// leased transaction. It cannot undo an object-service request already sent.
func (c *Catalog) finishObjectCollection(ctx context.Context, s asset.Scope, kind, digest, id, state, proof string) error {
	event, err := objectCollectionEvent(ctx, s, kind, digest, id, "object.collection_"+state, proof)
	if err != nil {
		return err
	}
	tx, err := c.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE object_collection_intents SET state=$1,updated=$2
 WHERE operation_id=$3 AND scope=$4 AND object_kind=$5 AND digest=$6 AND state='pending'`,
		state, time.Now().UnixNano(), id, s.Prefix(), kind, digest)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.Join(ErrConflict, err)
	}
	if err = appendAudit(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

// prepareObjectPut runs under c.mu immediately before every possible Put. A
// prior pending delete has an unknown outcome; it must not later be confirmed
// as the deletion of bytes recreated by this writer. Even a failed Put retires
// that old intent conservatively. It never emits a deletion confirmation.
func (c *Catalog) prepareObjectPut(ctx context.Context, s asset.Scope, kind, digest string) error {
	if _, err := objectCollectionKey(s, kind, digest); err != nil {
		return err
	}
	id, err := c.pendingObjectCollection(ctx, s, kind, digest)
	if err != nil || id == "" {
		return err
	}
	return c.finishObjectCollection(ctx, s, kind, digest, id, "superseded", "")
}

// collectObject runs only for candidates selected by the existing collectors,
// under c.mu and the store's exclusive lease. Pending intents never bypass the
// current reference/retention selector. No SQL transaction spans external I/O.
func (c *Catalog) collectObject(ctx context.Context, s asset.Scope, kind, digest string) (int, error) {
	key, err := objectCollectionKey(s, kind, digest)
	if err != nil {
		return 0, err
	}
	id, err := c.pendingObjectCollection(ctx, s, kind, digest)
	if err != nil {
		return 0, err
	}
	exists, err := c.objects.Exists(ctx, key)
	if err != nil {
		return 0, err
	}
	if id == "" {
		if !exists {
			return 0, nil
		}
		id = uuid.NewString()
		event, err := objectCollectionEvent(ctx, s, kind, digest, id, "object.collection_planned", "")
		if err != nil {
			return 0, err
		}
		tx, err := c.writer.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `INSERT INTO object_collection_intents(operation_id,scope,object_kind,digest,state,updated)
 VALUES($1,$2,$3,$4,'pending',$5)`, id, s.Prefix(), kind, digest, time.Now().UnixNano()); err != nil {
			return 0, err
		}
		if err = appendAudit(ctx, tx, event); err != nil {
			return 0, err
		}
		if err = tx.Commit(); err != nil {
			return 0, err
		}
	}
	proof, count := "absence", 0
	if exists {
		// Exists may have blocked after loading an old pending intent. Check
		// the exact leased writer again before beginning Remove; do not use a
		// pooled fallback. This cannot retract I/O sent after the check.
		var alive int
		if err = c.writer.QueryRowContext(ctx, "SELECT 1").Scan(&alive); err != nil {
			return 0, err
		}
		if err = c.objects.Remove(ctx, key); err != nil {
			return 0, err
		}
		proof, count = "remove_ack", 1
	}
	if err = c.finishObjectCollection(ctx, s, kind, digest, id, "confirmed", proof); err != nil {
		return 0, err // the physical result may already exist; no success claim
	}
	return count, nil
}
