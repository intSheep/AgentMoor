package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

type provenanceObjects struct {
	Objects
	gets      atomic.Int64
	beforeGet func()
}

func (o *provenanceObjects) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	o.gets.Add(1)
	if o.beforeGet != nil {
		o.beforeGet()
	}
	return o.Objects.Get(ctx, key)
}

type provenanceFixture struct {
	auditFixture
	address SandboxAddress
	profile SandboxExecutionProfile
	run     SandboxRun
	finish  task.Finish
	client  *Client
	token   string
	api     *API
	objects *provenanceObjects
}

func newProvenanceFixture(t *testing.T) *provenanceFixture {
	t.Helper()
	return newProvenanceFixtureWithCatalog(t, newAuditFixture(t), scope("provenance"))
}

func newProvenanceFixtureWithCatalog(t *testing.T, store auditFixture, s asset.Scope) *provenanceFixture {
	t.Helper()
	f := &provenanceFixture{auditFixture: store}
	s.Adapter = "handoff-fixture"
	f.objects = &provenanceObjects{Objects: f.c.objects}
	f.c.objects = f.objects
	f.address = handoffClaim(t, f.c, s, nil)
	f.profile = handoffProfile(s)
	session := uuid.NewString()
	primary := handoffPublish(t, f.c, s, asset.Key{Kind: "artifact", ID: "out/report.txt"}, []byte("private original report"), "file.v1", "text/plain", session, 0)
	second := handoffPublish(t, f.c, s, asset.Key{Kind: "artifact", ID: "out/details.txt"}, []byte("private original details"), "file.v1", "text/plain", session, 0)
	handoffPublish(t, f.c, s, asset.Key{Kind: "workspace", ID: "root"}, handoffTree(t, "input.txt", "saved original input"), "workspace-tar.v1", "application/x-tar", "", 0)
	handoffPublish(t, f.c, s, asset.Key{Kind: "session", ID: session}, handoffTree(t, "native.session", "saved opaque original session"), "fixture-session.v1", "application/x-tar", "", 0)
	f.run = handoffReadyForFinish(t, f.c, f.address, f.profile, nil)
	f.finish = task.Finish{Attempt: f.address.Attempt, SessionID: session, Artifacts: []task.Ref{primary, second}}
	secret := bytes.Repeat([]byte{73}, 32)
	f.api = &API{Catalog: f.c, Secret: secret, Temp: t.TempDir(), SandboxControl: &handoffControl{c: f.c}, SandboxProfiles: func(got asset.Scope) (SandboxExecutionProfile, bool) { return f.profile, got == s }}
	server := httptest.NewServer(f.api.Handler())
	t.Cleanup(server.Close)
	var err error
	f.token, err = Issue(secret, Grant{Scope: s, Read: true, Write: true, Execute: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.client, err = NewClient(server.URL, f.token)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *provenanceFixture) route() string { return "/v1/tasks/" + f.address.Scope.Runtime + "/finish" }
func (f *provenanceFixture) mutateRun(t *testing.T, change func(*SandboxRun)) {
	t.Helper()
	r := f.run
	change(&r)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	collected := 0
	if r.Collected {
		collected = 1
	}
	if _, err = f.c.db.ExecContext(t.Context(), "UPDATE sandbox_runs SET record=$1,collected=$2 WHERE scope=$3 AND turn_id=$4 AND attempt=$5", string(raw), collected, f.address.Scope.Prefix(), f.address.TurnID, f.address.Attempt); err != nil {
		t.Fatal(err)
	}
}
func provenanceAccepted(events []AuditEvent) []AuditEvent {
	var out []AuditEvent
	for _, e := range events {
		if e.Action == "sandbox.result_accepted" {
			out = append(out, e)
		}
	}
	return out
}

func TestSandboxProvenanceActualHTTPAcceptedAndACKReplay(t *testing.T) {
	f := newProvenanceFixture(t)
	before := len(auditEvents(t, f.r, f.address.Scope))
	var row task.Record
	if err := f.client.JSON(t.Context(), "POST", f.route(), f.finish, &row); err != nil || row.State != task.Succeeded {
		t.Fatal("actual HTTP receipt failed", row.State, err)
	}
	events := auditEvents(t, f.r, f.address.Scope)
	accepted := provenanceAccepted(events)
	if len(accepted) != 1 {
		t.Fatal("isolated success did not have one accepted source", len(accepted))
	}
	e := accepted[0]
	if e.RunID != f.run.ID || e.PodUID != f.run.PodUID || e.Digest != f.run.Output.SHA256 || e.Scope != f.address.Scope || e.TaskID != row.ID || e.TurnID != row.TurnID || e.AttemptSHA256 != auditHash("attempt", f.address.Attempt) || e.Actor.CredentialSHA256 != auditHash("credential", f.token) || e.Actor.Role != "execute" || e.Component != "task-store" {
		t.Fatal("accepted source was not bound to actual claim, Run, sealed output and credential")
	}
	links := assetLinkEvents(events)
	if len(links) != 4 || len(events) != before+6 {
		t.Fatal("success, accepted source and complete links did not commit together", len(events)-before, len(links))
	}
	for _, link := range links {
		if link.Cursor <= e.Cursor || link.AttemptSHA256 != e.AttemptSHA256 {
			t.Fatal("asset link is not after the same accepted execution")
		}
	}
	encoded, _ := json.Marshal(events)
	for _, secret := range []string{f.token, f.finish.Attempt, f.finish.SessionID, "private original report", "private original details", "saved opaque original session", "out/report.txt"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatal("accepted-source audit included a credential, private identity, path or body")
		}
	}
	reads := f.objects.gets.Load()
	// Already-completed ACK confirmation does not depend on provider config,
	// old input availability or an expired deadline.
	f.api.SandboxControl = nil
	f.api.SandboxProfiles = nil
	if err := os.Remove(filepath.Join(f.auditFixture.objects.Dir, sandboxObjectKey(f.address.Scope, SandboxInput, f.run.Input.SHA256))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.db.ExecContext(t.Context(), "UPDATE tasks SET record=$1 WHERE scope=$2", mustProvenanceTaskJSON(t, f.c, f.address.Scope, func(r *task.Record) { r.Deadline = time.Now().Add(-time.Hour) }), f.address.Scope.Prefix()); err != nil {
		t.Fatal(err)
	}
	if err := f.client.JSON(t.Context(), "POST", f.route(), f.finish, &row); err != nil || row.State != task.Succeeded {
		t.Fatal("successful ACK replay depended on retired input/profile", err)
	}
	if len(auditEvents(t, f.r, f.address.Scope)) != len(events) || f.objects.gets.Load() != reads {
		t.Fatal("ACK replay emitted events or reread old input")
	}
}

func mustProvenanceTaskJSON(t *testing.T, c *Catalog, s asset.Scope, change func(*task.Record)) string {
	t.Helper()
	slot, err := c.readStoredTask(t.Context(), taskAddress{Scope: s})
	if err != nil {
		t.Fatal(err)
	}
	change(&slot.Record)
	raw, err := json.Marshal(slot.Record)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSandboxProvenanceRejectsUnboundOrUnreadyReceiptBeforeObjectRead(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, *provenanceFixture)
	}{
		{"missing-run", func(t *testing.T, f *provenanceFixture) {
			_, err := f.c.db.ExecContext(t.Context(), "DELETE FROM sandbox_runs WHERE scope=$1", f.address.Scope.Prefix())
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong-attempt", func(_ *testing.T, f *provenanceFixture) { f.finish.Attempt = uuid.NewString() }},
		{"missing-profile", func(_ *testing.T, f *provenanceFixture) { f.api.SandboxProfiles = nil }},
		{"missing-provider", func(_ *testing.T, f *provenanceFixture) { f.api.SandboxControl = nil }},
		{"profile-drift", func(_ *testing.T, f *provenanceFixture) {
			f.profile.Fingerprint = sandboxTestBlob("new profile").SHA256
		}},
		{"not-ready", func(t *testing.T, f *provenanceFixture) {
			f.mutateRun(t, func(r *SandboxRun) { r.State = SandboxRunning })
		}},
		{"not-deleted", func(t *testing.T, f *provenanceFixture) { f.mutateRun(t, func(r *SandboxRun) { r.PodDeleted = false }) }},
		{"not-terminal", func(t *testing.T, f *provenanceFixture) {
			f.mutateRun(t, func(r *SandboxRun) { r.PodTerminal = false })
		}},
		{"nonzero-exit", func(t *testing.T, f *provenanceFixture) { f.mutateRun(t, func(r *SandboxRun) { r.ExitCode = 1 }) }},
		{"unsealed-output", func(t *testing.T, f *provenanceFixture) {
			f.mutateRun(t, func(r *SandboxRun) { r.Output.Sealed = false })
		}},
		{"collected", func(t *testing.T, f *provenanceFixture) { f.mutateRun(t, func(r *SandboxRun) { r.Collected = true }) }},
		{"bad-pod-uid", func(t *testing.T, f *provenanceFixture) {
			f.mutateRun(t, func(r *SandboxRun) { r.PodUID = "not-a-pod-uid" })
		}},
		{"wrong-scope", func(t *testing.T, f *provenanceFixture) {
			f.mutateRun(t, func(r *SandboxRun) { r.Scope.Tenant = "other" })
		}},
		{"expired", func(t *testing.T, f *provenanceFixture) {
			raw := mustProvenanceTaskJSON(t, f.c, f.address.Scope, func(r *task.Record) { r.Deadline = time.Now().Add(-time.Second) })
			if _, err := f.c.db.ExecContext(t.Context(), "UPDATE tasks SET record=$1 WHERE scope=$2", raw, f.address.Scope.Prefix()); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newProvenanceFixture(t)
			before := len(auditEvents(t, f.r, f.address.Scope))
			test.change(t, f)
			var row task.Record
			if err := f.client.JSON(t.Context(), "POST", f.route(), f.finish, &row); err == nil {
				t.Fatal("unbound isolated result reported success")
			}
			stored, err := f.c.readStoredTask(t.Context(), taskAddress{Scope: f.address.Scope})
			if err != nil || stored.State == task.Succeeded || len(stored.Assets) != 0 {
				t.Fatal("failed provenance changed success state", err)
			}
			if len(auditEvents(t, f.r, f.address.Scope)) != before || f.objects.gets.Load() != 0 {
				t.Fatal("early rejection wrote an event or fetched input")
			}
		})
	}
}

func TestSandboxProvenanceInspectRejectsVersionDriftAndEqualSizeCorruption(t *testing.T) {
	for _, kind := range []string{"adapter-version", "runtime-version", "outer-corruption"} {
		t.Run(kind, func(t *testing.T) {
			f := newProvenanceFixture(t)
			before := len(auditEvents(t, f.r, f.address.Scope))
			switch kind {
			case "adapter-version":
				f.profile.AdapterVersion = "new adapter"
			case "runtime-version":
				f.profile.RuntimeVersion = "new native runtime"
			case "outer-corruption":
				path := filepath.Join(f.auditFixture.objects.Dir, sandboxObjectKey(f.address.Scope, SandboxInput, f.run.Input.SHA256))
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-1] ^= 1
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var row task.Record
			if err := f.client.JSON(t.Context(), "POST", f.route(), f.finish, &row); err == nil {
				t.Fatal("sealed input provenance was assumed instead of inspected")
			}
			if f.objects.gets.Load() != 1 || len(auditEvents(t, f.r, f.address.Scope)) != before {
				t.Fatal("inspection was not bounded to one failed input read")
			}
		})
	}
}

func TestSandboxProvenanceRechecksCancellationAndProfileAfterUnlockedIO(t *testing.T) {
	for _, mode := range []string{"cancel", "profile", "output"} {
		t.Run(mode, func(t *testing.T) {
			f := newProvenanceFixture(t)
			f.objects.beforeGet = func() {
				if mode == "output" {
					f.mutateRun(t, func(r *SandboxRun) { r.Output.SHA256 = sandboxTestBlob("different sealed output").SHA256 })
					return
				}
				if mode == "profile" {
					f.profile.RuntimeVersion = "changed during input read"
					return
				}
				done := make(chan error, 1)
				go func() { _, err := f.c.cancelTask(t.Context(), taskAddress{Scope: f.address.Scope}); done <- err }()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(2 * time.Second):
					t.Error("input I/O held the mutation lock against cancellation")
				}
			}
			var row task.Record
			err := f.client.JSON(t.Context(), "POST", f.route(), f.finish, &row)
			if mode != "cancel" && err == nil || mode == "cancel" && (err != nil || row.State != task.Cancelled) {
				t.Fatal("lock recheck or original cancellation acknowledgement changed", mode, row.State, err)
			}
			if len(provenanceAccepted(auditEvents(t, f.r, f.address.Scope))) != 0 || len(assetLinkEvents(auditEvents(t, f.r, f.address.Scope))) != 0 {
				t.Fatal("stopped or changed allocation acquired accepted assets")
			}
		})
	}
}

func TestSandboxProvenanceAuditFailureRollsBackSuccessAcceptedAndLinks(t *testing.T) {
	for _, at := range []string{"accepted", "second-link"} {
		t.Run(at, func(t *testing.T) {
			f := newProvenanceFixture(t)
			before := len(auditEvents(t, f.r, f.address.Scope))
			condition := `NEW.event LIKE '%"action":"sandbox.result_accepted"%'`
			if at == "second-link" {
				condition = `NEW.event LIKE '%"action":"asset.linked"%' AND (SELECT count(*) FROM audit_events WHERE event LIKE '%"action":"asset.linked"%')=1`
			}
			if _, err := f.c.db.ExecContext(t.Context(), "CREATE TRIGGER reject_provenance BEFORE INSERT ON audit_events WHEN "+condition+" BEGIN SELECT RAISE(ABORT,'synthetic provenance rejection'); END"); err != nil {
				t.Fatal(err)
			}
			var row task.Record
			if err := f.client.JSON(t.Context(), "POST", f.route(), f.finish, &row); err == nil {
				t.Fatal("audit failure became isolated success")
			}
			stored, err := f.c.readStoredTask(t.Context(), taskAddress{Scope: f.address.Scope})
			if err != nil || stored.State != task.Running || len(stored.Assets) != 0 || len(auditEvents(t, f.r, f.address.Scope)) != before {
				t.Fatal("provenance transaction partially committed", err)
			}
			if _, err = f.c.db.ExecContext(t.Context(), "DROP TRIGGER reject_provenance"); err != nil {
				t.Fatal(err)
			}
			if err = f.client.JSON(t.Context(), "POST", f.route(), f.finish, &row); err != nil || row.State != task.Succeeded {
				t.Fatal("same receipt could not retry after audit recovery", err)
			}
		})
	}
}
