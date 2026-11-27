package platform

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
)

const AuditMaxLimit = 1000
const AuditSchema = "platform-audit.v1"
const auditMaxEventBytes = 8192

var ErrAudit = errors.New("audit store or query is invalid")

const auditSchema = `
CREATE TABLE IF NOT EXISTS audit_events (
 cursor BIGINT PRIMARY KEY, scope TEXT NOT NULL, event TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS audit_scope_cursor ON audit_events(scope,cursor);
`

// AuditActor identifies an authenticated API credential by a domain-separated
// digest. Background work has a fixed system source and no credential digest.
// No caller-supplied actor, request body, capability or secret is accepted.
type AuditActor struct {
	Source           string `json:"source"`
	Role             string `json:"role"`
	CredentialSHA256 string `json:"credential_sha256,omitempty"`
}

// AuditEvent has intentionally no general payload, path, message or error field.
// TaskID is the bounded allocation ID; arbitrary asset IDs are hashed instead.
type AuditEvent struct {
	Schema           string      `json:"schema"`
	Cursor           int64       `json:"cursor"`
	TimeUnixNano     int64       `json:"time_unix_nano"`
	Scope            asset.Scope `json:"scope"`
	Actor            AuditActor  `json:"actor"`
	Component        string      `json:"component"`
	Action           string      `json:"action"`
	TaskID           string      `json:"task_id"`
	TurnID           string      `json:"turn_id,omitempty"`
	AttemptSHA256    string      `json:"attempt_sha256,omitempty"`
	RunID            string      `json:"run_id,omitempty"`
	PodUID           string      `json:"pod_uid,omitempty"`
	AssetKind        string      `json:"asset_kind,omitempty"`
	AssetIDSHA256    string      `json:"asset_id_sha256,omitempty"`
	SessionIDSHA256  string      `json:"session_id_sha256,omitempty"`
	Version          int64       `json:"version,omitempty"`
	Digest           string      `json:"digest,omitempty"`
	PreviousState    string      `json:"previous_state,omitempty"`
	State            string      `json:"state,omitempty"`
	ModelRequests    int64       `json:"model_requests,omitempty"`
	ModelInputBytes  int64       `json:"model_input_bytes,omitempty"`
	ModelOutputBytes int64       `json:"model_output_bytes,omitempty"`
	OperationID      string      `json:"operation_id,omitempty"`
	ObjectKind       string      `json:"object_kind,omitempty"`
	ObjectIDSHA256   string      `json:"object_id_sha256,omitempty"`
	CollectionProof  string      `json:"collection_proof,omitempty"`
}

type auditActorContext struct{}

func auditHash(domain, value string) string {
	h := sha256.Sum256([]byte("agentmoor.audit.v1/" + domain + "\x00" + value))
	return hex.EncodeToString(h[:])
}

func auditHTTPContext(ctx context.Context, token, permission string) context.Context {
	role := "context"
	switch permission {
	case "read", "write", "submit", "execute":
		role = permission
	case "gc":
		role = "collect"
	}
	return context.WithValue(ctx, auditActorContext{}, AuditActor{Source: "http", Role: role, CredentialSHA256: auditHash("credential", token)})
}

func auditActor(ctx context.Context, source string) AuditActor {
	if actor, ok := ctx.Value(auditActorContext{}).(AuditActor); ok {
		return actor
	}
	return AuditActor{Source: source, Role: "system"}
}

// Called only after the Catalog has authenticated this exact capability and
// purpose. The capability identifies delegated run authority, not a person or
// platform credential; neither its value nor its hash is an audit actor field.
// A real authenticated platform HTTP actor remains the causal credential.
func auditCapabilityContext(ctx context.Context, purpose SandboxPurpose) context.Context {
	if actor, ok := ctx.Value(auditActorContext{}).(AuditActor); ok && actor.Source == "http" {
		return ctx
	}
	return context.WithValue(ctx, auditActorContext{}, AuditActor{Source: "guest-capability", Role: string(purpose)})
}

func auditTaskEvent(ctx context.Context, s taskAddress, before, after taskSlot) AuditEvent {
	prefix := "task."
	if s.turn != "" {
		prefix = "turn."
	}
	action := map[string]string{task.Queued: "queued", task.Running: "claimed", task.Cancelling: "cancel_requested", task.Cancelled: "cancelled", task.Succeeded: "succeeded", task.Failed: "failed", task.Interrupted: "interrupted"}[after.State]
	e := AuditEvent{Scope: s.Scope, Actor: auditActor(ctx, "internal"), Action: prefix + action, TaskID: s.Runtime, TurnID: s.turn, PreviousState: before.State, State: after.State}
	if after.attempt != "" {
		e.AttemptSHA256 = auditHash("attempt", after.attempt)
	}
	return e
}

func auditSandboxEvent(ctx context.Context, before *SandboxRun, after SandboxRun) (AuditEvent, bool) {
	action := ""
	previous := ""
	if before == nil {
		action = "begun"
	} else {
		previous = before.State
		switch {
		case !before.Collected && after.Collected:
			action = "collected"
		case !before.CreateStarted && after.CreateStarted:
			action = "create_fenced"
		case before.PodTemplateSHA != after.PodTemplateSHA:
			action = "template_sealed"
		case before.PodUID != after.PodUID:
			action = "pod_bound"
		case before.GuestStopProof == nil && after.GuestStopProof != nil:
			action = "guest_stop_proven"
		case before.PodNodeName != after.PodNodeName || before.GuestContainerID != after.GuestContainerID:
			action = "runtime_bound"
		case !before.PodTerminal && after.PodTerminal:
			action = "terminated"
		case !before.CleanupUnknown && after.CleanupUnknown:
			action = "cleanup_unconfirmed"
		case !before.PodDeleted && after.PodDeleted:
			action = "deleted"
		case !before.Output.Sealed && after.Output.Sealed:
			action = "output_sealed"
		case before.Output.SHA256 != after.Output.SHA256:
			action = "output_declared"
		case !before.Input.Sealed && after.Input.Sealed:
			action = "input_sealed"
		case before.State != after.State:
			action = map[string]string{SandboxCancelled: "cancelled", SandboxFailed: "failed", SandboxExpired: "expired", SandboxReady: "ready"}[after.State]
		}
	}
	if action == "" {
		return AuditEvent{}, false
	}
	e := AuditEvent{Scope: after.Scope, Actor: auditActor(ctx, "controller"), Action: "sandbox." + action, TaskID: after.Scope.Runtime, TurnID: after.TurnID, AttemptSHA256: auditHash("attempt", after.Attempt), RunID: after.ID, PodUID: after.PodUID, PreviousState: previous, State: after.State}
	if action == "begun" || action == "input_sealed" {
		e.Digest = after.Input.SHA256
	}
	if action == "output_declared" || action == "output_sealed" {
		e.Digest = after.Output.SHA256
	}
	return e, true
}

func auditState(s string) bool {
	switch s {
	case "", task.Queued, task.Running, task.Cancelling, task.Cancelled, task.Succeeded, task.Failed, task.Interrupted, SandboxPreparing, SandboxPrepared, SandboxCreating, SandboxReady, SandboxExpired:
		return true
	}
	return false
}

func (e AuditEvent) valid() bool {
	if e.Schema != AuditSchema || e.Cursor < 1 || e.TimeUnixNano < 1 || !e.Scope.Valid() || e.TaskID != e.Scope.Runtime || !auditState(e.State) || !auditState(e.PreviousState) {
		return false
	}
	for _, digest := range []string{e.Actor.CredentialSHA256, e.AttemptSHA256, e.RunID, e.AssetIDSHA256, e.SessionIDSHA256, e.Digest, e.ObjectIDSHA256} {
		if digest != "" && !sandboxDigest(digest) {
			return false
		}
	}
	if e.TurnID != "" && !sandboxUUID(e.TurnID) || e.PodUID != "" && !sandboxUUID(e.PodUID) || e.OperationID != "" && !sandboxUUID(e.OperationID) {
		return false
	}
	if !strings.HasPrefix(e.Action, "object.collection_") && (e.OperationID != "" || e.ObjectKind != "" || e.ObjectIDSHA256 != "" || e.CollectionProof != "") {
		return false
	}
	if e.SessionIDSHA256 != "" && e.Action != "asset.linked" {
		return false
	}
	if e.Actor.Source == "http" {
		if e.Actor.CredentialSHA256 == "" {
			return false
		}
		switch e.Actor.Role {
		case "context", "read", "write", "submit", "execute", "collect":
		default:
			return false
		}
	} else if e.Actor.Source == "guest-capability" {
		if e.Actor.CredentialSHA256 != "" || e.RunID == "" {
			return false
		}
		switch e.Actor.Role {
		case "output":
			if e.Action != "sandbox.output_declared" && e.Action != "sandbox.output_sealed" &&
				(e.Action != "object.collection_superseded" || e.ObjectKind != objectSandboxOutput || e.AttemptSHA256 == "") {
				return false
			}
		case "model":
			if e.Action != "model.reserved" {
				return false
			}
		default:
			return false
		}
	} else {
		switch e.Actor.Source {
		case "internal", "controller", "model-gateway":
		default:
			return false
		}
		if e.Actor.Role != "system" || e.Actor.CredentialSHA256 != "" {
			return false
		}
	}
	switch e.Action {
	case "object.collection_planned", "object.collection_confirmed", "object.collection_superseded":
		if e.Component != "object-collector" || e.OperationID == "" || e.ObjectIDSHA256 == "" ||
			e.TurnID != "" || e.AssetKind != "" || e.AssetIDSHA256 != "" || e.Version != 0 || e.Digest != "" ||
			e.State != "" || e.PreviousState != "" || e.PodUID != "" || e.ModelRequests != 0 || e.ModelInputBytes != 0 || e.ModelOutputBytes != 0 {
			return false
		}
		switch e.ObjectKind {
		case objectAssetBlob, objectWorkspacePart, objectSandboxInput, objectSandboxOutput:
		default:
			return false
		}
		if e.Action == "object.collection_confirmed" {
			if e.CollectionProof != "remove_ack" && e.CollectionProof != "absence" {
				return false
			}
		} else if e.CollectionProof != "" {
			return false
		}
		if e.Actor.Source == "http" {
			if e.Action == "object.collection_superseded" {
				if e.Actor.Role != "write" && e.Actor.Role != "execute" {
					return false
				}
			} else if e.Actor.Role != "collect" {
				return false
			}
		}
		if e.Actor.Source == "guest-capability" {
			return e.Action == "object.collection_superseded" && e.ObjectKind == objectSandboxOutput && e.RunID != "" && e.AttemptSHA256 != ""
		}
		return e.RunID == "" && e.AttemptSHA256 == ""
	case "task.queued", "task.claimed", "task.cancel_requested", "task.cancelled", "task.succeeded", "task.failed", "task.interrupted", "turn.queued", "turn.claimed", "turn.cancel_requested", "turn.cancelled", "turn.succeeded", "turn.failed", "turn.interrupted":
		_, suffix, _ := strings.Cut(e.Action, ".")
		state := map[string]string{"queued": task.Queued, "claimed": task.Running, "cancel_requested": task.Cancelling, "cancelled": task.Cancelled, "succeeded": task.Succeeded, "failed": task.Failed, "interrupted": task.Interrupted}[suffix]
		return e.Component == "task-store" && (strings.HasPrefix(e.Action, "turn.") == (e.TurnID != "")) && e.State == state && e.RunID == "" && e.AssetKind == "" && e.AssetIDSHA256 == "" && e.Version == 0 && e.Digest == "" && e.PodUID == "" && e.ModelRequests == 0 && e.ModelInputBytes == 0 && e.ModelOutputBytes == 0
	case "revision.committed":
		return e.Component == "revision-store" && e.AssetIDSHA256 != "" && e.Digest != "" && e.Version > 0 && (asset.Key{Kind: e.AssetKind, ID: "audit"}).Valid() && e.RunID == "" && e.State == "" && e.PreviousState == "" && e.PodUID == "" && e.ModelRequests == 0 && e.ModelInputBytes == 0 && e.ModelOutputBytes == 0
	case "asset.linked":
		return e.Component == "task-store" && e.AttemptSHA256 != "" && e.AssetIDSHA256 != "" && e.SessionIDSHA256 != "" && e.Digest != "" && e.Version > 0 &&
			(e.AssetKind == "artifact" || e.AssetKind == "workspace" || e.AssetKind == "session") && e.State == task.Succeeded && e.PreviousState == "" &&
			e.RunID == "" && e.PodUID == "" && e.ModelRequests == 0 && e.ModelInputBytes == 0 && e.ModelOutputBytes == 0
	case "sandbox.begun", "sandbox.collected", "sandbox.create_fenced", "sandbox.template_sealed", "sandbox.pod_bound", "sandbox.runtime_bound", "sandbox.terminated", "sandbox.guest_stop_proven", "sandbox.cleanup_unconfirmed", "sandbox.deleted", "sandbox.output_sealed", "sandbox.output_declared", "sandbox.input_sealed", "sandbox.cancelled", "sandbox.failed", "sandbox.expired", "sandbox.ready":
		return e.Component == "sandbox-controller" && e.RunID != "" && e.AttemptSHA256 != "" && e.State != "" && e.AssetKind == "" && e.AssetIDSHA256 == "" && e.Version == 0 && e.ModelRequests == 0 && e.ModelInputBytes == 0 && e.ModelOutputBytes == 0
	case "model.reserved":
		return e.Component == "model-gateway" && e.RunID != "" && e.AttemptSHA256 != "" && e.Digest != "" && e.AssetKind == "" && e.AssetIDSHA256 == "" && e.Version == 0 && e.State == "" && e.PreviousState == "" && e.ModelRequests > 0 && e.ModelRequests <= SandboxModelMaxRequests && e.ModelInputBytes >= e.ModelRequests && e.ModelInputBytes <= SandboxModelMaxInputBytes && e.ModelOutputBytes > 0 && e.ModelOutputBytes <= SandboxModelMaxOutputBytes
	}
	return false
}

// appendAudit is called only from a business transaction on the leased writer,
// under Catalog.mu. No sequence is consumed when either write rolls back.
func appendAudit(ctx context.Context, tx *sql.Tx, event AuditEvent) error {
	var last int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(cursor),0) FROM audit_events").Scan(&last); err != nil {
		return err
	}
	if last == math.MaxInt64 {
		return ErrAudit
	}
	event.Cursor, event.TimeUnixNano = last+1, time.Now().UTC().UnixNano()
	event.Schema = AuditSchema
	switch {
	case strings.HasPrefix(event.Action, "object.collection_"):
		event.Component = "object-collector"
	case strings.HasPrefix(event.Action, "task."), strings.HasPrefix(event.Action, "turn."), event.Action == "asset.linked":
		event.Component = "task-store"
	case event.Action == "revision.committed":
		event.Component = "revision-store"
	case strings.HasPrefix(event.Action, "sandbox."):
		event.Component = "sandbox-controller"
	case event.Action == "model.reserved":
		event.Component = "model-gateway"
	}
	if !event.valid() {
		return ErrAudit
	}
	raw, err := json.Marshal(event)
	if err != nil || len(raw) > auditMaxEventBytes {
		return ErrAudit
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO audit_events(cursor,scope,event) VALUES($1,$2,$3)", event.Cursor, event.Scope.Prefix(), string(raw))
	return err
}

// AuditReader is an operator-only read backend. Opening it neither obtains the
// catalog writer lease nor creates a database, schema, object store or secret.
type AuditReader struct{ db *sql.DB }
type AuditPage struct {
	Events []AuditEvent `json:"events"`
	Next   int64        `json:"next"`
	Total  int64        `json:"total"`
}

// Stored events are small, but Token walking still needs explicit structural
// bounds. Reject duplicate/case-folded members and null rather than accepting a
// corrupted event that has a different canonical meaning.
func decodeAudit(raw string, event *AuditEvent) error {
	d := json.NewDecoder(strings.NewReader(raw))
	nodes := 0
	var walk func(int) error
	walk = func(depth int) error {
		nodes++
		if depth > 8 || nodes > 256 {
			return ErrAudit
		}
		t, err := d.Token()
		if err != nil || t == nil {
			return ErrAudit
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		if delim != '{' {
			return ErrAudit
		}
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			key, ok := k.(string)
			if err != nil || !ok || key == "" || seen[key] || strings.Trim(key, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
				return ErrAudit
			}
			seen[key] = true
			if err = walk(depth + 1); err != nil {
				return err
			}
		}
		if t, err = d.Token(); err != nil || t != json.Delim('}') {
			return ErrAudit
		}
		return nil
	}
	if walk(0) != nil {
		return ErrAudit
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrAudit
	}
	d = json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(event) != nil {
		return ErrAudit
	}
	return nil
}

func OpenAuditReader(ctx context.Context, dir, postgres string) (*AuditReader, error) {
	driver, dsn := "postgres", postgres
	if postgres == "" {
		if !filepath.IsAbs(dir) || strings.ContainsRune(dir, 0) {
			return nil, ErrAudit
		}
		u := url.URL{Scheme: "file", Path: filepath.Join(dir, "catalog.sqlite")}
		q := u.Query()
		q.Set("mode", "ro")
		q.Set("_query_only", "true")
		q.Set("_busy_timeout", "5000")
		u.RawQuery = q.Encode()
		driver, dsn = "sqlite3", u.String()
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, ErrAudit
	}
	db.SetMaxOpenConns(2)
	r := &AuditReader{db: db}
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, ErrAudit
	}
	// An old catalog is upgraded only by its leased service owner. The audit
	// reader verifies its fixed columns through a read-only transaction and
	// refuses missing schema rather than silently initializing it.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		db.Close()
		return nil, ErrAudit
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT cursor,scope,event FROM audit_events WHERE 1=0")
	if err != nil {
		tx.Rollback()
		db.Close()
		return nil, ErrAudit
	}
	if err = rows.Close(); err != nil {
		tx.Rollback()
		db.Close()
		return nil, ErrAudit
	}
	if err = tx.Commit(); err != nil {
		db.Close()
		return nil, ErrAudit
	}
	return r, nil
}

func (r *AuditReader) Close() error { return r.db.Close() }

func (r *AuditReader) Page(ctx context.Context, scope asset.Scope, after int64, limit int) (AuditPage, error) {
	if limit == 0 {
		limit = 100
	}
	page := AuditPage{Events: []AuditEvent{}, Next: after}
	if r == nil || r.db == nil || !scope.Valid() || after < 0 || limit < 1 || limit > AuditMaxLimit {
		return page, ErrAudit
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return page, ErrAudit
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM audit_events WHERE scope=$1", scope.Prefix()).Scan(&page.Total); err != nil {
		return page, ErrAudit
	}
	rows, err := tx.QueryContext(ctx, "SELECT cursor,length(event),substr(event,1,$1) FROM audit_events WHERE scope=$2 AND cursor>$3 ORDER BY cursor LIMIT $4", auditMaxEventBytes+1, scope.Prefix(), after, limit)
	if err != nil {
		return page, ErrAudit
	}
	defer rows.Close()
	for rows.Next() {
		var cursor, length int64
		var raw string
		if err = rows.Scan(&cursor, &length, &raw); err != nil || length < 1 || length > auditMaxEventBytes || len(raw) > auditMaxEventBytes {
			return AuditPage{}, ErrAudit
		}
		var event AuditEvent
		if decodeAudit(raw, &event) != nil || !event.valid() || event.Scope != scope || event.Cursor != cursor || cursor <= page.Next {
			return AuditPage{}, ErrAudit
		}
		page.Events = append(page.Events, event)
		page.Next = cursor
	}
	if rows.Err() != nil {
		return AuditPage{}, ErrAudit
	}
	if err = rows.Close(); err != nil {
		return AuditPage{}, ErrAudit
	}
	if err = tx.Commit(); err != nil {
		return AuditPage{}, ErrAudit
	}
	return page, nil
}
