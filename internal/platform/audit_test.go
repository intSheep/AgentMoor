package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

type auditFixture struct {
	c       *Catalog
	r       *AuditReader
	dir     string
	objects DiskObjects
}

func newAuditFixture(t *testing.T) auditFixture {
	t.Helper()
	f := auditFixture{dir: t.TempDir(), objects: DiskObjects{Dir: t.TempDir()}}
	var err error
	f.c, err = OpenCatalog(t.Context(), f.dir, "", f.objects)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.c.Close() })
	f.r, err = OpenAuditReader(t.Context(), f.dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.r.Close() })
	return f
}

func auditEvents(t *testing.T, r *AuditReader, s asset.Scope) []AuditEvent {
	t.Helper()
	p, err := r.Page(t.Context(), s, 0, AuditMaxLimit)
	if err != nil || p.Total != int64(len(p.Events)) {
		t.Fatal("cannot read complete owned audit fixture", err, p.Total, len(p.Events))
	}
	return p.Events
}

func rejectAuditWrites(t *testing.T, c *Catalog) func() {
	t.Helper()
	if _, err := c.db.ExecContext(t.Context(), `CREATE TRIGGER reject_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'owned audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	remove := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := c.db.ExecContext(ctx, "DROP TRIGGER IF EXISTS reject_audit"); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(remove)
	return remove
}

func requireAuditCount(t *testing.T, f auditFixture, s asset.Scope, want int) {
	t.Helper()
	if got := len(auditEvents(t, f.r, s)); got != want {
		t.Fatalf("audit events=%d, want %d", got, want)
	}
}

func TestAuditFailureRollsBackTaskTransitions(t *testing.T) {
	for _, transition := range []string{"submit", "claim", "cancel", "finish"} {
		t.Run(transition, func(t *testing.T) {
			f := newAuditFixture(t)
			s := scope("audit-task-" + transition)
			a := taskAddress{Scope: s}
			attempt := uuid.NewString()
			if transition != "submit" {
				if _, err := f.c.submitTask(t.Context(), a, task.Spec{Instruction: "synthetic task", Output: "report.txt"}); err != nil {
					t.Fatal(err)
				}
			}
			if transition == "cancel" || transition == "finish" {
				if _, err := f.c.claimTask(t.Context(), a, task.Claim{Attempt: attempt}); err != nil {
					t.Fatal(err)
				}
			}
			before, beforeErr := f.c.readStoredTask(t.Context(), a)
			count := len(auditEvents(t, f.r, s))
			remove := rejectAuditWrites(t, f.c)
			apply := func() error {
				var err error
				switch transition {
				case "submit":
					_, err = f.c.submitTask(t.Context(), a, task.Spec{Instruction: "synthetic task", Output: "report.txt"})
				case "claim":
					_, err = f.c.claimTask(t.Context(), a, task.Claim{Attempt: attempt})
				case "cancel":
					_, err = f.c.cancelTask(t.Context(), a)
				case "finish":
					_, err = f.c.finishTask(t.Context(), a, task.Finish{Attempt: attempt, Error: "synthetic failed execution"})
				}
				return err
			}
			if err := apply(); err == nil {
				t.Fatal("audit failure did not reject the business transition")
			}
			after, afterErr := f.c.readStoredTask(t.Context(), a)
			if !reflect.DeepEqual(after, before) || !errors.Is(afterErr, beforeErr) {
				t.Fatal("business transition escaped its failed audit transaction")
			}
			requireAuditCount(t, f, s, count)
			remove()
			if err := apply(); err != nil {
				t.Fatal("business retry failed after removing the actual audit fault", err)
			}
			requireAuditCount(t, f, s, count+1)
			if err := apply(); err != nil {
				t.Fatal("ACK replay failed", err)
			}
			requireAuditCount(t, f, s, count+1)
		})
	}
}

func TestAuditFailureRollsBackRevisionCommitAndACKReplay(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("audit-revision")
	ticket := upload(t, f.c, s, change("original immutable bytes", 0), "original immutable bytes")
	remove := rejectAuditWrites(t, f.c)
	if _, err := f.c.Commit(t.Context(), s, ticket.ID); err == nil {
		t.Fatal("commit survived a failed audit insert")
	}
	if heads, err := f.c.Heads(t.Context(), s); err != nil || len(heads) != 0 {
		t.Fatal("failed audit committed a revision", heads, err)
	}
	op, err := getOp(t.Context(), f.c.db, s.Prefix(), ticket.ID)
	if err != nil || op.state != "pending" {
		t.Fatal("failed audit changed the operation state", op.state, err)
	}
	requireAuditCount(t, f, s, 0)
	remove()
	for range 2 {
		if version, err := f.c.Commit(t.Context(), s, ticket.ID); err != nil || version != 1 {
			t.Fatal(version, err)
		}
	}
	requireAuditCount(t, f, s, 1)
}

func TestAuditFailureRollsBackSandboxAuthorityAndBudget(t *testing.T) {
	for _, transition := range []string{"begin", "fence", "cancel", "new-budget", "existing-budget"} {
		t.Run(transition, func(t *testing.T) {
			f := newAuditFixture(t)
			s := scope("audit-sandbox-" + transition)
			a := sandboxTestClaim(t, f.c, s)
			input := "private original envelope"
			var permit SandboxCreatePermit
			if transition != "begin" {
				sandboxTestBegin(t, f.c, a, input)
				if _, err := f.c.SealSandboxInput(t.Context(), a, strings.NewReader(input)); err != nil {
					t.Fatal(err)
				}
			}
			if transition == "cancel" || strings.HasSuffix(transition, "budget") {
				run, err := f.c.SandboxRun(t.Context(), a)
				if err != nil {
					t.Fatal(err)
				}
				permit, err = f.c.FenceSandboxCreate(t.Context(), a, run.Deadline)
				if err != nil {
					t.Fatal(err)
				}
			}
			policy := sandboxModelTestPolicy()
			if transition == "existing-budget" {
				if _, err := f.c.ReserveSandboxModel(t.Context(), permit.Capabilities.Model, policy, 4); err != nil {
					t.Fatal(err)
				}
			}
			before, beforeErr := f.c.loadSandbox(t.Context(), a)
			budget, budgetErr := f.c.readSandboxModelBudget(t.Context(), before)
			count := len(auditEvents(t, f.r, s))
			remove := rejectAuditWrites(t, f.c)
			apply := func() error {
				var err error
				switch transition {
				case "begin":
					_, err = f.c.BeginSandbox(t.Context(), a, sandboxTestProfile, sandboxTestBlob(input))
				case "fence":
					_, err = f.c.FenceSandboxCreate(t.Context(), a, before.Deadline)
				case "cancel":
					_, err = f.c.CancelSandbox(t.Context(), a)
				default:
					_, err = f.c.ReserveSandboxModel(t.Context(), permit.Capabilities.Model, policy, 4)
				}
				return err
			}
			if err := apply(); err == nil {
				t.Fatal("audit failure did not reject sandbox or budget mutation")
			}
			after, afterErr := f.c.loadSandbox(t.Context(), a)
			if !reflect.DeepEqual(before, after) || !errors.Is(afterErr, beforeErr) {
				t.Fatal("sandbox state escaped audit rollback")
			}
			if transition == "fence" {
				var caps int
				if err := f.c.db.QueryRowContext(t.Context(), "SELECT count(*) FROM sandbox_caps WHERE scope=$1", s.Prefix()).Scan(&caps); err != nil || caps != 0 {
					t.Fatal("failed fence leaked a capability", caps, err)
				}
			}
			if transition == "cancel" {
				for purpose, token := range map[SandboxPurpose]string{SandboxInput: permit.Capabilities.Input, SandboxOutput: permit.Capabilities.Output, SandboxModel: permit.Capabilities.Model} {
					if _, err := f.c.AuthenticateSandboxCapability(t.Context(), token, purpose); err != nil {
						t.Fatal("failed audit nevertheless revoked the live capability", err)
					}
				}
			}
			if strings.HasSuffix(transition, "budget") {
				afterBudget, err := f.c.readSandboxModelBudget(t.Context(), before)
				if !reflect.DeepEqual(afterBudget, budget) || !errors.Is(err, budgetErr) {
					t.Fatal("failed audit consumed budget", err)
				}
			}
			requireAuditCount(t, f, s, count)
			remove()
			if err := apply(); err != nil {
				t.Fatal("retry failed after removing the audit fault", err)
			}
			requireAuditCount(t, f, s, count+1)
		})
	}
}

func TestAuditACKReplaySurvivesActualCatalogRestart(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("audit-restart")
	a := sandboxTestClaim(t, f.c, s)
	ticket := upload(t, f.c, s, change("restart immutable bytes", 0), "restart immutable bytes")
	if _, err := f.c.Commit(t.Context(), s, ticket.ID); err != nil {
		t.Fatal(err)
	}
	permit := sandboxTestFence(t, f.c, a, "sealed input")
	before := auditEvents(t, f.r, s)
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := OpenCatalog(t.Context(), f.dir, "", f.objects)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.submitTask(t.Context(), taskAddress{Scope: s}, task.Spec{Instruction: "make a report", Input: "private fixture", Output: "report.json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.claimTask(t.Context(), taskAddress{Scope: s}, task.Claim{Attempt: a.Attempt}); err != nil {
		t.Fatal(err)
	}
	if version, err := c.Commit(t.Context(), s, ticket.ID); err != nil || version != 1 {
		t.Fatal(version, err)
	}
	if replay, err := c.FenceSandboxCreate(t.Context(), a, permit.Run.Deadline); err != nil || replay.CreateAllowed || replay.Capabilities != (SandboxCapabilities{}) {
		t.Fatal("restart granted another execution", err)
	}
	if after := auditEvents(t, f.r, s); !reflect.DeepEqual(before, after) {
		t.Fatal("ACK replay or reopening duplicated audit evidence")
	}
}

func TestAuditHTTPActorsContainHashesAndNoSensitivePayloads(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("audit-private-http")
	secret := []byte("synthetic-platform-signing-SECRET_KEY-with-enough-bytes")
	api := httptest.NewServer((&API{Catalog: f.c, Secret: secret, Temp: t.TempDir(), TaskAdapters: map[string]bool{s.Adapter: true}}).Handler())
	defer api.Close()
	makeClient := func(grant Grant) (*Client, string) {
		grant.Scope, grant.Expires = s, time.Now().Add(time.Hour).Unix()
		token, err := Issue(secret, grant, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		client, err := NewClient(api.URL, token)
		if err != nil {
			t.Fatal(err)
		}
		return client, token
	}
	saas, submitToken := makeClient(Grant{Read: true, Submit: true})
	worker, workerToken := makeClient(Grant{Read: true, Write: true, Execute: true})
	const prompt, privatePath, skillBody, sessionBody = "SECRET_PROMPT_MARKER", "PRIVATE_SKILL_PATH", "SECRET_SKILL_CONTENT", "SECRET_NATIVE_SESSION_CONTENT"
	var record task.Record
	if err := saas.JSON(t.Context(), "PUT", "/v1/tasks/"+s.Runtime, task.Spec{Instruction: prompt, Input: prompt, Output: "report.txt"}, &record); err != nil {
		t.Fatal(err)
	}
	post := func(key asset.Key, raw []byte, session string) task.Ref {
		file := filepath.Join(t.TempDir(), "original")
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
		ch := change(string(raw), 0)
		ch.Key, ch.SessionID = key, session
		if key.Kind == "skill" {
			ch.Format, ch.MediaType = "skill-tar.v1", "application/x-tar"
		}
		version, err := worker.Publish(t.Context(), ch, file)
		if err != nil {
			t.Fatal(err)
		}
		return task.Ref{Key: key, Version: version, SHA256: ch.SHA256, ContentPath: key.ContentPath(version)}
	}
	skill := post(asset.Key{Kind: "skill", ID: "catalog/" + privatePath + "/SKILL.md"}, handoffTree(t, "SKILL.md", skillBody), "")
	attempt, session := uuid.NewString(), uuid.NewString()
	if err := worker.JSON(t.Context(), "POST", "/v1/tasks/"+s.Runtime+"/claim", task.Claim{Attempt: attempt, Skills: []task.SkillPin{{Name: "private", Source: skill}}}, &record); err != nil {
		t.Fatal(err)
	}
	for _, key := range []asset.Key{{Kind: "session", ID: session}, {Kind: "workspace", ID: "root"}, {Kind: "artifact", ID: "out/report.txt"}} {
		post(key, []byte(sessionBody), session)
	}
	if err := worker.JSON(t.Context(), "POST", "/v1/tasks/"+s.Runtime+"/finish", task.Finish{Attempt: attempt, SessionID: session}, &record); err != nil {
		t.Fatal(err)
	}
	events := auditEvents(t, f.r, s)
	seen := map[string]int{}
	for _, event := range events {
		seen[event.Action]++
		wantToken, wantRole := workerToken, "write"
		if event.Action == "task.queued" {
			wantToken, wantRole = submitToken, "submit"
		}
		if event.Action == "task.claimed" || event.Action == "task.succeeded" || event.Action == "asset.linked" {
			wantRole = "execute"
		}
		if event.Actor.Source != "http" || event.Actor.Role != wantRole || event.Actor.CredentialSHA256 != auditHash("credential", wantToken) {
			t.Fatal("HTTP audit actor was not bound to the authenticated credential and route right", event.Action)
		}
		if event.Action == "task.claimed" && event.AttemptSHA256 != auditHash("attempt", attempt) {
			t.Fatal("private attempt was not domain hashed")
		}
		if event.AssetKind == "skill" && event.AssetIDSHA256 != auditHash("asset-id/skill", skill.ID) {
			t.Fatal("arbitrary asset path was not domain hashed")
		}
	}
	if seen["task.queued"] != 1 || seen["task.claimed"] != 1 || seen["task.succeeded"] != 1 || seen["revision.committed"] != 4 {
		t.Fatal("audit transition matrix incomplete", seen)
	}
	modelScope := scope("audit-private-model")
	p := sandboxTestFence(t, f.c, sandboxTestClaim(t, f.c, modelScope), "SECRET_SANDBOX_INPUT")
	if _, err := f.c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, sandboxModelTestPolicy(), 3); err != nil {
		t.Fatal(err)
	}
	events = append(events, auditEvents(t, f.r, modelScope)...)
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{prompt, privatePath, skillBody, sessionBody, session, attempt, submitToken, workerToken, string(secret), "SECRET_SANDBOX_INPUT", p.Capabilities.Input, p.Capabilities.Output, p.Capabilities.Model, f.dir} {
		if bytes.Contains(raw, []byte(sensitive)) {
			t.Fatal("audit evidence persisted a sensitive string")
		}
	}
	for _, event := range events {
		if event.Action == "model.reserved" && (event.Actor.Source != "guest-capability" || event.Actor.Role != "model" || event.Actor.CredentialSHA256 != "") {
			t.Fatal("model capability was recorded as an HTTP credential")
		}
	}
}
