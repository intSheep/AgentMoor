package platform

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

// These are independent capabilities, rather than the convenient combinations
// issued by the CLI. In particular Execute must not imply Submit or Write.
var authorizationRoles = []struct {
	name  string
	grant Grant
}{
	{"reader", Grant{Read: true}},
	{"writer", Grant{Write: true}},
	{"submitter", Grant{Read: true, Submit: true}},
	{"executor", Grant{Read: true, Execute: true}},
	{"collector", Grant{Read: true, Collect: true}},
}

type authorizationFixture struct {
	t             *testing.T
	catalog       *Catalog
	handler       http.Handler
	secret        []byte
	scope         asset.Scope
	spec          task.Spec
	session, turn string
	attempt       string
	artifact      asset.Change
	rawChange     asset.Change
	rawTicket     asset.Ticket
	partChange    asset.Change
	partTicket    asset.Ticket
	manifest      asset.TarParts
	part          asset.TarPart
	partBody      []byte
}

func newAuthorizationFixture(t *testing.T, state, turnState string) *authorizationFixture {
	t.Helper()
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	f := &authorizationFixture{t: t, catalog: c, secret: bytes.Repeat([]byte{41}, 32),
		scope: scope("owner"), session: uuid.NewString(), turn: uuid.NewString(), attempt: uuid.NewString(),
		spec: task.Spec{Instruction: "produce a report", Input: "owner input", Output: "report.txt"}}
	f.handler = (&API{Catalog: c, Secret: f.secret, Temp: t.TempDir(),
		TaskAdapters: map[string]bool{f.scope.Adapter: true},
		TurnEnabled:  func(asset.Scope) bool { return true }}).Handler()
	f.artifact = change("owner report", 0)
	f.artifact.SessionID = f.session
	f.publish(f.scope, f.artifact, []byte("owner report"))
	for _, key := range []asset.Key{{Kind: "workspace", ID: "root"}, {Kind: "session", ID: f.session}} {
		ch := change("owner "+key.Kind, 0)
		ch.Key, ch.SessionID = key, f.session
		f.publish(f.scope, ch, []byte("owner "+key.Kind))
	}
	address := taskAddress{Scope: f.scope}
	if _, err = c.submitTask(t.Context(), address, f.spec); err != nil {
		t.Fatal(err)
	}
	if state != task.Queued {
		if _, err = c.claimTask(t.Context(), address, task.Claim{Attempt: f.attempt}); err != nil {
			t.Fatal(err)
		}
	}
	if state == task.Succeeded {
		if _, err = c.finishTask(t.Context(), address, task.Finish{Attempt: f.attempt, SessionID: f.session}); err != nil {
			t.Fatal(err)
		}
		address.turn = f.turn
		if _, err = c.submitTurn(t.Context(), address, f.question()); err != nil {
			t.Fatal(err)
		}
		if turnState == task.Running {
			if _, err = c.claimTask(t.Context(), address, task.Claim{Attempt: f.attempt}); err != nil {
				t.Fatal(err)
			}
			for _, key := range []asset.Key{{Kind: "artifact", ID: "out/" + task.ReplyFile}, {Kind: "workspace", ID: "root"}, {Kind: "session", ID: f.session}} {
				ch := change("owner turn "+key.Kind, 0)
				ch.Key, ch.SessionID = task.TurnKey(f.turn, key), f.session
				f.publish(f.scope, ch, []byte("owner turn "+key.Kind))
			}
		}
	}
	f.rawChange = change("pending upload", 0)
	f.rawChange.ID = "out/pending.txt"
	f.rawTicket, err = c.Begin(t.Context(), f.scope, f.rawChange)
	if err != nil {
		t.Fatal(err)
	}
	var bodies map[string][]byte
	f.partChange, f.manifest, _, bodies = tarFixture(t, 0, tarFile{"only.txt", "isolated part body"})
	f.partChange.ID = "pending-root"
	f.partTicket, err = c.Begin(t.Context(), f.scope, f.partChange)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := c.PrepareParts(t.Context(), f.scope, f.partTicket.ID, f.manifest)
	if err != nil || len(missing) != 1 {
		t.Fatal("part fixture must require one actual upload", missing, err)
	}
	f.part, f.partBody = missing[0], bodies[missing[0].SHA256]
	return f
}

func (f *authorizationFixture) publish(s asset.Scope, ch asset.Change, body []byte) {
	f.t.Helper()
	ticket := upload(f.t, f.catalog, s, ch, string(body))
	if _, err := f.catalog.Commit(f.t.Context(), s, ticket.ID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *authorizationFixture) route() string     { return "/v1/tasks/" + f.scope.Runtime }
func (f *authorizationFixture) turnRoute() string { return f.route() + "/turns/" + f.turn }
func (f *authorizationFixture) question() task.Question {
	return task.Question{SessionID: f.session, Message: "continue the owner report"}
}
func (f *authorizationFixture) rawRoute() string  { return "/v1/changes/" + f.rawTicket.ID }
func (f *authorizationFixture) partRoute() string { return "/v1/changes/" + f.partTicket.ID }

func (f *authorizationFixture) token(s asset.Scope, grant Grant) string {
	f.t.Helper()
	grant.Scope, grant.Expires = s, time.Now().Add(time.Hour).Unix()
	token, err := Issue(f.secret, grant, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	return token
}

func authorizationJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *authorizationFixture) request(method, path, token string, body []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

// Capture the owner's public record, old fixed content and in-flight operation
// metadata. A denied mutation must leave these identical, not merely return 403.
func (f *authorizationFixture) ownerState() map[string]string {
	f.t.Helper()
	token := f.token(f.scope, Grant{Read: true})
	state := make(map[string]string)
	for _, path := range []string{"/v1/assets", "/v1/heads", "/v1/status", f.route(), f.turnRoute(), f.route() + "/checkpoint", f.artifact.Key.ContentPath(1)} {
		w := f.request("GET", path, token, nil)
		if w.Code != http.StatusOK && w.Code != http.StatusNotFound && !(path == f.route()+"/checkpoint" && w.Code == http.StatusConflict) {
			f.t.Fatalf("snapshot %s: %d %s", path, w.Code, w.Body.String())
		}
		state[path] = w.Result().Status + "\n" + w.Header().Get("X-Agentmoor-Revision") + "\n" + w.Body.String()
	}
	return state
}

func (f *authorizationFixture) unchanged(before map[string]string) {
	f.t.Helper()
	if after := f.ownerState(); !reflect.DeepEqual(before, after) {
		f.t.Fatal("denied request changed the owner's records, checkpoint, operations or fixed content")
	}
}

func authorizationAllows(g Grant, permission string) bool {
	switch permission {
	case "":
		return true
	case "read":
		return g.Read
	case "write":
		return g.Write
	case "submit":
		return g.Submit
	case "execute":
		return g.Execute
	case "collect":
		return g.Collect
	default:
		panic("unknown test capability")
	}
}

func TestHTTPAuthorizationCapabilities(t *testing.T) {
	type endpoint struct {
		name, method, permission, state, turnState string
		prepare                                    func(*authorizationFixture) (string, []byte)
		status                                     int
	}
	endpoints := []endpoint{
		{"context", "GET", "", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) { return "/v1/context", nil }, 200},
		{"assets", "GET", "read", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) { return "/v1/assets", nil }, 200},
		{"heads", "GET", "read", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) { return "/v1/heads", nil }, 200},
		{"status", "GET", "read", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) { return "/v1/status", nil }, 200},
		{"content", "GET", "read", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) { return f.artifact.Key.ContentPath(1), nil }, 200},
		{"begin", "POST", "write", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) {
			return "/v1/changes", authorizationJSON(t, f.rawChange)
		}, 200},
		{"upload", "PUT", "write", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) {
			return f.rawRoute() + "/content", []byte("pending upload")
		}, 204},
		{"commit", "POST", "write", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) {
			f.rawTicket = upload(t, f.catalog, f.scope, f.rawChange, "pending upload")
			return f.rawRoute() + "/commit", nil
		}, 200},
		{"manifest", "POST", "write", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) {
			return f.partRoute() + "/parts", authorizationJSON(t, f.manifest)
		}, 200},
		{"part", "PUT", "write", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) {
			return f.partRoute() + "/parts/" + f.part.SHA256, f.partBody
		}, 204},
		{"gc", "POST", "collect", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) { return "/v1/gc", nil }, 200},
		{"submit-task", "PUT", "submit", task.Queued, "", func(f *authorizationFixture) (string, []byte) { return f.route(), authorizationJSON(t, f.spec) }, 200},
		{"read-task", "GET", "read", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) { return f.route(), nil }, 200},
		{"checkpoint", "GET", "read", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) { return f.route() + "/checkpoint", nil }, 200},
		{"cancel-task", "POST", "submit", task.Queued, "", func(f *authorizationFixture) (string, []byte) { return f.route() + "/cancel", nil }, 200},
		{"claim-task", "POST", "execute", task.Queued, "", func(f *authorizationFixture) (string, []byte) {
			return f.route() + "/claim", authorizationJSON(t, task.Claim{Attempt: f.attempt})
		}, 200},
		{"finish-task", "POST", "execute", task.Running, "", func(f *authorizationFixture) (string, []byte) {
			return f.route() + "/finish", authorizationJSON(t, task.Finish{Attempt: f.attempt, SessionID: f.session})
		}, 200},
		{"submit-turn", "PUT", "submit", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) {
			return f.turnRoute(), authorizationJSON(t, f.question())
		}, 200},
		{"read-turn", "GET", "read", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) { return f.turnRoute(), nil }, 200},
		{"cancel-turn", "POST", "submit", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) { return f.turnRoute() + "/cancel", nil }, 200},
		{"claim-turn", "POST", "execute", task.Succeeded, task.Queued, func(f *authorizationFixture) (string, []byte) {
			return f.turnRoute() + "/claim", authorizationJSON(t, task.Claim{Attempt: f.attempt})
		}, 200},
		{"finish-turn", "POST", "execute", task.Succeeded, task.Running, func(f *authorizationFixture) (string, []byte) {
			return f.turnRoute() + "/finish", authorizationJSON(t, task.Finish{Attempt: f.attempt, SessionID: f.session})
		}, 200},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			f := newAuthorizationFixture(t, endpoint.state, endpoint.turnState)
			path, body := endpoint.prepare(f)
			before := f.ownerState()
			// Invalid credentials are rejected even when the route's body and
			// identifiers are valid and the operation exists.
			for _, token := range []string{"", "synthetic.invalid"} {
				w := f.request(endpoint.method, path, token, body)
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("invalid credential: %d %s", w.Code, w.Body.String())
				}
				f.unchanged(before)
			}
			for _, role := range authorizationRoles {
				if authorizationAllows(role.grant, endpoint.permission) {
					continue
				}
				w := f.request(endpoint.method, path, f.token(f.scope, role.grant), body)
				if w.Code != http.StatusForbidden || w.Header().Get("X-Agentmoor-Revision") != "" {
					t.Fatalf("%s must be forbidden: %d %s", role.name, w.Code, w.Body.String())
				}
				f.unchanged(before)
			}
			for _, role := range authorizationRoles {
				if !authorizationAllows(role.grant, endpoint.permission) {
					continue
				}
				w := f.request(endpoint.method, path, f.token(f.scope, role.grant), body)
				if w.Code != endpoint.status {
					t.Fatalf("positive control %s: %d, want %d: %s", role.name, w.Code, endpoint.status, w.Body.String())
				}
				if endpoint.name == "content" && w.Body.String() != "owner report" {
					t.Fatal("positive content control did not read the existing owner object")
				}
			}
		})
	}
}

func TestHTTPAuthorizationSeparatesEveryScopeDimension(t *testing.T) {
	for _, dimension := range []string{"tenant", "project", "runtime", "adapter"} {
		t.Run(dimension, func(t *testing.T) {
			f := newAuthorizationFixture(t, task.Succeeded, task.Queued)
			other := f.scope
			switch dimension {
			case "tenant":
				other.Tenant = "foreign"
			case "project":
				other.Project = "foreign"
			case "runtime":
				other.Runtime = "foreign"
			case "adapter":
				other.Adapter = "foreign"
			}
			all := Grant{Read: true, Write: true, Submit: true, Execute: true, Collect: true}
			token := f.token(other, all)
			context := f.request("GET", "/v1/context", token, nil)
			var grant Grant
			if context.Code != 200 || json.Unmarshal(context.Body.Bytes(), &grant) != nil || grant.Scope != other {
				t.Fatal("foreign credential must be valid and bound to its own scope", context.Code)
			}
			before := f.ownerState()
			for _, path := range []string{"/v1/assets", "/v1/heads", "/v1/status"} {
				w := f.request("GET", path, token, nil)
				if w.Code != 200 || w.Body.String() != "[]\n" {
					t.Fatalf("foreign scope listed owner data: %s %d %s", path, w.Code, w.Body.String())
				}
			}
			for _, request := range []struct {
				method, path string
				body         []byte
			}{
				{"GET", f.artifact.Key.ContentPath(1), nil},
				{"PUT", f.rawRoute() + "/content", []byte("pending upload")},
				{"POST", f.rawRoute() + "/commit", nil},
				{"POST", f.partRoute() + "/parts", authorizationJSON(t, f.manifest)},
				{"PUT", f.partRoute() + "/parts/" + f.part.SHA256, f.partBody},
				{"POST", f.partRoute() + "/commit", nil},
				{"GET", f.route(), nil}, {"GET", f.turnRoute(), nil}, {"GET", f.route() + "/checkpoint", nil},
				{"PUT", f.turnRoute(), authorizationJSON(t, f.question())},
				{"POST", f.route() + "/cancel", nil}, {"POST", f.turnRoute() + "/cancel", nil},
				{"POST", f.route() + "/claim", authorizationJSON(t, task.Claim{Attempt: f.attempt})},
				{"POST", f.turnRoute() + "/claim", authorizationJSON(t, task.Claim{Attempt: f.attempt})},
				{"POST", f.route() + "/finish", authorizationJSON(t, task.Finish{Attempt: f.attempt, SessionID: f.session})},
				{"POST", f.turnRoute() + "/finish", authorizationJSON(t, task.Finish{Attempt: f.attempt, SessionID: f.session})},
			} {
				w := f.request(request.method, request.path, token, request.body)
				if w.Code != 404 || w.Header().Get("X-Agentmoor-Revision") != "" {
					t.Fatalf("foreign %s %s: %d %s", request.method, request.path, w.Code, w.Body.String())
				}
				f.unchanged(before)
			}
			// Equal hashes must not make an owner's staged part visible or
			// usable by another scope. The other scope can publish its own copy.
			ch := f.partChange
			ch.ID = "foreign-root"
			w := f.request("POST", "/v1/changes", token, authorizationJSON(t, ch))
			var ticket asset.Ticket
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ticket) != nil || ticket.ID == f.partTicket.ID {
				t.Fatal("foreign positive Begin control", w.Code, w.Body.String())
			}
			path := "/v1/changes/" + ticket.ID
			w = f.request("POST", path+"/parts", token, authorizationJSON(t, f.manifest))
			var missing struct {
				Missing []asset.TarPart `json:"missing"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &missing) != nil || len(missing.Missing) != 1 || !reflect.DeepEqual(missing.Missing[0], f.part) {
				t.Fatal("equal digest crossed scope or foreign manifest control failed", w.Code, w.Body.String())
			}
			if w = f.request("PUT", path+"/parts/"+f.part.SHA256, token, f.partBody); w.Code != 204 {
				t.Fatal("foreign positive part control", w.Code, w.Body.String())
			}
			if w = f.request("POST", path+"/commit", token, nil); w.Code != 200 {
				t.Fatal("foreign positive commit control", w.Code, w.Body.String())
			}
			if w = f.request("GET", ch.Key.ContentPath(1), token, nil); w.Code != 200 {
				t.Fatal("foreign positive content control", w.Code, w.Body.String())
			}
			header, err := base64.RawURLEncoding.DecodeString(w.Header().Get("X-Agentmoor-Revision"))
			var revision asset.Revision
			sum := sha256.Sum256(w.Body.Bytes())
			if err != nil || json.Unmarshal(header, &revision) != nil || revision.Scope != other || revision.Key != ch.Key || revision.SHA256 != ch.SHA256 || hex.EncodeToString(sum[:]) != ch.SHA256 || int64(w.Body.Len()) != ch.Size {
				t.Fatal("foreign download did not return its own complete verified content")
			}
			f.unchanged(before)
			owner := f.token(f.scope, all)
			w = f.request("POST", f.partRoute()+"/parts", owner, authorizationJSON(t, f.manifest))
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &missing) != nil || len(missing.Missing) != 1 {
				t.Fatal("foreign upload satisfied the owner's pending part", w.Code, w.Body.String())
			}
		})
	}
}
