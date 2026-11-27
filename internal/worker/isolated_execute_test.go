package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentmoor/internal/adapter"
	"agentmoor/internal/asset"
	"agentmoor/internal/platform"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

// This adapter has the assigned Codex ID so the provider's allocation check is
// exercised, but its version strings, sessions and controller are explicitly
// synthetic. Every host-native capability remains a counting failure trap.
type isolatedExecutionAdapter struct{ *sandboxPublicationAdapter }

func (a *isolatedExecutionAdapter) Descriptor() adapter.Descriptor {
	d := a.sandboxPublicationAdapter.Descriptor()
	d.ID = "codex"
	return d
}

type isolatedExecutionControl struct {
	mu                        sync.Mutex
	h                         *isolatedExecutionHarness
	starts, creates, statuses int
	cancels                   int
	hold                      bool
	addresses                 map[string]platform.SandboxAddress
	started                   chan struct{}
	startOnce                 sync.Once
}

func (c *isolatedExecutionControl) Start(ctx context.Context, a platform.SandboxAddress) (platform.SandboxRun, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts++
	run, err := c.h.catalog.SandboxRun(ctx, a)
	if err != nil {
		return run, err
	}
	permit, err := c.h.catalog.FenceSandboxCreate(ctx, a, run.Deadline)
	if err != nil {
		return run, err
	}
	c.addresses[a.TurnID] = a
	if !permit.CreateAllowed {
		return permit.Run, nil
	}
	c.creates++
	if _, err = c.h.catalog.SealSandboxPodTemplate(ctx, a, "synthetic-sandbox", sandboxPublicationDigest([]byte("fixed synthetic provider Pod"))); err != nil {
		return run, err
	}
	uid := uuid.NewString()
	if run, err = c.h.catalog.BindSandboxPod(ctx, a, uid); err != nil {
		return run, err
	}
	c.startOnce.Do(func() { close(c.started) })
	if c.hold {
		return run, nil
	}
	session := c.h.session
	if run.Source.Parent != nil {
		session = run.Source.Parent.SessionID
	}
	workspace := sandboxPublicationWorkspace(c.h.t, "sealed synthetic report "+a.TurnID)
	nativeSession := []byte("opaque original fixture session " + session + " " + a.TurnID)
	result := sandboxPublicationResult(c.h.adapter.Descriptor(), session, workspace, nativeSession)
	packet := sandboxPublicationEncode(c.h.t, result, workspace, nativeSession)
	if _, err = c.h.catalog.SealSandboxOutput(ctx, permit.Capabilities.Output, sandboxPublicationBlob(packet), bytes.NewReader(packet)); err != nil {
		return run, err
	}
	if _, err = c.h.catalog.RecordSandboxTerminal(ctx, a, uid, 0); err != nil {
		return run, err
	}
	return c.h.catalog.ConfirmSandboxDeleted(ctx, a, uid)
}

func (c *isolatedExecutionControl) Reconcile(ctx context.Context, a platform.SandboxAddress) (platform.SandboxRun, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statuses++
	return c.h.catalog.SandboxRun(ctx, a)
}

func (c *isolatedExecutionControl) Cancel(ctx context.Context, a platform.SandboxAddress) (platform.SandboxRun, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancels++
	run, err := c.h.catalog.CancelSandbox(ctx, a)
	if err != nil || run.PodDeleted || !run.CreateStarted {
		return run, err
	}
	// Require a second cleanup round-trip, so finishing immediately after the
	// cancel acknowledgement would leave an observable, undeleted owner.
	if c.cancels == 1 {
		return run, nil
	}
	if _, err = c.h.catalog.RecordSandboxTerminal(ctx, a, run.PodUID, 143); err != nil {
		return run, err
	}
	return c.h.catalog.ConfirmSandboxDeleted(ctx, a, run.PodUID)
}

type isolatedExecutionHarness struct {
	*sandboxPublicationHarness
	adapter        *isolatedExecutionAdapter
	controller     *isolatedExecutionControl
	realAPI        http.Handler
	requests       atomic.Int64
	faultUsed      atomic.Bool
	fault          string
	finishEarly    atomic.Bool
	claimMu        sync.Mutex
	claims         map[string][]task.Claim
	outputReads    atomic.Int64
	finishRequests atomic.Int64
	session        string
}

func newIsolatedExecutionHarness(t *testing.T) *isolatedExecutionHarness {
	t.Helper()
	base := newSandboxPublicationHarness(t)
	base.scope.Adapter = "codex"
	h := &isolatedExecutionHarness{sandboxPublicationHarness: base,
		adapter: &isolatedExecutionAdapter{base.adapter}, claims: map[string][]task.Claim{}, session: uuid.NewString()}
	h.controller = &isolatedExecutionControl{h: h, addresses: map[string]platform.SandboxAddress{}, started: make(chan struct{})}
	h.realAPI = (&platform.API{Catalog: h.catalog, Secret: h.secret, Temp: t.TempDir(),
		TaskAdapters: map[string]bool{"codex": true}, FileTaskAdapters: map[string]bool{"codex": true},
		TurnEnabled: func(s asset.Scope) bool { return s == h.scope }, SandboxControl: h.controller,
		SandboxProfiles: func(s asset.Scope) (platform.SandboxExecutionProfile, bool) {
			d := h.adapter.Descriptor()
			return platform.SandboxExecutionProfile{Fingerprint: base.profile().Fingerprint, AdapterID: d.ID, AdapterVersion: d.Version, RuntimeVersion: d.TestedRuntime}, s == h.scope
		}}).Handler()
	base.api = http.HandlerFunc(h.serveIsolated)
	base.saas = base.client(platform.Grant{Scope: h.scope, Read: true, Submit: true})
	base.worker = base.client(platform.Grant{Scope: h.scope, Read: true, Write: true, Execute: true})
	return h
}

func (h *isolatedExecutionHarness) serveIsolated(w http.ResponseWriter, r *http.Request) {
	h.requests.Add(1)
	if strings.HasSuffix(r.URL.Path, "/claim") {
		raw, err := io.ReadAll(r.Body)
		var claim task.Claim
		if err != nil || json.Unmarshal(raw, &claim) != nil {
			h.t.Error("cannot observe actual claim")
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		h.claimMu.Lock()
		h.claims[r.URL.Path] = append(h.claims[r.URL.Path], claim)
		h.claimMu.Unlock()
	}
	if strings.HasSuffix(r.URL.Path, "/finish") {
		h.finishRequests.Add(1)
		turn := ""
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) == 7 {
			turn = parts[5]
		}
		h.controller.mu.Lock()
		a, exists := h.controller.addresses[turn]
		h.controller.mu.Unlock()
		if exists {
			run, err := h.catalog.SandboxRun(r.Context(), a)
			if err != nil || (run.CreateStarted && !run.PodDeleted) {
				h.finishEarly.Store(true)
			}
		}
	}
	start := r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/sandbox")
	if strings.HasSuffix(r.URL.Path, "/sandbox/cancel") && h.fault == "cleanup-forbidden" {
		http.Error(w, "synthetic cleanup authority denied", http.StatusForbidden)
		return
	}
	if start && h.fault == "before-start-503" && h.faultUsed.CompareAndSwap(false, true) {
		http.Error(w, "synthetic pre-start outage", 503)
		return
	}
	if start && (h.fault == "after-start-503" || h.fault == "after-start-eof" || h.fault == "after-start-truncated") && h.faultUsed.CompareAndSwap(false, true) {
		buffer := httptest.NewRecorder()
		h.realAPI.ServeHTTP(buffer, r)
		if buffer.Code != 200 {
			h.t.Error("fault requires a real successful Start", buffer.Code)
		}
		if h.fault == "after-start-eof" || h.fault == "after-start-truncated" {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				if h.fault == "after-start-truncated" {
					raw := buffer.Body.Bytes()
					fmt.Fprintf(connection, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(raw))
					_, _ = connection.Write(raw[:len(raw)/2])
				}
				connection.Close()
			}
			return
		}
		http.Error(w, "synthetic lost start acknowledgement", 503)
		return
	}
	if start && h.fault == "status-metadata" {
		buffer := httptest.NewRecorder()
		h.realAPI.ServeHTTP(buffer, r)
		var status platform.SandboxTaskStatus
		if json.Unmarshal(buffer.Body.Bytes(), &status) != nil {
			h.t.Error("cannot decode real status for metadata corruption")
		}
		status.Request.RuntimeVersion = "wrong synthetic runtime"
		json.NewEncoder(w).Encode(status)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/sandbox/output") {
		h.outputReads.Add(1)
		if h.fault == "output-body" {
			buffer := httptest.NewRecorder()
			h.realAPI.ServeHTTP(buffer, r)
			for key, values := range buffer.Header() {
				w.Header()[key] = append([]string(nil), values...)
			}
			raw := append([]byte(nil), buffer.Body.Bytes()...)
			if len(raw) > 0 {
				raw[len(raw)-1] ^= 1
			}
			w.WriteHeader(buffer.Code)
			w.Write(raw)
			return
		}
	}
	h.realAPI.ServeHTTP(w, r)
}

func (h *isolatedExecutionHarness) engine() Engine {
	e := h.sandboxPublicationHarness.engine()
	e.Adapter = h.adapter
	// The real SDK transport below deliberately uses the loopback HTTP server.
	// Declaring the deployment origin as HTTPS tests provider validation only;
	// this fixture does not claim TLS or actual gVisor acceptance.
	e.Config.Endpoint = strings.Replace(h.server.URL, "http://", "https://", 1)
	e.Config.RuntimeCLI, e.Config.NativeSessions = "", false
	e.Config.IsolatedProvider = KubernetesGVisorProvider
	return e
}

func (h *isolatedExecutionHarness) submit(turn string) task.Record {
	h.t.Helper()
	var request any = task.Spec{Mode: task.FileTurn, Instruction: "synthetic provider execution", Output: "report.txt"}
	if turn != "" {
		request = task.Question{SessionID: h.session, Message: "continue synthetic provider execution"}
	}
	var record task.Record
	if err := h.saas.JSON(h.t.Context(), "PUT", h.route(turn), request, &record); err != nil {
		h.t.Fatal(err)
	}
	return record
}

func (h *isolatedExecutionHarness) checkpoint() task.Checkpoint {
	h.t.Helper()
	var cp task.Checkpoint
	if err := h.saas.JSON(h.t.Context(), "GET", h.route("")+"/checkpoint", nil, &cp); err != nil {
		h.t.Fatal(err)
	}
	return cp
}

func (h *isolatedExecutionHarness) download(ref task.Ref) []byte {
	h.t.Helper()
	revision, reader, err := h.saas.Content(h.t.Context(), ref.Key, ref.Version)
	if err != nil {
		h.t.Fatal("public fixed-version download failed", err)
	}
	raw, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || revision.SHA256 != ref.SHA256 || sandboxPublicationDigest(raw) != ref.SHA256 {
		h.t.Fatal("public bytes differ from their successful fixed reference", err, closeErr)
	}
	return raw
}

func TestIsolatedProviderRunsTaskAndTurnsThroughActualHTTP(t *testing.T) {
	h := newIsolatedExecutionHarness(t)
	pin := h.pin()
	for iteration := range 3 {
		turn := ""
		if iteration > 0 {
			turn = uuid.NewString()
		}
		h.submit(turn)
		e := h.engine()
		e.Config.SkillPins = []task.SkillPin{pin}
		var err error
		if turn == "" {
			err = e.RunTask(t.Context(), 0)
		} else {
			err = e.RunTurn(t.Context(), turn)
		}
		if err != nil {
			t.Fatal("actual HTTP provider execution failed", iteration, err)
		}
		var record task.Record
		if err = h.saas.JSON(t.Context(), "GET", h.route(turn), nil, &record); err != nil || record.State != task.Succeeded || record.ExecutionPolicy != task.ExecutionUntrustedIsolated || len(record.Assets) != 4 {
			t.Fatal("provider did not finish the real frozen record", record, err)
		}
		if !reflect.DeepEqual(record.Skills, []task.SkillPin{pin}) || h.checkpoint().SessionID != h.session {
			t.Fatal("provider lost the fixed Skill or native session identity")
		}
		if iteration > 0 && (record.Parent == nil || record.Parent.SessionID != h.session) {
			t.Fatal("continuation did not use the frozen parent")
		}
		for _, ref := range record.Assets {
			raw := h.download(ref)
			if ref.Kind == "artifact" && strings.HasSuffix(ref.ID, "/report.txt") && string(raw) != "sealed synthetic report "+turn {
				t.Fatal("completed Worker published different primary report bytes")
			}
		}
		h.claimMu.Lock()
		claims := append([]task.Claim(nil), h.claims[h.route(turn)+"/claim"]...)
		h.claimMu.Unlock()
		if len(claims) != 1 || claims[0].ExecutionPolicy != task.ExecutionUntrustedIsolated {
			t.Fatal("Worker did not explicitly claim isolated execution", claims)
		}
		if err := os.RemoveAll(e.Config.Workspace); err != nil {
			t.Fatal(err)
		}
	}
	if h.controller.creates != 3 || h.outputReads.Load() != 3 || h.adapter.nativeCalls.Load() != 0 || h.finishEarly.Load() {
		t.Fatal("provider duplicated execution, skipped real output or invoked host-native code")
	}
}

func TestIsolatedProviderRecoversStartFailuresWithoutAnotherCreate(t *testing.T) {
	for _, fault := range []string{"before-start-503", "after-start-503", "after-start-eof", "after-start-truncated"} {
		t.Run(fault, func(t *testing.T) {
			h := newIsolatedExecutionHarness(t)
			h.fault = fault
			h.submit("")
			e := h.engine()
			if err := e.RunTask(t.Context(), 0); err != nil {
				t.Fatal("same-claim recovery failed", err)
			}
			if h.controller.creates != 1 || h.controller.starts != 1 || !h.faultUsed.Load() || h.adapter.nativeCalls.Load() != 0 {
				t.Fatal("Start failure allocated another synthetic execution")
			}
			h.claimMu.Lock()
			claims := h.claims[h.route("")+"/claim"]
			h.claimMu.Unlock()
			if len(claims) != 1 || claims[0].ExecutionPolicy != task.ExecutionUntrustedIsolated {
				t.Fatal("recovery changed the private claim")
			}
		})
	}
}

func TestIsolatedProviderRejectsRealStatusOrOutputDriftAndPreservesCheckpoint(t *testing.T) {
	for _, fault := range []string{"status-metadata", "output-body"} {
		t.Run(fault, func(t *testing.T) {
			h := newIsolatedExecutionHarness(t)
			h.submit("")
			first := h.engine()
			if err := first.RunTask(t.Context(), 0); err != nil {
				t.Fatal("positive control failed", err)
			}
			before := h.checkpoint()
			turn := uuid.NewString()
			h.submit(turn)
			h.fault = fault
			e := h.engine()
			if err := e.RunTurn(t.Context(), turn); err == nil {
				t.Fatal("bad actual HTTP response was accepted")
			}
			var failed task.Record
			if err := h.saas.JSON(t.Context(), "GET", h.route(turn), nil, &failed); err != nil || failed.State != task.Failed || len(failed.Assets) != 0 || !reflect.DeepEqual(before, h.checkpoint()) || h.adapter.nativeCalls.Load() != 0 || h.finishEarly.Load() {
				t.Fatal("bad isolated result changed success state or old checkpoint", failed, err)
			}
		})
	}
}

func TestIsolatedProviderCleansCreatedPodBeforeCancelledOrDeadlineFinish(t *testing.T) {
	for _, mode := range []string{"saas-cancel", "caller-deadline"} {
		t.Run(mode, func(t *testing.T) {
			h := newIsolatedExecutionHarness(t)
			h.submit("")
			first := h.engine()
			if err := first.RunTask(t.Context(), 0); err != nil {
				t.Fatal("positive parent control failed", err)
			}
			checkpoint := h.checkpoint()
			oldWorkspace := h.download(checkpoint.Workspace)
			oldSession := h.download(checkpoint.Session)
			turn := uuid.NewString()
			h.submit(turn)
			e := h.engine()
			h.controller.mu.Lock()
			h.controller.hold = true
			h.controller.started = make(chan struct{})
			h.controller.startOnce = sync.Once{}
			h.controller.mu.Unlock()
			ctx := t.Context()
			if mode == "caller-deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
			}
			done := make(chan error, 1)
			go func() { done <- e.RunTurn(ctx, turn) }()
			select {
			case <-h.controller.started:
			case <-time.After(5 * time.Second):
				t.Fatal("synthetic Pod was not created")
			}
			if mode == "saas-cancel" {
				var cancelled task.Record
				if err := h.saas.JSON(t.Context(), "POST", h.route(turn)+"/cancel", nil, &cancelled); err != nil || cancelled.State != task.Cancelling {
					t.Fatal("public cancellation failed", err)
				}
			}
			select {
			case err := <-done:
				if (err == nil) != (mode == "saas-cancel") {
					t.Fatal("unexpected worker outcome", mode, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Worker did not clean up and acknowledge")
			}
			var record task.Record
			want := task.Cancelled
			if mode == "caller-deadline" {
				want = task.Failed
			}
			if err := h.saas.JSON(t.Context(), "GET", h.route(turn), nil, &record); err != nil || record.State != want || len(record.Assets) != 0 || h.controller.cancels < 2 || h.finishEarly.Load() || h.adapter.nativeCalls.Load() != 0 {
				t.Fatal("finish occurred before deletion or published failed output", record, err)
			}
			if !reflect.DeepEqual(checkpoint, h.checkpoint()) || !bytes.Equal(oldWorkspace, h.download(checkpoint.Workspace)) || !bytes.Equal(oldSession, h.download(checkpoint.Session)) {
				t.Fatal("cancelled/deadline turn changed the successful checkpoint or old bytes")
			}
		})
	}
}

func TestIsolatedProviderRefusesFinishWhenCleanupDeletionIsUnconfirmed(t *testing.T) {
	h := newIsolatedExecutionHarness(t)
	h.submit("")
	first := h.engine()
	if err := first.RunTask(t.Context(), 0); err != nil {
		t.Fatal("positive parent control failed", err)
	}
	checkpoint := h.checkpoint()
	oldWorkspace, oldSession := h.download(checkpoint.Workspace), h.download(checkpoint.Session)
	turn := uuid.NewString()
	h.submit(turn)
	h.controller.mu.Lock()
	h.controller.hold = true
	h.controller.started = make(chan struct{})
	h.controller.startOnce = sync.Once{}
	h.controller.mu.Unlock()
	h.fault = "cleanup-forbidden"
	beforeFinish := h.finishRequests.Load()
	e := h.engine()
	done := make(chan error, 1)
	go func() { done <- e.RunTurn(t.Context(), turn) }()
	select {
	case <-h.controller.started:
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic execution did not start")
	}
	var cancelled task.Record
	if err := h.saas.JSON(t.Context(), "POST", h.route(turn)+"/cancel", nil, &cancelled); err != nil || cancelled.State != task.Cancelling {
		t.Fatal("public cancellation did not enter cancelling", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cleanup rejection was hidden by a cancelled acknowledgement")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Worker did not report rejected cleanup")
	}
	var current task.Record
	if err := h.saas.JSON(t.Context(), "GET", h.route(turn), nil, &current); err != nil || current.State != task.Cancelling || len(current.Assets) != 0 || h.finishRequests.Load() != beforeFinish {
		t.Fatal("Worker finished cancellation without confirmed deletion", current, err)
	}
	h.controller.mu.Lock()
	address := h.controller.addresses[turn]
	h.controller.mu.Unlock()
	status, err := h.worker.SandboxTask(t.Context(), h.route(turn)+"/sandbox", "/status", address.Attempt)
	if err != nil || !status.Run.CreateStarted || status.Run.PodDeleted || h.adapter.nativeCalls.Load() != 0 {
		t.Fatal("negative control did not retain a live unconfirmed owner", err)
	}
	if !reflect.DeepEqual(checkpoint, h.checkpoint()) || !bytes.Equal(oldWorkspace, h.download(checkpoint.Workspace)) || !bytes.Equal(oldSession, h.download(checkpoint.Session)) {
		t.Fatal("unconfirmed cleanup changed the successful checkpoint or bytes")
	}
}

func TestIsolatedProviderConfigurationFailsBeforeAuthorization(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*Config)
	}{
		{"missing-provider", func(c *Config) { c.IsolatedProvider = "" }},
		{"unknown-provider", func(c *Config) { c.IsolatedProvider = "unknown.v1" }},
		{"native-cli", func(c *Config) { c.RuntimeCLI = filepath.Join(filepath.Dir(c.Workspace), "forbidden-native") }},
		{"adapter-version", func(c *Config) { c.AdapterVersion = "wrong" }},
		{"runtime-version", func(c *Config) { c.RuntimeVersion = "wrong" }},
		{"plaintext-origin", func(c *Config) { c.Endpoint = "http://127.0.0.1:7401" }},
	} {
		for _, continuation := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/turn=%v", change.name, continuation), func(t *testing.T) {
				h := newIsolatedExecutionHarness(t)
				turn := ""
				if continuation {
					h.submit("")
					positive := h.engine()
					if err := positive.RunTask(t.Context(), 0); err != nil {
						t.Fatal("positive parent control failed", err)
					}
					turn = uuid.NewString()
				}
				accepted := h.submit(turn)
				e := h.engine()
				change.apply(&e.Config)
				before := h.requests.Load()
				var err error
				if continuation {
					err = e.RunTurn(t.Context(), turn)
				} else {
					err = e.RunTask(t.Context(), 0)
				}
				if err == nil || h.requests.Load() != before || h.adapter.nativeCalls.Load() != 0 {
					t.Fatal("invalid provider contacted authorization or host-native code", err)
				}
				if _, err := os.Stat(e.Config.Workspace); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("invalid provider created a workspace")
				}
				var after task.Record
				if err := h.saas.JSON(t.Context(), "GET", h.route(turn), nil, &after); err != nil || !reflect.DeepEqual(accepted, after) {
					t.Fatal("invalid provider changed the accepted queued record", err)
				}
			})
		}
	}
}
