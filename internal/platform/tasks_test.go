package platform

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
)

func TestTaskHTTPContract(t *testing.T) {
	catalog, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	secret := bytes.Repeat([]byte{8}, 32)
	api := (&API{Catalog: catalog, Secret: secret, Temp: t.TempDir(), TaskAdapters: map[string]bool{"claude-code": true}, TurnEnabled: func(s asset.Scope) bool { return s.Adapter == "claude-code" }}).Handler()
	scope := asset.Scope{Tenant: "company", Project: "reports", Runtime: "task-1", Adapter: "claude-code"}
	token := func(s asset.Scope, submit, execute bool) string {
		t.Helper()
		tok, err := Issue(secret, Grant{Scope: s, Read: true, Write: execute, Submit: submit, Execute: execute, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	submit, execute, read := token(scope, true, false), token(scope, false, true), token(scope, false, false)
	route := "/v1/tasks/task-1"
	req := func(method, path, tok string, in any, want int) task.Record {
		t.Helper()
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s = %d, wanted %d: %s", method, path, w.Code, want, w.Body.String())
		}
		var v task.Record
		if want == 200 {
			if err = json.Unmarshal(w.Body.Bytes(), &v); err != nil {
				t.Fatal(err)
			}
		}
		return v
	}
	spec := task.Spec{Instruction: "summarize", Input: "sales", Output: "report.json"}
	req("PUT", route, read, spec, 403)
	req("PUT", route, execute, spec, 403)
	other := scope
	other.Adapter = "unimplemented"
	req("PUT", route, token(other, true, false), spec, 422)
	req("PUT", route, submit, task.Spec{Instruction: "x", Output: "../escape"}, 400)
	req("PUT", route, submit, spec, 200)
	req("PUT", route, submit, spec, 200)
	changed := spec
	changed.Input += "x"
	req("PUT", route, submit, changed, 409)
	other = scope
	other.Tenant = "other"
	req("GET", route, token(other, false, false), nil, 404)
	req("GET", "/v1/tasks/other", submit, nil, 404)
	claim := task.Claim{Attempt: uuid.NewString()}
	req("POST", route+"/claim", submit, claim, 403)
	req("POST", route+"/claim", execute, claim, 200)
	req("POST", route+"/claim", execute, claim, 200)
	req("POST", route+"/claim", execute, task.Claim{Attempt: uuid.NewString()}, 409)
	session := uuid.NewString()
	finish := task.Finish{Attempt: claim.Attempt, SessionID: session}
	req("POST", route+"/finish", execute, finish, 425)
	req("POST", route+"/finish", execute, task.Finish{Attempt: uuid.NewString(), Error: "stale"}, 409)
	for _, k := range []asset.Key{{Kind: "artifact", ID: "out/report.json"}, {Kind: "workspace", ID: "root"}, {Kind: "session", ID: session}} {
		ch := change("first", 0)
		ch.Key = k
		ch.SessionID = session
		ticket := upload(t, catalog, scope, ch, "first")
		if _, err := catalog.Commit(t.Context(), scope, ticket.ID); err != nil {
			t.Fatal(err)
		}
	}
	got := req("POST", route+"/finish", execute, finish, 200)
	if got.State != task.Succeeded || len(got.Assets) != 3 {
		t.Fatal(got)
	}
	newer := change("new version", 1)
	newer.Key = got.Assets[0].Key
	newer.SessionID = session
	ticket := upload(t, catalog, scope, newer, "new version")
	if _, err := catalog.Commit(t.Context(), scope, ticket.ID); err != nil {
		t.Fatal(err)
	}
	for _, record := range []task.Record{req("POST", route+"/finish", execute, finish, 200), req("GET", route, read, nil, 200)} {
		if record.Assets[0] != got.Assets[0] || record.Assets[0].Version != 1 {
			t.Fatal("published receipt changed", record)
		}
	}
	req("POST", route+"/cancel", submit, nil, 409)
	turn := route + "/turns/" + uuid.NewString()
	q := task.Question{SessionID: session, Message: "Why?"}
	req("PUT", turn, read, q, 403)
	req("PUT", turn, execute, q, 403)
	req("PUT", turn, submit, map[string]any{"session_id": session, "message": "Why?", "image": "custom"}, 400)
	req("PUT", route+"/turns/bad", submit, q, 400)
	wrong := scope
	wrong.Adapter = "unknown"
	req("PUT", turn, token(wrong, true, false), q, 422)
	optimistic := req("PUT", turn, submit, q, 200)
	if optimistic.Parent == nil || optimistic.Parent.SessionID != session {
		t.Fatal(optimistic)
	}
	req("GET", turn, token(other, false, false), nil, 404)
	req("POST", turn+"/claim", submit, claim, 403)
	req("POST", turn+"/cancel", read, nil, 403)
	req("POST", turn+"/cancel", submit, nil, 200)
}

func TestTaskCancellationAndCrash(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	spec := task.Spec{Instruction: "go", Output: "result"}
	for _, scenario := range []string{"queued", "running", "crash", "failure"} {
		t.Run(scenario, func(t *testing.T) {
			s := asset.Scope{Tenant: "t", Project: "p", Runtime: scenario, Adapter: "agent"}
			if _, err := c.submitTask(t.Context(), taskAddress{Scope: s}, spec); err != nil {
				t.Fatal(err)
			}
			if scenario == "queued" {
				got, err := c.cancelTask(t.Context(), taskAddress{Scope: s})
				if err != nil || got.State != task.Cancelled {
					t.Fatal(got, err)
				}
				return
			}
			var wg sync.WaitGroup
			winners := make(chan string, 2)
			for range 2 {
				wg.Go(func() {
					id := uuid.NewString()
					if _, err := c.claimTask(t.Context(), taskAddress{Scope: s}, task.Claim{Attempt: id}); err == nil {
						winners <- id
					}
				})
			}
			wg.Wait()
			close(winners)
			if len(winners) != 1 {
				t.Fatal("expected exactly one executor")
			}
			id := <-winners
			if scenario == "crash" {
				// Simulate the time after a claimed Pod disappeared without an acknowledgement.
				_, err := c.changeTask(t.Context(), taskAddress{Scope: s}, false, func(slot *taskSlot) error { slot.Deadline = time.Now().Add(-time.Second); return nil })
				if err != nil {
					t.Fatal(err)
				}
				got, err := c.readTask(t.Context(), taskAddress{Scope: s})
				if err != nil || got.State != task.Interrupted {
					t.Fatal(got, err)
				}
				if _, err := c.claimTask(t.Context(), taskAddress{Scope: s}, task.Claim{Attempt: uuid.NewString()}); err != ErrConflict {
					t.Fatal("replayed expired execution", err)
				}
				return
			}
			expected := task.Failed
			if scenario == "running" {
				pending, err := c.cancelTask(t.Context(), taskAddress{Scope: s})
				if err != nil || pending.State != task.Cancelling {
					t.Fatal(pending, err)
				}
				expected = task.Cancelled
			}
			got, err := c.finishTask(t.Context(), taskAddress{Scope: s}, task.Finish{Attempt: id, Error: "stopped"})
			if err != nil || got.State != expected || len(got.Assets) > 0 {
				t.Fatal(got, err)
			}
		})
	}
}
