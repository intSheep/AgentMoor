package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

// SandboxMaxBytes bounds each private input/output envelope independently.
const SandboxMaxBytes int64 = 64 << 20

const (
	SandboxPreparing = "preparing"
	SandboxPrepared  = "prepared"
	SandboxCreating  = "create-started"
	SandboxRunning   = "running"
	SandboxReady     = "ready-to-publish"
	SandboxFailed    = "failed"
	SandboxCancelled = "cancelled"
	SandboxExpired   = "expired"
)

type SandboxPurpose string

const (
	SandboxInput  SandboxPurpose = "input"
	SandboxOutput SandboxPurpose = "output"
	SandboxModel  SandboxPurpose = "model"
)

var ErrSandboxDenied = errors.New("sandbox capability denied")

// SandboxAddress is derived from an authenticated allocation, never a guest.
// Attempt is the already-persisted private task claim, not a new execution ID.
type SandboxAddress struct {
	Scope   asset.Scope
	TurnID  string
	Attempt string
}

type SandboxBlob struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Sealed bool   `json:"sealed"`
}

// SandboxRun is durable metadata, not an HTTP authorization or Pod template.
// Collection retains this identity and its create fence as a tombstone.
type SandboxRun struct {
	ID                 string      `json:"run_id"`
	Scope              asset.Scope `json:"scope"`
	TurnID             string      `json:"turn_id,omitempty"`
	Attempt            string      `json:"-"`
	Source             task.Record `json:"source"`
	RecordFingerprint  string      `json:"record_fingerprint"`
	ProfileFingerprint string      `json:"profile_fingerprint"`
	Input              SandboxBlob `json:"input"`
	Output             SandboxBlob `json:"output"`
	State              string      `json:"state"`
	CreateStarted      bool        `json:"create_started"`
	PodName            string      `json:"pod_name,omitempty"`
	PodUID             string      `json:"pod_uid,omitempty"`
	PodTerminal        bool        `json:"pod_terminal"`
	ExitCode           int         `json:"exit_code"`
	PodDeleted         bool        `json:"pod_deleted"`
	Collected          bool        `json:"collected"`
	Deadline           time.Time   `json:"deadline"`
	UpdatedAt          time.Time   `json:"updated_at"`
	ErrorCode          string      `json:"error_code,omitempty"`
}

func (r SandboxRun) Address() SandboxAddress { return SandboxAddress{r.Scope, r.TurnID, r.Attempt} }

// Raw capabilities are returned only by the first committed create fence.
// They are deliberately absent from JSON and are never stored in the catalog.
type SandboxCapabilities struct {
	Input  string `json:"-"`
	Output string `json:"-"`
	Model  string `json:"-"`
}

type SandboxCreatePermit struct {
	Run           SandboxRun
	CreateAllowed bool                `json:"-"`
	Capabilities  SandboxCapabilities `json:"-"`
}

const sandboxSchema = `
CREATE TABLE IF NOT EXISTS sandbox_runs (
 scope TEXT NOT NULL, turn_id TEXT NOT NULL, attempt TEXT NOT NULL, id TEXT NOT NULL,
 record TEXT NOT NULL, updated BIGINT NOT NULL, collected BIGINT NOT NULL,
 PRIMARY KEY(scope,turn_id,attempt), UNIQUE(scope,id));
CREATE INDEX IF NOT EXISTS sandbox_collection ON sandbox_runs(scope,collected,updated);
CREATE TABLE IF NOT EXISTS sandbox_caps (
 token_hash TEXT PRIMARY KEY, scope TEXT NOT NULL, run_id TEXT NOT NULL,
 purpose TEXT NOT NULL, expires BIGINT NOT NULL, revoked BIGINT NOT NULL,
 UNIQUE(scope,run_id,purpose));
`

func sandboxDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}

func sandboxUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

func (a SandboxAddress) valid() bool {
	return a.Scope.Valid() && sandboxUUID(a.Attempt) && (a.TurnID == "" || sandboxUUID(a.TurnID))
}

func (b SandboxBlob) valid() bool {
	return sandboxDigest(b.SHA256) && b.Size >= 0 && b.Size <= SandboxMaxBytes
}

func sandboxIdentity(a SandboxAddress) string {
	b, _ := json.Marshal(struct{ Scope, Turn, Attempt string }{a.Scope.Prefix(), a.TurnID, a.Attempt})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func sandboxRecordFingerprint(a SandboxAddress, source task.Record) (string, error) {
	b, err := json.Marshal(struct {
		Address  SandboxAddress
		Spec     task.Spec
		Parent   *task.Checkpoint
		Skills   []task.SkillPin
		Deadline time.Time
		// Omit the new field for legacy rows, preserving their exact original
		// fingerprint and permanent create fence across upgrades.
		Policy string `json:",omitempty"`
	}{a, source.Spec, source.Parent, source.Skills, source.Deadline, source.ExecutionPolicy})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func (c *Catalog) sandboxTask(ctx context.Context, a SandboxAddress, succeeded bool) (taskSlot, error) {
	if !a.valid() {
		return taskSlot{}, errBadRequest
	}
	slot, err := c.readTask(ctx, taskAddress{a.Scope, a.TurnID})
	if err != nil {
		return taskSlot{}, err
	}
	if slot.attempt != a.Attempt || slot.ID != a.Scope.Runtime || slot.TurnID != a.TurnID ||
		(slot.State != task.Running && !(succeeded && slot.State == task.Succeeded)) {
		return taskSlot{}, ErrConflict
	}
	return slot, nil
}

func (c *Catalog) sandboxCurrent(ctx context.Context, r SandboxRun, succeeded bool) error {
	slot, err := c.sandboxSnapshot(ctx, r)
	if err != nil {
		return err
	}
	if slot.State != task.Running && !(succeeded && slot.State == task.Succeeded) {
		return ErrConflict
	}
	return nil
}

func (c *Catalog) sandboxSnapshot(ctx context.Context, r SandboxRun) (taskSlot, error) {
	slot, err := c.readTask(ctx, taskAddress{r.Scope, r.TurnID})
	if err != nil {
		return taskSlot{}, err
	}
	if slot.attempt != r.Attempt || slot.ID != r.Scope.Runtime || slot.TurnID != r.TurnID {
		return taskSlot{}, ErrConflict
	}
	fingerprint, err := sandboxRecordFingerprint(r.Address(), slot.Record)
	if err != nil {
		return taskSlot{}, err
	}
	if fingerprint != r.RecordFingerprint || !r.Deadline.Equal(slot.Deadline) {
		return taskSlot{}, ErrConflict
	}
	return slot, nil
}

func sandboxStopped(r SandboxRun) bool {
	return r.Collected || r.State == SandboxFailed || r.State == SandboxCancelled || r.State == SandboxExpired || r.State == SandboxReady
}

func (c *Catalog) sandboxActive(ctx context.Context, r SandboxRun) error {
	if sandboxStopped(r) || !time.Now().Before(r.Deadline) {
		return ErrConflict
	}
	return c.sandboxCurrent(ctx, r, false)
}

// BeginSandbox freezes the actual catalog claim, input identity and operator
// profile. It does not run or schedule anything and cannot revive an old run.
func (c *Catalog) BeginSandbox(ctx context.Context, a SandboxAddress, profile string, input SandboxBlob) (SandboxRun, error) {
	if !a.valid() || !sandboxDigest(profile) || !input.valid() || input.Sealed {
		return SandboxRun{}, errBadRequest
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	slot, err := c.sandboxTask(ctx, a, false)
	if err != nil {
		return SandboxRun{}, err
	}
	fingerprint, err := sandboxRecordFingerprint(a, slot.Record)
	if err != nil {
		return SandboxRun{}, err
	}
	r, err := c.loadSandbox(ctx, a)
	if err == nil {
		if r.RecordFingerprint != fingerprint || r.ProfileFingerprint != profile ||
			r.Input.SHA256 != input.SHA256 || r.Input.Size != input.Size || sandboxStopped(r) {
			return SandboxRun{}, ErrConflict
		}
		return r, nil
	}
	if !errors.Is(err, ErrMissing) {
		return SandboxRun{}, err
	}
	r = SandboxRun{ID: sandboxIdentity(a), Scope: a.Scope, TurnID: a.TurnID, Attempt: a.Attempt,
		Source: slot.Record, RecordFingerprint: fingerprint, ProfileFingerprint: profile,
		Input: input, State: SandboxPreparing, Deadline: slot.Deadline, UpdatedAt: time.Now().UTC()}
	raw, err := json.Marshal(r)
	if err != nil {
		return SandboxRun{}, err
	}
	_, err = c.writer.ExecContext(ctx, `INSERT INTO sandbox_runs(scope,turn_id,attempt,id,record,updated,collected)
 VALUES($1,$2,$3,$4,$5,$6,0)`, a.Scope.Prefix(), a.TurnID, a.Attempt, r.ID, string(raw), r.UpdatedAt.UnixNano())
	return r, err
}

func (c *Catalog) SandboxRun(ctx context.Context, a SandboxAddress) (SandboxRun, error) {
	if !a.valid() {
		return SandboxRun{}, errBadRequest
	}
	r, err := c.loadSandbox(ctx, a)
	if err != nil {
		return SandboxRun{}, err
	}
	slot, err := c.sandboxSnapshot(ctx, r)
	if err != nil {
		return SandboxRun{}, err
	}
	// Status reads do not write through the leased connection. Authentication
	// and mutations independently recheck the real task state and deadline.
	if !sandboxStopped(r) {
		if slot.State == task.Cancelling || slot.State == task.Cancelled {
			r.State = SandboxCancelled
		} else if !time.Now().Before(r.Deadline) {
			r.State = SandboxExpired
		} else if slot.State != task.Running {
			r.State = SandboxFailed
		}
	}
	return r, nil
}

// FenceSandboxCreate commits the one create attempt before a controller may
// contact Kubernetes. An uncertain caller must inspect/clean up, never recreate.
func (c *Catalog) FenceSandboxCreate(ctx context.Context, a SandboxAddress, expires time.Time) (SandboxCreatePermit, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.loadSandbox(ctx, a)
	if err != nil {
		return SandboxCreatePermit{}, err
	}
	if err = c.sandboxActive(ctx, r); err != nil || !r.Input.Sealed {
		if err == nil {
			err = ErrNotReady
		}
		return SandboxCreatePermit{}, err
	}
	if r.CreateStarted {
		return SandboxCreatePermit{Run: r}, nil
	}
	if !time.Now().Before(expires) || expires.After(r.Deadline) {
		return SandboxCreatePermit{}, errBadRequest
	}
	var raw SandboxCapabilities
	var caps []sandboxCap
	for _, purpose := range []SandboxPurpose{SandboxInput, SandboxOutput, SandboxModel} {
		nonce := make([]byte, 32)
		if _, err = rand.Read(nonce); err != nil {
			return SandboxCreatePermit{}, err
		}
		token := "sbx1." + base64.RawURLEncoding.EncodeToString(nonce)
		switch purpose {
		case SandboxInput:
			raw.Input = token
		case SandboxOutput:
			raw.Output = token
		case SandboxModel:
			raw.Model = token
		}
		caps = append(caps, sandboxCap{hash: sandboxTokenHash(token), purpose: purpose, expires: expires})
	}
	r.CreateStarted, r.State, r.PodName = true, SandboxCreating, "sandbox-"+r.ID[:32]
	if err = c.saveSandbox(ctx, &r, "", caps); err != nil {
		return SandboxCreatePermit{}, err
	}
	return SandboxCreatePermit{Run: r, CreateAllowed: true, Capabilities: raw}, nil
}

func (c *Catalog) BindSandboxPod(ctx context.Context, a SandboxAddress, uid string) (SandboxRun, error) {
	if !sandboxUUID(uid) {
		return SandboxRun{}, errBadRequest
	}
	return c.changeSandbox(ctx, a, func(r *SandboxRun) (string, error) {
		if !r.CreateStarted || r.Collected || (r.PodUID != "" && r.PodUID != uid) {
			return "", ErrConflict
		}
		r.PodUID = uid
		if !sandboxStopped(*r) {
			r.State = SandboxRunning
		}
		return "", nil
	})
}

func (c *Catalog) RecordSandboxTerminal(ctx context.Context, a SandboxAddress, uid string, exit int) (SandboxRun, error) {
	if exit < 0 || exit > 255 {
		return SandboxRun{}, errBadRequest
	}
	return c.changeSandbox(ctx, a, func(r *SandboxRun) (string, error) {
		if r.PodUID == "" || r.PodUID != uid || r.Collected || (r.PodTerminal && r.ExitCode != exit) {
			return "", ErrConflict
		}
		r.PodTerminal, r.ExitCode = true, exit
		if !sandboxStopped(*r) && (exit != 0 || !r.Output.Sealed) {
			r.State, r.ErrorCode = SandboxFailed, "guest-exit-or-missing-result"
		}
		return "all", nil
	})
}

func (c *Catalog) ConfirmSandboxDeleted(ctx context.Context, a SandboxAddress, uid string) (SandboxRun, error) {
	return c.changeSandbox(ctx, a, func(r *SandboxRun) (string, error) {
		if r.PodUID == "" || r.PodUID != uid || r.Collected {
			return "", ErrConflict
		}
		r.PodDeleted = true
		if !sandboxStopped(*r) {
			if r.PodTerminal && r.ExitCode == 0 && r.Output.Sealed {
				r.State = SandboxReady
			} else {
				r.State, r.ErrorCode = SandboxFailed, "pod-lost"
			}
		}
		return "all", nil
	})
}

func (c *Catalog) CancelSandbox(ctx context.Context, a SandboxAddress) (SandboxRun, error) {
	return c.changeSandbox(ctx, a, func(r *SandboxRun) (string, error) {
		if r.Collected {
			return "", ErrConflict
		}
		r.State, r.ErrorCode = SandboxCancelled, "cancelled"
		return "all", nil
	})
}

func (c *Catalog) FailSandbox(ctx context.Context, a SandboxAddress, code string) (SandboxRun, error) {
	if len(code) == 0 || len(code) > 64 || strings.Trim(code, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return SandboxRun{}, errBadRequest
	}
	return c.changeSandbox(ctx, a, func(r *SandboxRun) (string, error) {
		if r.Collected {
			return "", ErrConflict
		}
		r.State, r.ErrorCode = SandboxFailed, code
		return "all", nil
	})
}

func (c *Catalog) changeSandbox(ctx context.Context, a SandboxAddress, change func(*SandboxRun) (string, error)) (SandboxRun, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.loadSandbox(ctx, a)
	if err != nil {
		return SandboxRun{}, err
	}
	revoke, err := change(&r)
	if err != nil {
		return SandboxRun{}, err
	}
	err = c.saveSandbox(ctx, &r, revoke, nil)
	return r, err
}

func sandboxTokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// AuthenticateSandboxCapability checks current authority, not a model budget.
// A model proxy must separately reserve a persistent, bounded allowance.
func (c *Catalog) AuthenticateSandboxCapability(ctx context.Context, token string, purpose SandboxPurpose) (SandboxRun, error) {
	if len(token) != 48 || !strings.HasPrefix(token, "sbx1.") || (purpose != SandboxInput && purpose != SandboxOutput && purpose != SandboxModel) {
		return SandboxRun{}, ErrSandboxDenied
	}
	var scope, id string
	var expires int64
	var revoked bool
	err := c.db.QueryRowContext(ctx, "SELECT scope,run_id,expires,revoked FROM sandbox_caps WHERE token_hash=$1 AND purpose=$2", sandboxTokenHash(token), string(purpose)).Scan(&scope, &id, &expires, &revoked)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SandboxRun{}, ErrSandboxDenied
		}
		return SandboxRun{}, err
	}
	if revoked || !time.Now().Before(time.Unix(0, expires)) {
		return SandboxRun{}, ErrSandboxDenied
	}
	r, err := c.loadSandboxID(ctx, scope, id)
	if err != nil {
		return SandboxRun{}, err
	}
	if !r.CreateStarted || r.PodTerminal || r.PodDeleted ||
		(r.Output.Sealed && purpose != SandboxOutput) {
		return SandboxRun{}, ErrSandboxDenied
	}
	if err = c.sandboxActive(ctx, r); err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrMissing) {
			return SandboxRun{}, ErrSandboxDenied
		}
		return SandboxRun{}, err
	}
	return r, nil
}
