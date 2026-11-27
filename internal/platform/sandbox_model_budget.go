package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const (
	SandboxModelMaxRequests      int64 = 128
	SandboxModelMaxInputBytes    int64 = 16 << 20
	SandboxModelMaxOutputBytes   int64 = 64 << 20
	SandboxModelMaxRequestInput  int64 = 256 << 10
	SandboxModelMaxRequestOutput int64 = 4 << 20
)

var ErrSandboxModelBudget = errors.New("sandbox model allowance exhausted")

// SandboxModelPolicy comes from the trusted model gateway. Fingerprint binds
// the fixed upstream origin, model and limits, never the upstream secret value.
// The first reservation freezes every field for the lifetime of this run.
type SandboxModelPolicy struct {
	Fingerprint           string `json:"fingerprint"`
	MaxRequests           int64  `json:"max_requests"`
	MaxInputBytes         int64  `json:"max_input_bytes"`
	MaxOutputBytes        int64  `json:"max_output_bytes"`
	PerRequestOutputBytes int64  `json:"per_request_output_bytes"`
}

// SandboxModelReservation is returned only after its charge commits. Requests,
// InputBytes and OutputBytes are cumulative reservations, not actual billing.
// A gateway must bound this response to Policy.PerRequestOutputBytes. There is
// no refund, including when forwarding, streaming or acknowledgement fails.
type SandboxModelReservation struct {
	Run         SandboxRun         `json:"run"`
	Policy      SandboxModelPolicy `json:"policy"`
	Requests    int64              `json:"requests"`
	InputBytes  int64              `json:"input_bytes"`
	OutputBytes int64              `json:"output_bytes"`
}

const sandboxModelSchema = `
CREATE TABLE IF NOT EXISTS sandbox_model_budgets (
 scope TEXT NOT NULL, run_id TEXT NOT NULL, policy TEXT NOT NULL,
 requests BIGINT NOT NULL, input_bytes BIGINT NOT NULL, output_bytes BIGINT NOT NULL,
 updated BIGINT NOT NULL, PRIMARY KEY(scope,run_id));
`

func (p SandboxModelPolicy) valid() bool {
	return sandboxDigest(p.Fingerprint) && p.MaxRequests > 0 && p.MaxRequests <= SandboxModelMaxRequests &&
		p.MaxInputBytes > 0 && p.MaxInputBytes <= SandboxModelMaxInputBytes &&
		p.MaxOutputBytes > 0 && p.MaxOutputBytes <= SandboxModelMaxOutputBytes &&
		p.PerRequestOutputBytes > 0 && p.PerRequestOutputBytes <= SandboxModelMaxRequestOutput &&
		p.PerRequestOutputBytes <= p.MaxOutputBytes
}

func (r SandboxModelReservation) valid() bool {
	return r.Policy.valid() && r.Requests > 0 && r.Requests <= r.Policy.MaxRequests &&
		r.InputBytes >= r.Requests && r.InputBytes <= r.Policy.MaxInputBytes &&
		r.InputBytes <= r.Requests*SandboxModelMaxRequestInput &&
		r.OutputBytes > 0 && r.OutputBytes <= r.Policy.MaxOutputBytes &&
		r.OutputBytes == r.Requests*r.Policy.PerRequestOutputBytes
}

// ReserveSandboxModel must succeed before each upstream request. Retrying the
// same request reserves again; a lost response may waste allowance but cannot
// restore it. The run's permanent identity also survives collection/restarts.
// It grants no model authority after a claim stops or output becomes sealed.
func (c *Catalog) ReserveSandboxModel(ctx context.Context, token string, policy SandboxModelPolicy, inputBytes int64) (SandboxModelReservation, error) {
	if !policy.valid() || inputBytes <= 0 || inputBytes > SandboxModelMaxRequestInput {
		return SandboxModelReservation{}, errBadRequest
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	run, err := c.AuthenticateSandboxCapability(ctx, token, SandboxModel)
	if err != nil {
		return SandboxModelReservation{}, err
	}
	ctx = auditCapabilityContext(ctx, SandboxModel)
	reservation, err := c.readSandboxModelBudget(ctx, run)
	fresh := errors.Is(err, ErrMissing)
	if err != nil && !fresh {
		return SandboxModelReservation{}, err
	}
	if fresh {
		reservation = SandboxModelReservation{Run: run, Policy: policy}
	} else if reservation.Policy != policy {
		return SandboxModelReservation{}, ErrConflict
	}
	// All existing values were validated before subtraction. Add only after
	// checking the remaining allowance, so even hostile int64 inputs cannot
	// overflow the persisted counters.
	if reservation.Requests >= policy.MaxRequests || inputBytes > policy.MaxInputBytes-reservation.InputBytes ||
		policy.PerRequestOutputBytes > policy.MaxOutputBytes-reservation.OutputBytes {
		return SandboxModelReservation{}, ErrSandboxModelBudget
	}
	reservation.Requests++
	reservation.InputBytes += inputBytes
	reservation.OutputBytes += policy.PerRequestOutputBytes
	raw, err := json.Marshal(policy)
	if err != nil {
		return SandboxModelReservation{}, err
	}
	tx, err := c.writer.BeginTx(ctx, nil)
	if err != nil {
		return SandboxModelReservation{}, err
	}
	defer tx.Rollback()
	if fresh {
		_, err = tx.ExecContext(ctx, `INSERT INTO sandbox_model_budgets(scope,run_id,policy,requests,input_bytes,output_bytes,updated)
 VALUES($1,$2,$3,$4,$5,$6,$7)`, run.Scope.Prefix(), run.ID, string(raw), reservation.Requests,
			reservation.InputBytes, reservation.OutputBytes, time.Now().UTC().UnixNano())
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE sandbox_model_budgets SET requests=$1,input_bytes=$2,output_bytes=$3,updated=$4
 WHERE scope=$5 AND run_id=$6`, reservation.Requests, reservation.InputBytes, reservation.OutputBytes,
			time.Now().UTC().UnixNano(), run.Scope.Prefix(), run.ID)
		if err == nil {
			var changed int64
			changed, err = result.RowsAffected()
			if err == nil && changed != 1 {
				err = ErrConflict
			}
		}
	}
	if err != nil {
		return SandboxModelReservation{}, err
	}
	e := AuditEvent{Scope: run.Scope, Actor: auditActor(ctx, "model-gateway"), Action: "model.reserved", TaskID: run.Scope.Runtime,
		TurnID: run.TurnID, AttemptSHA256: auditHash("attempt", run.Attempt), RunID: run.ID, PodUID: run.PodUID,
		Digest: policy.Fingerprint, ModelRequests: reservation.Requests, ModelInputBytes: reservation.InputBytes, ModelOutputBytes: reservation.OutputBytes}
	if err = appendAudit(ctx, tx, e); err != nil {
		return SandboxModelReservation{}, err
	}
	// A SQL operation may wait until the original authority has expired. Read
	// through the pool, never the leased transaction, before authorizing any
	// forward. A lost lease/ambiguous commit returns no successful reservation.
	if _, err = c.AuthenticateSandboxCapability(ctx, token, SandboxModel); err != nil {
		return SandboxModelReservation{}, err
	}
	if err = tx.Commit(); err != nil {
		return SandboxModelReservation{}, err
	}
	return reservation, nil
}

func (c *Catalog) readSandboxModelBudget(ctx context.Context, run SandboxRun) (SandboxModelReservation, error) {
	r := SandboxModelReservation{Run: run}
	var raw string
	err := c.db.QueryRowContext(ctx, `SELECT policy,requests,input_bytes,output_bytes FROM sandbox_model_budgets
 WHERE scope=$1 AND run_id=$2`, run.Scope.Prefix(), run.ID).Scan(&raw, &r.Requests, &r.InputBytes, &r.OutputBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrMissing
	}
	if err != nil {
		return SandboxModelReservation{}, err
	}
	if err = json.Unmarshal([]byte(raw), &r.Policy); err != nil || !r.valid() {
		return SandboxModelReservation{}, ErrConflict
	}
	return r, nil
}
