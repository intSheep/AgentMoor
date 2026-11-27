package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agentmoor/internal/asset"
)

type sandboxCap struct {
	hash    string
	purpose SandboxPurpose
	expires time.Time
}

type sandboxScanner interface{ Scan(...any) error }

const sandboxColumns = "scope,turn_id,attempt,id,record,updated,collected"

func scanSandbox(row sandboxScanner) (SandboxRun, error) {
	var r SandboxRun
	var scope, turn, attempt, id, raw string
	var updated int64
	var collected bool
	if err := row.Scan(&scope, &turn, &attempt, &id, &raw, &updated, &collected); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrMissing
		}
		return r, err
	}
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return r, err
	}
	r.Attempt = attempt
	if !r.Address().valid() || r.Scope.Prefix() != scope || r.TurnID != turn || r.ID != id ||
		r.ID != sandboxIdentity(r.Address()) || r.UpdatedAt.UnixNano() != updated || r.Collected != collected ||
		!r.Input.valid() || !sandboxDigest(r.ProfileFingerprint) {
		return SandboxRun{}, ErrConflict
	}
	fingerprint, err := sandboxRecordFingerprint(r.Address(), r.Source)
	if err != nil {
		return SandboxRun{}, err
	}
	if fingerprint != r.RecordFingerprint || !r.Deadline.Equal(r.Source.Deadline) {
		return SandboxRun{}, ErrConflict
	}
	return r, nil
}

func (c *Catalog) loadSandbox(ctx context.Context, a SandboxAddress) (SandboxRun, error) {
	if !a.valid() {
		return SandboxRun{}, errBadRequest
	}
	return scanSandbox(c.db.QueryRowContext(ctx, "SELECT "+sandboxColumns+" FROM sandbox_runs WHERE scope=$1 AND turn_id=$2 AND attempt=$3", a.Scope.Prefix(), a.TurnID, a.Attempt))
}

func (c *Catalog) loadSandboxID(ctx context.Context, scope, id string) (SandboxRun, error) {
	return scanSandbox(c.db.QueryRowContext(ctx, "SELECT "+sandboxColumns+" FROM sandbox_runs WHERE scope=$1 AND id=$2", scope, id))
}

// ListSandboxRuns is an operator/controller read, not a guest capability route.
func (c *Catalog) ListSandboxRuns(ctx context.Context, scope asset.Scope) ([]SandboxRun, error) {
	if !scope.Valid() {
		return nil, errBadRequest
	}
	rows, err := c.db.QueryContext(ctx, "SELECT "+sandboxColumns+" FROM sandbox_runs WHERE scope=$1 ORDER BY updated,id", scope.Prefix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SandboxRun
	for rows.Next() {
		r, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// saveSandbox is called under c.mu; all reads use the pool and the transaction
// only mutates on the connection owning the service lease.
func (c *Catalog) saveSandbox(ctx context.Context, r *SandboxRun, revoke string, caps []sandboxCap) error {
	before, err := c.loadSandbox(ctx, r.Address())
	if err != nil {
		return err
	}
	r.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := c.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	collected := 0
	if r.Collected {
		collected = 1
	}
	result, err := tx.ExecContext(ctx, `UPDATE sandbox_runs SET record=$1,updated=$2,collected=$3
 WHERE scope=$4 AND turn_id=$5 AND attempt=$6 AND id=$7`, string(raw), r.UpdatedAt.UnixNano(), collected,
		r.Scope.Prefix(), r.TurnID, r.Attempt, r.ID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrMissing
	}
	if revoke != "" {
		query := "UPDATE sandbox_caps SET revoked=1 WHERE scope=$1 AND run_id=$2"
		arguments := []any{r.Scope.Prefix(), r.ID}
		if revoke == "input-model" {
			query += " AND purpose<>$3"
			arguments = append(arguments, string(SandboxOutput))
		}
		if _, err = tx.ExecContext(ctx, query, arguments...); err != nil {
			return err
		}
	}
	for _, cap := range caps {
		if _, err = tx.ExecContext(ctx, `INSERT INTO sandbox_caps(token_hash,scope,run_id,purpose,expires,revoked)
 VALUES($1,$2,$3,$4,$5,0)`, cap.hash, r.Scope.Prefix(), r.ID, string(cap.purpose), cap.expires.UnixNano()); err != nil {
			return err
		}
	}
	if event, changed := auditSandboxEvent(ctx, &before, *r); changed {
		if err = appendAudit(ctx, tx, event); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Check the writer before an external Put. A Put may still finish after the
// lease is lost; the final seal must then fail and the registered candidate
// remains available to the next owner for safe recovery/collection.
func (c *Catalog) sandboxWriteGuard(ctx context.Context, r SandboxRun) error {
	result, err := c.writer.ExecContext(ctx, "UPDATE sandbox_runs SET updated=updated WHERE scope=$1 AND id=$2", r.Scope.Prefix(), r.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrMissing
	}
	return nil
}

func sandboxObjectKey(scope asset.Scope, purpose SandboxPurpose, digest string) string {
	return scope.Prefix() + "/sandbox/" + string(purpose) + "/" + digest
}

// collectSandbox runs under the same c.mu as Put/seal. The caller's cutoff is
// the existing retention policy (public GC supplies now-24h). Active claims
// remain references even when a test/operator supplies a future cutoff.
func (c *Catalog) collectSandbox(ctx context.Context, scope asset.Scope, before time.Time) (int, error) {
	runs, err := c.ListSandboxRuns(ctx, scope)
	if err != nil {
		return 0, err
	}
	protected := make(map[string]bool)
	active := make(map[string]bool)
	for _, r := range runs {
		if r.Collected {
			continue
		}
		current := c.sandboxCurrent(ctx, r, false)
		if current != nil && !errors.Is(current, ErrConflict) && !errors.Is(current, ErrMissing) {
			return 0, current
		}
		active[r.ID] = current == nil
		keep := !r.UpdatedAt.Before(before) || active[r.ID]
		if !keep {
			continue
		}
		for purpose, blob := range map[SandboxPurpose]SandboxBlob{SandboxInput: r.Input, SandboxOutput: r.Output} {
			if blob.SHA256 != "" {
				protected[sandboxObjectKey(scope, purpose, blob.SHA256)] = true
			}
		}
	}
	n := 0
	for _, r := range runs {
		if r.Collected || !r.UpdatedAt.Before(before) || active[r.ID] {
			continue
		}
		if err = c.sandboxWriteGuard(ctx, r); err != nil {
			return n, err
		}
		for purpose, blob := range map[SandboxPurpose]SandboxBlob{SandboxInput: r.Input, SandboxOutput: r.Output} {
			if blob.SHA256 == "" {
				continue
			}
			key := sandboxObjectKey(scope, purpose, blob.SHA256)
			if protected[key] {
				continue
			}
			exists, err := c.objects.Exists(ctx, key)
			if err != nil {
				return n, err
			}
			if exists {
				if err = c.objects.Remove(ctx, key); err != nil {
					return n, err
				}
				n++
			}
		}
		// Keep identity, frozen fingerprints, Pod ownership and create_started.
		// A collected attempt can never become a fresh execution again.
		r.Collected = true
		if err = c.saveSandbox(ctx, &r, "all", nil); err != nil {
			return n, err
		}
	}
	return n, nil
}
