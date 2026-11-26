package worker

import (
	"bytes"
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"agentmoor/internal/adapter"
	"agentmoor/internal/adapters/workspace"
	"agentmoor/internal/asset"
	"agentmoor/internal/platform"
	"agentmoor/internal/snapshot"
	"agentmoor/internal/task"
)

// This test-only adapter calls a real child process and exports its own tiny
// native format. The independent counter is outside all captured asset roots.
type publicationRuntime struct {
	workspace.Layout
	counter string
}

func (a publicationRuntime) ExecuteFile(ctx context.Context, env task.Environment, spec task.Spec, result, session string, history task.RestoreHistory) (task.Execution, error) {
	dir := filepath.Join(env.Home, "history")
	if history == nil {
		session = uuid.NewString()
		if err := os.Mkdir(dir, 0700); err != nil {
			return task.Execution{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, "session"), []byte(session), 0600); err != nil {
			return task.Execution{}, err
		}
	} else {
		if err := history.Files(dir); err != nil {
			return task.Execution{}, err
		}
		raw, err := os.ReadFile(filepath.Join(dir, "session"))
		if err != nil || string(raw) != session {
			return task.Execution{}, fmt.Errorf("native fixture identity did not restore")
		}
	}
	var text bytes.Buffer
	if err := adapter.RunCLIInput(ctx, env.Executable, env.Workspace, []string{"HOME=" + env.Home}, strings.NewReader(spec.Instruction), &text, 64, a.counter, result); err != nil {
		return task.Execution{}, err
	}
	return task.Execution{SessionID: session, HistoryDir: dir, Text: text.String()}, nil
}

type publicationFault struct {
	point, mode string
	status      int
	limit       int
	mu          sync.Mutex
	target      asset.Key
	work        string
	failures    int
	changes     []asset.Change
	uploads     [][]byte
	commits     int
	statuses    []int
	checkpoint  task.Checkpoint
	checkpoints []task.Checkpoint
}

type publicationHarness struct {
	t       *testing.T
	catalog *platform.Catalog
	api     http.Handler
	server  *httptest.Server
	saas    *platform.Client
	worker  *platform.Client
	scope   asset.Scope
	runtime publicationRuntime
	exe     string
	db      *sql.DB
	fault   *publicationFault
}

func newPublicationHarness(t *testing.T) *publicationHarness {
	t.Helper()
	dir := t.TempDir()
	catalog, err := platform.OpenCatalog(t.Context(), dir, "", platform.DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { catalog.Close() })
	db, err := sql.Open("sqlite3", filepath.Join(dir, "catalog.sqlite")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &publicationHarness{t: t, catalog: catalog, db: db,
		scope: asset.Scope{Tenant: "fixture", Project: "publication", Runtime: "report", Adapter: "publication-fixture"},
		fault: &publicationFault{}}
	h.runtime = publicationRuntime{Layout: workspace.Layout{ID: h.scope.Adapter, Version: "1", TestedRuntime: "publication-fixture 1",
		ReadSessions: func(_ context.Context, root *os.Root) ([]adapter.NativeSession, error) {
			raw, err := root.ReadFile("session")
			if err != nil {
				return nil, err
			}
			return []adapter.NativeSession{{ID: string(raw), Format: "publication-session.v1", Entries: []snapshot.Entry{{Source: "session", Name: "session"}}}}, nil
		}}, counter: filepath.Join(t.TempDir(), "executions")}
	h.exe = filepath.Join(t.TempDir(), "publication-fixture")
	if err := os.WriteFile(h.exe, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then\n  printf '%s\\n' 'publication-fixture 1'\n  exit 0\nfi\nprintf 'executed\\n' >> \"$1\"\n/bin/cat > \"$2\"\nprintf 'done'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{7}, 32)
	h.api = (&platform.API{Catalog: catalog, Secret: secret, Temp: t.TempDir(),
		FileTaskAdapters: map[string]bool{h.scope.Adapter: true}, TurnEnabled: func(s asset.Scope) bool { return s == h.scope }}).Handler()
	h.server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.server.Close)
	client := func(execute bool) *platform.Client {
		tok, err := platform.Issue(secret, platform.Grant{Scope: h.scope, Read: true, Submit: !execute, Execute: execute, Write: execute, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		c, err := platform.NewClient(h.server.URL, tok)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	h.saas, h.worker = client(false), client(true)
	return h
}

func (h *publicationHarness) engine() Engine {
	return Engine{Config: Config{Scope: h.scope, Endpoint: h.server.URL, RuntimeCLI: h.exe,
		Workspace: filepath.Join(h.t.TempDir(), "workspace"), StateDir: h.t.TempDir()}, Adapter: h.runtime, Client: h.worker}
}

func (h *publicationHarness) checkpoint(ctx context.Context) task.Checkpoint {
	h.t.Helper()
	var cp task.Checkpoint
	if err := h.saas.JSON(ctx, "GET", "/v1/tasks/report/checkpoint", nil, &cp); err != nil {
		h.t.Fatal(err)
	}
	return cp
}

func (h *publicationHarness) first() task.Checkpoint {
	h.t.Helper()
	var row task.Record
	if err := h.saas.JSON(h.t.Context(), "PUT", "/v1/tasks/report", task.Spec{Mode: task.FileTurn, Instruction: "first frozen bytes\n", Output: "report.txt"}, &row); err != nil {
		h.t.Fatal(err)
	}
	e := h.engine()
	if err := e.RunTask(h.t.Context(), 0); err != nil {
		h.t.Fatal(err)
	}
	return h.checkpoint(h.t.Context())
}

func (h *publicationHarness) prepare(point, mode string, status, limit int) (Engine, string, task.Checkpoint) {
	h.t.Helper()
	cp := h.first()
	turn := uuid.NewString()
	route := "/v1/tasks/report/turns/" + turn
	var row task.Record
	if err := h.saas.JSON(h.t.Context(), "PUT", route, task.Question{SessionID: cp.SessionID, Message: "revised frozen bytes\n"}, &row); err != nil {
		h.t.Fatal(err)
	}
	e := h.engine()
	h.fault.mu.Lock()
	h.fault.point, h.fault.mode, h.fault.status, h.fault.limit = point, mode, status, limit
	h.fault.target = task.TurnKey(turn, asset.Key{Kind: "artifact", ID: "out/report.txt"})
	h.fault.work, h.fault.checkpoint = e.Config.Workspace, cp
	h.fault.mu.Unlock()
	return e, turn, cp
}

func (h *publicationHarness) serve(w http.ResponseWriter, r *http.Request) {
	f := h.fault
	stage := ""
	var ch asset.Change
	var upload []byte
	if r.Method == "POST" && r.URL.Path == "/v1/changes" {
		raw, err := io.ReadAll(r.Body)
		if err == nil {
			err = json.Unmarshal(raw, &ch)
		}
		if err != nil {
			h.api.ServeHTTP(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		stage = "begin"
	} else if strings.HasPrefix(r.URL.Path, "/v1/changes/") {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/content") {
			stage = "upload"
			upload, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(upload))
		} else if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/commit") {
			stage = "commit"
		}
	}
	f.mu.Lock()
	match := stage == "begin" && ch.Key == f.target
	if stage != "begin" && len(f.changes) > 0 {
		op := asset.Fingerprint(f.changes[0])
		match = r.URL.Path == "/v1/changes/"+op+"/"+map[string]string{"upload": "content", "commit": "commit"}[stage]
	}
	if match {
		switch stage {
		case "begin":
			f.changes = append(f.changes, ch)
		case "upload":
			f.uploads = append(f.uploads, append([]byte(nil), upload...))
		case "commit":
			f.commits++
		}
	}
	selected := match && strings.HasPrefix(f.point, stage+"-") && f.failures < f.limit
	if selected {
		f.failures++
	}
	point, mode, status, work, expected := f.point, f.mode, f.status, f.work, f.checkpoint
	f.mu.Unlock()
	if !selected {
		h.api.ServeHTTP(w, r)
		return
	}
	// Change the live file after its immutable blob has already been captured.
	if err := os.WriteFile(filepath.Join(work, "out", "report.txt"), []byte("changed after freezing\n"), 0600); err != nil {
		h.t.Error(err)
	}
	var current task.Checkpoint
	if err := h.saas.JSON(r.Context(), "GET", "/v1/tasks/report/checkpoint", nil, &current); err != nil {
		h.t.Error(err)
	} else if !reflect.DeepEqual(current, expected) {
		h.t.Error("partial publication advanced the successful checkpoint")
	}
	f.mu.Lock()
	f.checkpoints = append(f.checkpoints, current)
	f.mu.Unlock()
	if mode == "cancel" {
		var row task.Record
		turn := strings.Split(f.target.ID, "/")[1]
		if err := h.saas.JSON(r.Context(), "POST", "/v1/tasks/report/turns/"+turn+"/cancel", nil, &row); err != nil || row.State != task.Cancelling {
			h.t.Errorf("request cancellation: %s %v", row.State, err)
		}
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			h.t.Error("Worker did not interrupt its publication request after cancellation")
		}
		return
	}
	if mode == "deadline" {
		<-r.Context().Done()
		return
	}
	if mode == "database" || strings.HasSuffix(point, "-after") {
		recorder := httptest.NewRecorder()
		h.api.ServeHTTP(recorder, r)
		f.mu.Lock()
		f.statuses = append(f.statuses, recorder.Code)
		f.mu.Unlock()
		if mode == "database" {
			if recorder.Code != http.StatusInternalServerError {
				h.t.Errorf("SQLite trigger did not fail the actual commit: HTTP %d", recorder.Code)
			}
			if _, err := h.db.ExecContext(r.Context(), "DROP TRIGGER reject_publication"); err != nil {
				h.t.Error(err)
			}
			for name, values := range recorder.Header() {
				w.Header()[name] = values
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		if recorder.Code/100 != 2 {
			h.t.Errorf("acknowledgement was dropped for a failed operation: HTTP %d", recorder.Code)
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			h.t.Error(err)
			return
		}
		conn.Close()
		return
	}
	if mode == "format" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("]"))
		return
	}
	http.Error(w, "fixture publication failure", status)
}

func (h *publicationHarness) verify(t *testing.T, e Engine, turn string, cp task.Checkpoint, state string, begins int) task.Record {
	t.Helper()
	var row task.Record
	if err := h.saas.JSON(t.Context(), "GET", "/v1/tasks/report/turns/"+turn, nil, &row); err != nil {
		t.Fatal(err)
	}
	if row.State != state || row.Parent == nil || !reflect.DeepEqual(*row.Parent, cp) {
		t.Fatalf("unexpected completion %+v", row)
	}
	runs, err := os.ReadFile(h.runtime.counter)
	if err != nil || strings.Count(string(runs), "executed\n") != 2 {
		t.Fatalf("native fixture ran more than once per turn: %q %v", runs, err)
	}
	f := h.fault
	f.mu.Lock()
	changes := append([]asset.Change(nil), f.changes...)
	uploads := append([][]byte(nil), f.uploads...)
	checkpoints := append([]task.Checkpoint(nil), f.checkpoints...)
	f.mu.Unlock()
	if len(changes) != begins || len(checkpoints) == 0 {
		t.Fatalf("begin attempts=%d, checkpoints=%d", len(changes), len(checkpoints))
	}
	for _, change := range changes {
		if !reflect.DeepEqual(change, changes[0]) {
			t.Fatal("retry changed metadata, base, or operation identity")
		}
	}
	for _, raw := range uploads {
		if string(raw) != "revised frozen bytes\n" {
			t.Fatalf("retry read changed workspace bytes: %q", raw)
		}
	}
	config := e.Config
	config.StateDir = filepath.Join(config.StateDir, "publish")
	j, err := load(config)
	if err != nil {
		t.Fatal(err)
	}
	key := task.TurnKey(turn, asset.Key{Kind: "artifact", ID: "out/report.txt"})
	if state == task.Succeeded {
		if j.Pending != nil || j.Cursors[asset.Fingerprint(key)].Version != 1 {
			t.Fatal("successful retry did not commit the cursor", j)
		}
		ref, ok := findArtifact(row, key)
		if !ok {
			t.Fatal("successful record has no exact artifact")
		}
		h.verifyContent(t, ref.Key, ref.Version, "revised frozen bytes\n")
	} else {
		if len(row.Assets) != 0 || !reflect.DeepEqual(h.checkpoint(t.Context()), cp) || j.Pending == nil || !reflect.DeepEqual(j.Pending.Change, changes[0]) || j.Cursors[asset.Fingerprint(key)].Version != 0 {
			t.Fatal("failure lost pending state or advanced the successful checkpoint/cursor", row, j)
		}
		blob, err := os.ReadFile(filepath.Join(config.StateDir, j.Pending.Blob))
		if err != nil || string(blob) != "revised frozen bytes\n" {
			t.Fatalf("pending bytes changed: %q %v", blob, err)
		}
	}
	var revisions []asset.Revision
	if err := h.saas.JSON(t.Context(), "GET", "/v1/assets", nil, &revisions); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, revision := range revisions {
		if revision.Key == key {
			count++
			if revision.Version != 1 {
				t.Fatal("retry produced a second version", revision)
			}
		}
	}
	if (state == task.Succeeded && count != 1) || (state != task.Succeeded && count != 0) {
		t.Fatal("unexpected published version count", count)
	}
	var operations []asset.Job
	if err := h.saas.JSON(t.Context(), "GET", "/v1/status", nil, &operations); err != nil {
		t.Fatal(err)
	}
	operationCount := 0
	for _, operation := range operations {
		if operation.Key != key {
			continue
		}
		operationCount++
		wantState := "pending"
		if state == task.Succeeded {
			wantState = "committed"
		}
		if operation.Operation != asset.Fingerprint(changes[0]) || operation.State != wantState {
			t.Fatal("publication retried with a different operation identity", operation)
		}
	}
	if operationCount > 1 || (state == task.Succeeded && operationCount != 1) {
		t.Fatal("retry created another operation", operationCount)
	}
	h.verifyContent(t, asset.Key{Kind: "artifact", ID: "out/report.txt"}, 1, "first frozen bytes\n")
	return row
}

func findArtifact(row task.Record, key asset.Key) (task.Ref, bool) {
	for _, ref := range row.Assets {
		if ref.Key == key {
			return ref, true
		}
	}
	return task.Ref{}, false
}

func (h *publicationHarness) verifyContent(t *testing.T, key asset.Key, version int64, want string) {
	t.Helper()
	_, body, err := h.saas.Content(t.Context(), key, version)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(body)
	body.Close()
	if err != nil || string(raw) != want {
		t.Fatalf("immutable delivery changed: %q %v", raw, err)
	}
}

func TestTaskPublicationRetriesFrozenOperationAcrossHTTPBoundaries(t *testing.T) {
	for _, point := range []string{"begin-before", "begin-after", "upload-before", "upload-after", "commit-before", "commit-after"} {
		t.Run(point, func(t *testing.T) {
			h := newPublicationHarness(t)
			e, turn, cp := h.prepare(point, "", http.StatusServiceUnavailable, 1)
			if err := e.RunTurn(t.Context(), turn); err != nil {
				t.Fatal(err)
			}
			h.verify(t, e, turn, cp, task.Succeeded, 2)
		})
	}
}

func TestTaskPublicationRetriesRealMetadataFailure(t *testing.T) {
	h := newPublicationHarness(t)
	e, turn, cp := h.prepare("commit-before", "database", 0, 1)
	if _, err := h.db.ExecContext(t.Context(), "CREATE TRIGGER reject_publication BEFORE INSERT ON revisions WHEN NEW.kind='artifact' BEGIN SELECT RAISE(ABORT, 'fixture actual metadata failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := e.RunTurn(t.Context(), turn); err != nil {
		t.Fatal(err)
	}
	h.verify(t, e, turn, cp, task.Succeeded, 2)
}

func TestTaskPublicationDoesNotRetryPermanentErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 422, 425, 501} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := newPublicationHarness(t)
			e, turn, cp := h.prepare("begin-before", "", status, 99)
			err := e.RunTurn(t.Context(), turn)
			if err == nil {
				t.Fatal("permanent publication error was swallowed")
			}
			var httpError *platform.HTTPError
			if !errors.As(err, &httpError) || httpError.StatusCode != status || (status == 404 && !errors.Is(err, platform.ErrMissing)) || (status == 409 && !errors.Is(err, platform.ErrConflict)) {
				t.Fatal("HTTP error identity changed", err)
			}
			h.verify(t, e, turn, cp, task.Failed, 1)
		})
	}
	t.Run("JSON format", func(t *testing.T) {
		h := newPublicationHarness(t)
		e, turn, cp := h.prepare("begin-before", "format", 0, 99)
		var syntax *json.SyntaxError
		if err := e.RunTurn(t.Context(), turn); !errors.As(err, &syntax) {
			t.Fatal("format error was hidden", err)
		}
		h.verify(t, e, turn, cp, task.Failed, 1)
	})
}

func TestTaskPublicationExhaustionKeepsFrozenPendingCursor(t *testing.T) {
	h := newPublicationHarness(t)
	e, turn, cp := h.prepare("commit-before", "", http.StatusServiceUnavailable, 99)
	if err := e.RunTurn(t.Context(), turn); err == nil {
		t.Fatal("exhausted publication did not fail")
	}
	h.verify(t, e, turn, cp, task.Failed, 3)
}

func TestTaskPublicationCancellationAndDeadlineInterruptRequests(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			h := newPublicationHarness(t)
			e, turn, cp := h.prepare("begin-before", mode, 0, 99)
			ctx := t.Context()
			if mode == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 2*time.Second)
				defer stop()
			}
			begin := time.Now()
			err := e.RunTurn(ctx, turn)
			if time.Since(begin) > 4*time.Second {
				t.Fatal("publication ignored cancellation/deadline")
			}
			state := task.Cancelled
			if mode == "deadline" {
				state = task.Failed
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("deadline identity was lost", err)
				}
			} else if err != nil {
				t.Fatal("confirmed cancellation returned an error", err)
			}
			h.verify(t, e, turn, cp, state, 1)
		})
	}
}

func TestPublicationRetryClassification(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 504} {
		if !transientPublication(fmt.Errorf("publish: %w", &platform.HTTPError{StatusCode: status, Message: "fixture"})) {
			t.Errorf("transient HTTP %d was not retried", status)
		}
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"lost headers", io.EOF, true},
		{"truncated acknowledgement", fmt.Errorf("decode: %w", io.ErrUnexpectedEOF), true},
		{"reset connection", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"refused connection", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, true},
		{"network timeout", &net.DNSError{IsTimeout: true}, true},
		{"caller cancelled", fmt.Errorf("request: %w", context.Canceled), false},
		{"caller deadline", fmt.Errorf("request: %w", context.DeadlineExceeded), false},
		{"TLS certificate", &url.Error{Op: "Post", URL: "https://fixture.invalid", Err: x509.UnknownAuthorityError{}}, false},
		{"local frozen file missing", &os.PathError{Op: "open", Path: "missing-blob", Err: os.ErrNotExist}, false},
		{"local permission", &os.PathError{Op: "open", Path: "frozen-blob", Err: os.ErrPermission}, false},
		{"JSON syntax", &json.SyntaxError{Offset: 1}, false},
		{"conflict", platform.ErrConflict, false},
		{"missing", platform.ErrMissing, false},
		{"unsupported HTTP", &platform.HTTPError{StatusCode: 501}, false},
		{"unclassified error", errors.New("permanent fixture error"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transientPublication(tc.err); got != tc.want {
				t.Fatalf("retry=%t, want %t for %T", got, tc.want, tc.err)
			}
		})
	}
}
