package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

// This provider is explicitly a metadata fixture: no Pod or native CLI runs.
type handoffControl struct {
	c             *Catalog
	starts        int
	reconciles    int
	cancellations int
	permit        SandboxCreatePermit
}

func (p *handoffControl) Start(ctx context.Context, a SandboxAddress) (SandboxRun, error) {
	p.starts++
	r, err := p.c.SandboxRun(ctx, a)
	if err != nil {
		return r, err
	}
	permit, err := p.c.FenceSandboxCreate(ctx, a, r.Deadline)
	if permit.CreateAllowed {
		p.permit = permit
	}
	return permit.Run, err
}
func (p *handoffControl) Reconcile(ctx context.Context, a SandboxAddress) (SandboxRun, error) {
	p.reconciles++
	return p.c.SandboxRun(ctx, a)
}
func (p *handoffControl) Cancel(ctx context.Context, a SandboxAddress) (SandboxRun, error) {
	p.cancellations++
	return p.c.CancelSandbox(ctx, a)
}

type handoffHTTPFixture struct {
	c       *Catalog
	scope   asset.Scope
	address SandboxAddress
	control *handoffControl
	api     *API
	secret  []byte
}

func newHandoffHTTPFixture(t *testing.T, turn bool) handoffHTTPFixture {
	t.Helper()
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	s := scope("http-handoff")
	s.Adapter = "handoff-fixture"
	a := handoffClaim(t, c, s, nil)
	if turn {
		session := uuid.NewString()
		handoffPublish(t, c, s, asset.Key{Kind: "workspace", ID: "root"}, handoffTree(t, "input.txt", "saved input"), "workspace-tar.v1", "application/x-tar", "", 0)
		handoffPublish(t, c, s, asset.Key{Kind: "session", ID: session}, handoffTree(t, "native.session", "saved synthetic history"), "fixture-session.v1", "application/x-tar", "", 0)
		handoffPublish(t, c, s, asset.Key{Kind: "artifact", ID: "out/report.txt"}, []byte("saved report"), "file.v1", "text/plain", session, 0)
		handoffReadyForFinish(t, c, a, handoffProfile(s), nil)
		if _, err = c.finishTask(handoffFinishContext(t.Context(), s), taskAddress{Scope: s}, task.Finish{Attempt: a.Attempt, SessionID: session}); err != nil {
			t.Fatal(err)
		}
		a.TurnID, a.Attempt = uuid.NewString(), uuid.NewString()
		if _, err = c.submitTurn(t.Context(), taskAddress{s, a.TurnID}, task.Question{SessionID: session, Message: "continue synthetic fixture"}); err != nil {
			t.Fatal(err)
		}
		if _, err = c.claimTask(t.Context(), taskAddress{s, a.TurnID}, task.Claim{Attempt: a.Attempt, ExecutionPolicy: task.ExecutionUntrustedIsolated}); err != nil {
			t.Fatal(err)
		}
	}
	p := &handoffControl{c: c}
	f := handoffHTTPFixture{c: c, scope: s, address: a, control: p, secret: bytes.Repeat([]byte{47}, 32)}
	f.api = &API{Catalog: c, Secret: f.secret, Temp: t.TempDir(), SandboxControl: p,
		TaskAdapters: map[string]bool{s.Adapter: true}, TurnEnabled: func(asset.Scope) bool { return true },
		SandboxProfiles: func(requested asset.Scope) (SandboxExecutionProfile, bool) { return handoffProfile(s), requested == s }}
	return f
}

func (f handoffHTTPFixture) route() string {
	path := "/v1/tasks/" + f.scope.Runtime
	if f.address.TurnID != "" {
		path += "/turns/" + f.address.TurnID
	}
	return path + "/sandbox"
}

func (f handoffHTTPFixture) request(t *testing.T, grant Grant, method, path, raw string) *httptest.ResponseRecorder {
	t.Helper()
	grant.Expires = time.Now().Add(time.Hour).Unix()
	token, err := Issue(f.secret, grant, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.api.Handler().ServeHTTP(w, r)
	return w
}

func (f handoffHTTPFixture) body() string { return `{"attempt":"` + f.address.Attempt + `"}` }

func (f handoffHTTPFixture) prepare(t *testing.T) {
	t.Helper()
	w := f.request(t, Grant{Scope: f.scope, Read: true, Execute: true}, "POST", f.route(), f.body())
	if w.Code != 200 {
		t.Fatal("prepare fixture failed", w.Code, w.Body.String())
	}
	uid := uuid.NewString()
	if _, err := f.c.BindSandboxPod(t.Context(), f.address, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.SealSandboxOutput(t.Context(), f.control.permit.Capabilities.Output, sandboxTestBlob("synthetic sealed result"), strings.NewReader("synthetic sealed result")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.RecordSandboxTerminal(t.Context(), f.address, uid, 0); err != nil {
		t.Fatal(err)
	}
	sandboxTestGuestStop(t, f.c, f.address, uid, 0)
	if _, err := f.c.ConfirmSandboxDeleted(t.Context(), f.address, uid); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxHTTPRoleAndFourDimensionScopeMatrix(t *testing.T) {
	for _, turn := range []bool{false, true} {
		for _, operation := range []string{"", "/status", "/output", "/cancel"} {
			for _, role := range authorizationRoles {
				t.Run(fmt.Sprintf("turn=%v%s/%s", turn, operation, role.name), func(t *testing.T) {
					f := newHandoffHTTPFixture(t, turn)
					if operation != "" {
						f.prepare(t)
					}
					before, _ := f.c.readTask(t.Context(), taskAddress{f.scope, f.address.TurnID})
					calls := f.control.starts + f.control.reconciles + f.control.cancellations
					grant := role.grant
					grant.Scope = f.scope
					w := f.request(t, grant, "POST", f.route()+operation, f.body())
					want := 403
					if role.grant.Execute {
						want = 200
					}
					if w.Code != want {
						t.Fatal("role boundary", w.Code, want, w.Body.String())
					}
					if !role.grant.Execute {
						after, _ := f.c.readTask(t.Context(), taskAddress{f.scope, f.address.TurnID})
						if !reflect.DeepEqual(before, after) || calls != f.control.starts+f.control.reconciles+f.control.cancellations {
							t.Fatal("denied role reached provider or changed task")
						}
					}
				})
			}
			for _, dimension := range []string{"tenant", "project", "runtime", "adapter"} {
				t.Run(fmt.Sprintf("turn=%v%s/other-%s", turn, operation, dimension), func(t *testing.T) {
					f := newHandoffHTTPFixture(t, turn)
					if operation != "" {
						f.prepare(t)
					}
					other := f.scope
					switch dimension {
					case "tenant":
						other.Tenant = "other-owner"
					case "project":
						other.Project = "other-project"
					case "runtime":
						other.Runtime = "other-runtime"
					case "adapter":
						other.Adapter = "other-adapter"
					}
					calls := f.control.starts + f.control.reconciles + f.control.cancellations
					w := f.request(t, Grant{Scope: other, Read: true, Execute: true}, "POST", f.route()+operation, f.body())
					if w.Code != 404 || calls != f.control.starts+f.control.reconciles+f.control.cancellations {
						t.Fatal("cross-scope operation reached provider", w.Code)
					}
				})
			}
		}
	}
}

func TestSandboxHTTPRejectsBodyOverridesAndUnownedClaim(t *testing.T) {
	for _, raw := range []string{`{"Attempt":"x"}`, `{"attempt":null}`, `{"attempt":"%s","attempt":"%s"}`, `{"attempt":"%s","spec":{"instruction":"override"}}`, `{"attempt":"%s"} {}`, `{"attempt":"bad-id"}`} {
		f := newHandoffHTTPFixture(t, false)
		raw = strings.ReplaceAll(raw, "%s", f.address.Attempt)
		w := f.request(t, Grant{Scope: f.scope, Read: true, Execute: true}, "POST", f.route(), raw)
		if w.Code != 400 || f.control.starts != 0 {
			t.Fatal("noncanonical task override accepted", w.Code)
		}
	}
	f := newHandoffHTTPFixture(t, false)
	w := f.request(t, Grant{Scope: f.scope, Read: true, Execute: true}, "POST", f.route(), `{"attempt":"`+uuid.NewString()+`"}`)
	if w.Code != 409 || f.control.starts != 0 {
		t.Fatal("unknown claim reached provider", w.Code)
	}
	f.api.SandboxControl = nil
	w = f.request(t, Grant{Scope: f.scope, Read: true, Execute: true}, "POST", f.route(), f.body())
	if w.Code != 422 {
		t.Fatal("unavailable provider accepted", w.Code)
	}
}

func TestSandboxHTTPOutputIntegrityAndCancellationAfterTaskStops(t *testing.T) {
	f := newHandoffHTTPFixture(t, false)
	f.prepare(t)
	w := f.request(t, Grant{Scope: f.scope, Read: true, Execute: true}, "POST", f.route()+"/output", f.body())
	if w.Code != 200 || w.Body.String() != "synthetic sealed result" || w.Header().Get("X-Agentmoor-SHA256") != sandboxTestBlob(w.Body.String()).SHA256 {
		t.Fatal("sealed result differs", w.Code)
	}
	if strings.Contains(w.Body.String(), f.address.Attempt) {
		t.Fatal("private claim appeared in output")
	}
	if _, err := f.c.cancelTask(t.Context(), taskAddress{Scope: f.scope}); err != nil {
		t.Fatal(err)
	}
	w = f.request(t, Grant{Scope: f.scope, Read: true, Execute: true}, "POST", f.route()+"/cancel", f.body())
	if w.Code != 200 || f.control.cancellations != 1 {
		t.Fatal("stopping task cannot clean its sandbox", w.Code)
	}
	w = f.request(t, Grant{Scope: f.scope, Read: true, Execute: true}, "POST", f.route()+"/output", f.body())
	if w.Code != 409 {
		t.Fatal("cancelled claim could still read output", w.Code)
	}
}

func TestSandboxClaimCannotSwitchBetweenLocalAndIsolatedExecution(t *testing.T) {
	f := newHandoffHTTPFixture(t, false)
	// Add another queued allocation to exercise the public claim decision.
	f.scope.Runtime = "queued-allocation"
	f.address = SandboxAddress{Scope: f.scope, Attempt: uuid.NewString()}
	f.api.SandboxProfiles = func(s asset.Scope) (SandboxExecutionProfile, bool) { return handoffProfile(s), s == f.scope }
	if _, err := f.c.submitTask(t.Context(), taskAddress{Scope: f.scope}, task.Spec{Instruction: "policy binding", Output: "report.txt"}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/tasks/" + f.scope.Runtime + "/claim"
	grant := Grant{Scope: f.scope, Read: true, Execute: true}
	for _, policy := range []string{"", task.ExecutionTrustedLocal, "unknown-policy"} {
		raw, _ := json.Marshal(task.Claim{Attempt: f.address.Attempt, ExecutionPolicy: policy})
		w := f.request(t, grant, "POST", path, string(raw))
		row, err := f.c.readTask(t.Context(), taskAddress{Scope: f.scope})
		if w.Code != 422 || err != nil || row.State != task.Queued || row.attempt != "" {
			t.Fatal("isolated allocation accepted a local/unknown claim", w.Code, err)
		}
	}
	selected := f.api.SandboxProfiles
	f.api.SandboxProfiles = func(asset.Scope) (SandboxExecutionProfile, bool) { return SandboxExecutionProfile{}, false }
	for _, policy := range []string{"", task.ExecutionTrustedLocal, task.ExecutionUntrustedIsolated} {
		raw, _ := json.Marshal(task.Claim{Attempt: f.address.Attempt, ExecutionPolicy: policy})
		w := f.request(t, grant, "POST", path, string(raw))
		row, _ := f.c.readTask(t.Context(), taskAddress{Scope: f.scope})
		if w.Code != 422 || row.State != task.Queued || row.attempt != "" {
			t.Fatal("missing isolated profile silently enabled execution", policy, w.Code)
		}
	}
	f.api.SandboxProfiles = selected
	raw, _ := json.Marshal(task.Claim{Attempt: f.address.Attempt, ExecutionPolicy: task.ExecutionUntrustedIsolated})
	w := f.request(t, grant, "POST", path, string(raw))
	if w.Code != 200 {
		t.Fatal("configured isolated claim rejected", w.Code, w.Body.String())
	}
	before, _ := f.c.readTask(t.Context(), taskAddress{Scope: f.scope})
	if _, err := f.c.claimTask(t.Context(), taskAddress{Scope: f.scope}, task.Claim{Attempt: f.address.Attempt, ExecutionPolicy: task.ExecutionTrustedLocal}); !errors.Is(err, ErrConflict) {
		t.Fatal("same private claim changed execution provider", err)
	}
	// Removing a profile cannot turn an already-owned isolated claim local.
	f.api.SandboxProfiles, f.api.SandboxControl = nil, nil
	w = f.request(t, grant, "POST", path, f.body())
	after, _ := f.c.readTask(t.Context(), taskAddress{Scope: f.scope})
	if w.Code != 409 || !reflect.DeepEqual(before.Record, after.Record) {
		t.Fatal("profile change revived local execution", w.Code)
	}
	// An existing legacy local claim likewise cannot be converted in flight.
	legacy := sandboxTestClaim(t, f.c, scope("legacy-local"))
	if _, err := f.c.claimTask(t.Context(), taskAddress{Scope: legacy.Scope}, task.Claim{Attempt: legacy.Attempt, ExecutionPolicy: task.ExecutionUntrustedIsolated}); !errors.Is(err, ErrConflict) {
		t.Fatal("local claim could also execute in sandbox", err)
	}
}
