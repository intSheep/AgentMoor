package worker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"agentmoor/internal/nativeguest"
	"agentmoor/internal/platform"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

const sandboxPublicationSkill = ".agents/skills/native/SKILL.md"

// This is an explicitly synthetic adapter. Publication is permitted to use
// only its static descriptor and pure workspace Skill discovery.
type sandboxPublicationAdapter struct{ nativeCalls atomic.Int64 }

func (*sandboxPublicationAdapter) Descriptor() adapter.Descriptor {
	return adapter.Descriptor{ID: "sandbox-publication-fixture", Version: "fixture.1", TestedRuntime: "synthetic publication fixture 1", TaskModes: []string{task.TextTurn, task.FileTurn}}
}
func (a *sandboxPublicationAdapter) forbidden() error {
	a.nativeCalls.Add(1)
	return errors.New("trusted publication called a forbidden native adapter capability")
}
func (a *sandboxPublicationAdapter) Detect(context.Context, *os.Root) ([]string, error) {
	return nil, a.forbidden()
}
func (a *sandboxPublicationAdapter) Sessions(context.Context, *os.Root) ([]adapter.NativeSession, error) {
	return nil, a.forbidden()
}
func (a *sandboxPublicationAdapter) ExportNative(context.Context, string, string, string) error {
	return a.forbidden()
}
func (a *sandboxPublicationAdapter) ImportNative(context.Context, string, string, string, string) error {
	return a.forbidden()
}
func (a *sandboxPublicationAdapter) SkillDirectory(task.Environment, string) (string, error) {
	return "", a.forbidden()
}
func (a *sandboxPublicationAdapter) ExecuteTask(context.Context, task.Environment, task.Spec) (task.Execution, error) {
	return task.Execution{}, a.forbidden()
}
func (a *sandboxPublicationAdapter) ContinueTask(context.Context, task.Environment, string, string, task.RestoreHistory) (task.Execution, error) {
	return task.Execution{}, a.forbidden()
}
func (a *sandboxPublicationAdapter) ExecuteFile(context.Context, task.Environment, task.Spec, string, string, task.RestoreHistory) (task.Execution, error) {
	return task.Execution{}, a.forbidden()
}
func (*sandboxPublicationAdapter) DiscoverSkills(ctx context.Context, root *os.Root) ([]adapter.SkillRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := root.Lstat(sandboxPublicationSkill)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("invalid synthetic workspace Skill")
	}
	return []adapter.SkillRef{{Manifest: sandboxPublicationSkill}}, nil
}

type sandboxPublicationControl struct{ catalog *platform.Catalog }

func (c sandboxPublicationControl) Start(ctx context.Context, a platform.SandboxAddress) (platform.SandboxRun, error) {
	return c.catalog.SandboxRun(ctx, a)
}
func (c sandboxPublicationControl) Reconcile(ctx context.Context, a platform.SandboxAddress) (platform.SandboxRun, error) {
	return c.catalog.SandboxRun(ctx, a)
}
func (c sandboxPublicationControl) Cancel(ctx context.Context, a platform.SandboxAddress) (platform.SandboxRun, error) {
	return c.catalog.CancelSandbox(ctx, a)
}

type sandboxPublicationHarness struct {
	t        *testing.T
	catalog  *platform.Catalog
	api      http.Handler // Fault tests may wrap this before starting publication.
	server   *httptest.Server
	saas     *platform.Client
	worker   *platform.Client
	scope    asset.Scope
	adapter  *sandboxPublicationAdapter
	secret   []byte
	attempts map[string]string
	mu       sync.Mutex
	changes  []asset.Change
	uploads  map[string][]byte
}

func newSandboxPublicationHarness(t *testing.T) *sandboxPublicationHarness {
	t.Helper()
	catalog, err := platform.OpenCatalog(t.Context(), t.TempDir(), "", platform.DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { catalog.Close() })
	h := &sandboxPublicationHarness{t: t, catalog: catalog, adapter: &sandboxPublicationAdapter{}, secret: bytes.Repeat([]byte{73}, 32), attempts: map[string]string{}, uploads: map[string][]byte{}}
	h.scope = asset.Scope{Tenant: "synthetic", Project: "isolated-publication", Runtime: "allocation", Adapter: h.adapter.Descriptor().ID}
	h.api = (&platform.API{Catalog: catalog, Secret: h.secret, Temp: t.TempDir(),
		TaskAdapters: map[string]bool{h.scope.Adapter: true}, FileTaskAdapters: map[string]bool{h.scope.Adapter: true},
		TurnEnabled: func(s asset.Scope) bool { return s == h.scope }, SandboxControl: sandboxPublicationControl{catalog},
		SandboxProfiles: func(s asset.Scope) (platform.SandboxExecutionProfile, bool) { return h.profile(), s == h.scope }}).Handler()
	h.server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.server.Close)
	h.saas = h.client(platform.Grant{Scope: h.scope, Read: true, Submit: true})
	h.worker = h.client(platform.Grant{Scope: h.scope, Read: true, Write: true, Execute: true})
	return h
}

func (h *sandboxPublicationHarness) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/v1/changes" {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			h.t.Error(err)
			http.Error(w, "synthetic capture failed", 500)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var change asset.Change
		if err := json.Unmarshal(raw, &change); err != nil {
			h.t.Error(err)
		}
		h.mu.Lock()
		h.changes = append(h.changes, change)
		h.mu.Unlock()
	}
	if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/changes/") && strings.HasSuffix(r.URL.Path, "/content") {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			h.t.Error(err)
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		h.mu.Lock()
		h.uploads[r.URL.Path] = append([]byte(nil), raw...)
		h.mu.Unlock()
	}
	h.api.ServeHTTP(w, r)
}

func (h *sandboxPublicationHarness) client(grant platform.Grant) *platform.Client {
	h.t.Helper()
	grant.Expires = time.Now().Add(time.Hour).Unix()
	token, err := platform.Issue(h.secret, grant, time.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	client, err := platform.NewClient(h.server.URL, token)
	if err != nil {
		h.t.Fatal(err)
	}
	return client
}
func (h *sandboxPublicationHarness) profile() platform.SandboxExecutionProfile {
	d := h.adapter.Descriptor()
	return platform.SandboxExecutionProfile{Fingerprint: sandboxPublicationDigest([]byte("fixed synthetic image and runtime")), AdapterID: d.ID, AdapterVersion: d.Version, RuntimeVersion: d.TestedRuntime}
}
func (h *sandboxPublicationHarness) route(turn string) string {
	route := "/v1/tasks/" + h.scope.Runtime
	if turn != "" {
		route += "/turns/" + turn
	}
	return route
}
func (h *sandboxPublicationHarness) engine() Engine {
	h.t.Helper()
	executable := filepath.Join(h.t.TempDir(), "must-not-execute")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf called > \"$0.called\"\nexit 99\n"), 0700); err != nil {
		h.t.Fatal(err)
	}
	return Engine{Config: Config{Scope: h.scope, Endpoint: h.server.URL, Workspace: filepath.Join(h.t.TempDir(), "workspace"), StateDir: h.t.TempDir(), RuntimeCLI: executable,
		RuntimeVersion: h.adapter.Descriptor().TestedRuntime, NativeSessions: true, AdapterVersion: h.adapter.Descriptor().Version, ExecutionPolicy: ExecutionUntrustedIsolated}, Adapter: h.adapter, Client: h.worker}
}
func (h *sandboxPublicationHarness) claim(turn string, pins []task.SkillPin) task.Record {
	h.t.Helper()
	var record task.Record
	var request any = task.Spec{Mode: task.FileTurn, Instruction: "synthetic isolated file delivery", Input: "original untrusted input", Output: "report.txt"}
	if turn != "" {
		var checkpoint task.Checkpoint
		if err := h.saas.JSON(h.t.Context(), "GET", h.route("")+"/checkpoint", nil, &checkpoint); err != nil {
			h.t.Fatal(err)
		}
		request = task.Question{SessionID: checkpoint.SessionID, Message: "continue synthetic isolated delivery"}
	}
	if err := h.saas.JSON(h.t.Context(), "PUT", h.route(turn), request, &record); err != nil {
		h.t.Fatal(err)
	}
	attempt := uuid.NewString()
	if err := h.worker.JSON(h.t.Context(), "POST", h.route(turn)+"/claim", task.Claim{Attempt: attempt, Skills: pins, ExecutionPolicy: task.ExecutionUntrustedIsolated}, &record); err != nil {
		h.t.Fatal(err)
	}
	h.attempts[turn] = attempt
	return record
}
func (h *sandboxPublicationHarness) finish(record task.Record, session string, refs []task.Ref) task.Record {
	h.t.Helper()
	var finished task.Record
	if err := h.worker.JSON(h.t.Context(), "POST", h.route(record.TurnID)+"/finish", task.Finish{Attempt: h.attempts[record.TurnID], SessionID: session, Artifacts: refs}, &finished); err != nil || finished.State != task.Succeeded {
		h.t.Fatal("synthetic publication could not finish through the public API", err)
	}
	return finished
}
func (h *sandboxPublicationHarness) packet(record task.Record, session string, workspaceRaw, sessionRaw []byte) (platform.SandboxTaskStatus, []byte) {
	h.t.Helper()
	a := platform.SandboxAddress{Scope: h.scope, TurnID: record.TurnID, Attempt: h.attempts[record.TurnID]}
	run, request, err := h.catalog.PrepareSandboxInput(h.t.Context(), a, h.profile())
	if err != nil {
		h.t.Fatal(err)
	}
	permit, err := h.catalog.FenceSandboxCreate(h.t.Context(), a, run.Deadline)
	if err != nil || !permit.CreateAllowed {
		h.t.Fatal("synthetic sandbox could not acquire its one create fence", err)
	}
	if _, err := h.catalog.SealSandboxPodTemplate(h.t.Context(), a, "synthetic-sandbox", sandboxPublicationDigest([]byte("fixed synthetic Pod template"))); err != nil {
		h.t.Fatal(err)
	}
	uid := uuid.NewString()
	if _, err := h.catalog.BindSandboxPod(h.t.Context(), a, uid); err != nil {
		h.t.Fatal(err)
	}
	result := sandboxPublicationResult(h.adapter.Descriptor(), session, workspaceRaw, sessionRaw)
	raw := sandboxPublicationEncode(h.t, result, workspaceRaw, sessionRaw)
	if _, err := h.catalog.SealSandboxOutput(h.t.Context(), permit.Capabilities.Output, sandboxPublicationBlob(raw), bytes.NewReader(raw)); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.catalog.RecordSandboxTerminal(h.t.Context(), a, uid, 0); err != nil {
		h.t.Fatal(err)
	}
	if err := fixtureGuestStopped(h.t.Context(), h.catalog, a, uid, 0); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.catalog.ConfirmSandboxDeleted(h.t.Context(), a, uid); err != nil {
		h.t.Fatal(err)
	}
	// Exercise the actual HTTP status shape: the private attempt is absent.
	status, err := h.worker.SandboxTask(h.t.Context(), h.route(record.TurnID)+"/sandbox", "/status", a.Attempt)
	if err != nil || !reflect.DeepEqual(status.Request, request) || status.Run.State != platform.SandboxReady || status.Run.Attempt != "" {
		h.t.Fatal("synthetic ready status differs from its actual catalog claim", err)
	}
	return status, raw
}
func (h *sandboxPublicationHarness) read(key asset.Key, version int64) (asset.Revision, []byte) {
	h.t.Helper()
	revision, reader, err := h.catalog.Read(h.t.Context(), h.scope, key, version)
	if err != nil {
		h.t.Fatal(err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(reader)
	if err != nil {
		h.t.Fatal(err)
	}
	return revision, raw
}
func (h *sandboxPublicationHarness) changeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.changes)
}
func (h *sandboxPublicationHarness) pin() task.SkillPin {
	h.t.Helper()
	raw := sandboxPublicationTree(h.t, []sandboxPublicationEntry{{name: "SKILL.md", value: "---\nname: pinned\n---\noriginal pinned package\n"}})
	file := filepath.Join(h.t.TempDir(), "skill.tar")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		h.t.Fatal(err)
	}
	key := asset.Key{Kind: "skill", ID: "catalog/skills/pinned"}
	meta := asset.Metadata{Key: key, AdapterVersion: h.adapter.Descriptor().Version, RuntimeVersion: h.adapter.Descriptor().TestedRuntime, Format: "skill-tar.v1", MediaType: "application/x-tar", Name: "pinned", Origin: "platform-managed", Completeness: "persisted-files", SHA256: sandboxPublicationDigest(raw), Size: int64(len(raw))}
	version, err := h.worker.Publish(h.t.Context(), asset.Change{Metadata: meta}, file)
	if err != nil {
		h.t.Fatal(err)
	}
	return task.SkillPin{Name: "pinned", Source: task.Ref{Key: key, Version: version, SHA256: meta.SHA256, ContentPath: key.ContentPath(version)}}
}

type sandboxPublicationEntry struct {
	name, value, target string
	kind                byte
}

func sandboxPublicationTree(t *testing.T, entries []sandboxPublicationEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Typeflag: kind, Mode: 0600, Linkname: entry.target, Format: tar.FormatUSTAR, ModTime: time.Unix(12345, 0)}
		if kind == tar.TypeReg {
			header.Size = int64(len(entry.value))
		}
		if kind == tar.TypeDir {
			header.Mode = 0700
		}
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := io.WriteString(w, entry.value); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func sandboxPublicationWorkspace(t *testing.T, value string) []byte {
	return sandboxPublicationTree(t, []sandboxPublicationEntry{{name: "input.txt", value: "original workspace input"}, {name: "out/report.txt", value: value}, {name: "out/nested/detail.json", value: "{\"synthetic\":true}\n"}, {name: sandboxPublicationSkill, value: "---\nname: native\nversion: original\n---\noriginal workspace Skill\n"}, {name: ".agents/skills/native/scripts/check.py", value: "print('original synthetic script')\n"}})
}
func sandboxPublicationDigest(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func sandboxPublicationBlob(raw []byte) platform.SandboxBlob {
	return platform.SandboxBlob{SHA256: sandboxPublicationDigest(raw), Size: int64(len(raw))}
}
func sandboxPublicationResult(d adapter.Descriptor, session string, workspaceRaw, sessionRaw []byte) nativeguest.Result {
	return nativeguest.Result{Version: nativeguest.Protocol, AdapterID: d.ID, AdapterVersion: d.Version, RuntimeVersion: d.TestedRuntime, SessionID: session, Text: "synthetic file-delivery summary",
		Workspace: nativeguest.BlobMeta{Format: "workspace-tar.v1", MediaType: "application/x-tar", SHA256: sandboxPublicationDigest(workspaceRaw), Size: int64(len(workspaceRaw))},
		Session:   nativeguest.BlobMeta{Format: "fixture-session.v1", MediaType: "application/octet-stream", SHA256: sandboxPublicationDigest(sessionRaw), Size: int64(len(sessionRaw))}}
}
func sandboxPublicationEncode(t *testing.T, result nativeguest.Result, workspaceRaw, sessionRaw []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := nativeguest.WriteResult(t.Context(), &output, result, func(name string) (io.ReadCloser, error) {
		switch name {
		case "workspace.tar":
			return io.NopCloser(bytes.NewReader(workspaceRaw)), nil
		case "session.blob":
			return io.NopCloser(bytes.NewReader(sessionRaw)), nil
		default:
			return nil, fmt.Errorf("unexpected synthetic result slot")
		}
	}, nativeguest.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestSandboxPublicationPreservesRawSessionWorkspaceAndCompleteDeliveryAcrossTurns(t *testing.T) {
	h := newSandboxPublicationHarness(t)
	pin := h.pin()
	record := h.claim("", []task.SkillPin{pin})
	session := uuid.NewString()
	var previousWorkspace string
	for iteration := range 2 {
		if iteration > 0 {
			record = h.claim(uuid.NewString(), []task.SkillPin{pin})
			if record.Parent == nil || record.Parent.SessionID != session || !reflect.DeepEqual(record.Skills, []task.SkillPin{pin}) {
				t.Fatal("continuation lost the fixed session or Skill pin")
			}
		}
		workspaceRaw := sandboxPublicationWorkspace(t, fmt.Sprintf("original synthetic report %d\n", iteration))
		sessionRaw := []byte(fmt.Sprintf("opaque original synthetic session %s turn %d\x00\xff", session, iteration))
		status, packet := h.packet(record, session, workspaceRaw, sessionRaw)
		e := h.engine()
		if e.Config.Workspace == previousWorkspace {
			t.Fatal("continuation reused its previous workspace")
		}
		previousWorkspace = e.Config.Workspace
		originalConfig := e.Config
		gotSession, refs, err := e.publishSandboxResult(t.Context(), record, status, bytes.NewReader(packet))
		if err != nil || gotSession != session || len(refs) != 2 || !reflect.DeepEqual(e.Config, originalConfig) {
			t.Fatal("isolated publication failed or mutated its caller configuration", gotSession, len(refs), err)
		}
		if h.adapter.nativeCalls.Load() != 0 {
			t.Fatal("trusted publication invoked a native capability")
		}
		if _, err := os.Stat(e.Config.RuntimeCLI + ".called"); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("trusted publication spawned the native executable")
		}
		for key, expected := range map[asset.Key][]byte{
			task.TurnKey(record.TurnID, asset.Key{Kind: "workspace", ID: "root"}):                  workspaceRaw,
			task.TurnKey(record.TurnID, asset.Key{Kind: "session", ID: session}):                   sessionRaw,
			task.TurnKey(record.TurnID, asset.Key{Kind: "artifact", ID: "out/report.txt"}):         []byte(fmt.Sprintf("original synthetic report %d\n", iteration)),
			task.TurnKey(record.TurnID, asset.Key{Kind: "artifact", ID: "out/nested/detail.json"}): []byte("{\"synthetic\":true}\n"),
		} {
			metadata, raw := h.read(key, 1)
			if !bytes.Equal(raw, expected) || metadata.Scope != h.scope || metadata.Version != 1 || metadata.AdapterVersion != h.adapter.Descriptor().Version || metadata.RuntimeVersion != h.adapter.Descriptor().TestedRuntime || metadata.SHA256 != sandboxPublicationDigest(expected) {
				t.Fatal("published metadata or original bytes changed", key)
			}
			if key.Kind == "artifact" && metadata.SessionID != session {
				t.Fatal("artifact metadata lost the fixed native session")
			}
			if key.Kind == "session" && (metadata.Format != "fixture-session.v1" || metadata.MediaType != "application/octet-stream") {
				t.Fatal("opaque original native format changed")
			}
		}
		for _, ref := range refs {
			if ref.Kind != "artifact" || ref.Version != 1 || ref.ContentPath != ref.Key.ContentPath(1) {
				t.Fatal("incomplete or unpinned artifact receipt", ref)
			}
		}
		skillMeta, skillRaw := h.read(task.TurnKey(record.TurnID, asset.Key{Kind: "skill", ID: sandboxPublicationSkill}), 1)
		if skillMeta.Format != "skill-tar.v1" || skillMeta.DeclaredVersion != "original" || !bytes.Contains(skillRaw, []byte("original workspace Skill")) || !bytes.Contains(skillRaw, []byte("original synthetic script")) {
			t.Fatal("publication skipped or rewrote the native Skill package")
		}
		finished := h.finish(record, session, refs)
		if len(finished.Assets) != 4 {
			t.Fatal("public finish omitted the complete delivery and checkpoint")
		}
	}
}

func TestSandboxPublicationRejectsUnboundOrIncompleteStatusBeforeWrites(t *testing.T) {
	for name, change := range map[string]func(*task.Record, *platform.SandboxTaskStatus){
		"scope":              func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.Scope.Tenant = "other" },
		"run-id":             func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.ID = "invalid" },
		"record-fingerprint": func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.RecordFingerprint = "invalid" },
		"pod-template-sha":   func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.PodTemplateSHA = "invalid" },
		"pod-namespace":      func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.PodNamespace = "Upper.invalid" },
		"pod-name":           func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.PodName = "foreign-pod" },
		"pod-uid":            func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.PodUID = "not-a-uuid" },
		"not-created":        func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.CreateStarted = false },
		"not-ready":          func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.State = platform.SandboxRunning },
		"not-deleted":        func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.PodDeleted = false },
		"not-terminal":       func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.PodTerminal = false },
		"nonzero-exit":       func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.ExitCode = 255 },
		"collected":          func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.Collected = true },
		"unsealed-input":     func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.Input.Sealed = false },
		"unsealed-output":    func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.Output.Sealed = false },
		"bad-input-digest":   func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.Input.SHA256 = "bad" },
		"source-spec":        func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.Source.Spec.Input = "different source" },
		"source-policy": func(_ *task.Record, s *platform.SandboxTaskStatus) {
			s.Run.Source.ExecutionPolicy = task.ExecutionTrustedLocal
		},
		"source-pins":     func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.Source.Skills = nil },
		"record-policy":   func(r *task.Record, _ *platform.SandboxTaskStatus) { r.ExecutionPolicy = task.ExecutionTrustedLocal },
		"record-state":    func(r *task.Record, _ *platform.SandboxTaskStatus) { r.State = task.Cancelled },
		"deadline":        func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Run.Deadline = s.Run.Deadline.Add(time.Second) },
		"request-adapter": func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Request.AdapterID = "different-fixture" },
		"request-version": func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Request.AdapterVersion = "unverified" },
		"request-runtime": func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Request.RuntimeVersion = "unverified runtime" },
		"request-spec":    func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Request.Spec.Instruction = "different request" },
		"request-pins":    func(_ *task.Record, s *platform.SandboxTaskStatus) { s.Request.Skills = nil },
	} {
		t.Run(name, func(t *testing.T) {
			h := newSandboxPublicationHarness(t)
			pin := h.pin()
			record := h.claim("", []task.SkillPin{pin})
			status, packet := h.packet(record, uuid.NewString(), sandboxPublicationWorkspace(t, "report"), []byte("opaque original session"))
			change(&record, &status)
			before := h.changeCount()
			e := h.engine()
			if _, _, err := e.publishSandboxResult(t.Context(), record, status, bytes.NewReader(packet)); err == nil {
				t.Fatal("unbound or incomplete status was accepted")
			}
			if h.changeCount() != before || h.adapter.nativeCalls.Load() != 0 {
				t.Fatal("rejected status wrote assets or invoked a native capability")
			}
		})
	}
}

func TestSandboxPublicationValidatesEntireEnvelopeAndArtifactsBeforeWrites(t *testing.T) {
	for _, condition := range []string{"outer-corrupt", "malformed", "native-version", "missing-main", "out-file", "out-symlink", "main-symlink", "nested-symlink", "too-many-artifacts"} {
		t.Run(condition, func(t *testing.T) {
			h := newSandboxPublicationHarness(t)
			record := h.claim("", nil)
			session := uuid.NewString()
			entries := []sandboxPublicationEntry{{name: "out/report.txt", value: "report"}}
			switch condition {
			case "missing-main":
				entries = []sandboxPublicationEntry{{name: "out/detail.txt", value: "detail"}}
			case "out-file":
				entries = []sandboxPublicationEntry{{name: "out", value: "regular file"}}
			case "out-symlink":
				entries = []sandboxPublicationEntry{{name: "data/report.txt", value: "report"}, {name: "out", kind: tar.TypeSymlink, target: "data"}}
			case "main-symlink":
				entries = []sandboxPublicationEntry{{name: "out/detail.txt", value: "report"}, {name: "out/report.txt", kind: tar.TypeSymlink, target: "detail.txt"}}
			case "nested-symlink":
				entries = append(entries, sandboxPublicationEntry{name: "out/link", kind: tar.TypeSymlink, target: "report.txt"})
			case "too-many-artifacts":
				for i := range task.MaxArtifacts {
					entries = append(entries, sandboxPublicationEntry{name: fmt.Sprintf("out/extra-%03d.txt", i), value: "extra"})
				}
			}
			workspaceRaw, sessionRaw := sandboxPublicationTree(t, entries), []byte("opaque original session")
			status, packet := h.packet(record, session, workspaceRaw, sessionRaw)
			switch condition {
			case "outer-corrupt":
				packet = append([]byte(nil), packet...)
				packet[len(packet)-1] ^= 1
			case "malformed":
				packet = []byte("not a native guest envelope")
				status.Run.Output = sandboxPublicationBlob(packet)
				status.Run.Output.Sealed = true
			case "native-version":
				result := sandboxPublicationResult(h.adapter.Descriptor(), session, workspaceRaw, sessionRaw)
				result.RuntimeVersion = "unsupported synthetic runtime"
				packet = sandboxPublicationEncode(t, result, workspaceRaw, sessionRaw)
				status.Run.Output = sandboxPublicationBlob(packet)
				status.Run.Output.Sealed = true
			}
			before := h.changeCount()
			e := h.engine()
			if _, _, err := e.publishSandboxResult(t.Context(), record, status, bytes.NewReader(packet)); err == nil {
				t.Fatal("invalid envelope or artifact tree was accepted")
			}
			if h.changeCount() != before || h.adapter.nativeCalls.Load() != 0 {
				t.Fatal("validation failure wrote an asset or invoked native code")
			}
			if _, err := os.Lstat(e.Config.Workspace); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed validation retained its new workspace")
			}
		})
	}
}

func TestSandboxPublicationRejectsContinuationParentDriftBeforeWrites(t *testing.T) {
	for name, change := range map[string]func(*platform.SandboxTaskStatus){
		"parent-session-id":     func(s *platform.SandboxTaskStatus) { s.Request.ParentSessionID = uuid.NewString() },
		"parent-workspace-sha":  func(s *platform.SandboxTaskStatus) { s.Request.Workspace.SHA256 = strings.Repeat("0", 64) },
		"parent-session-sha":    func(s *platform.SandboxTaskStatus) { s.Request.Session.SHA256 = strings.Repeat("0", 64) },
		"parent-session-format": func(s *platform.SandboxTaskStatus) { s.Request.Session.Format = "different-session.v1" },
		"parent-session-media":  func(s *platform.SandboxTaskStatus) { s.Request.Session.MediaType = "text/plain" },
		"source-parent-ref":     func(s *platform.SandboxTaskStatus) { s.Run.Source.Parent.Workspace.SHA256 = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newSandboxPublicationHarness(t)
			record, session := h.claim("", nil), uuid.NewString()
			status, packet := h.packet(record, session, sandboxPublicationWorkspace(t, "first report"), []byte("first opaque session"))
			engine := h.engine()
			id, refs, err := engine.publishSandboxResult(t.Context(), record, status, bytes.NewReader(packet))
			if err != nil {
				t.Fatal("initial positive control could not publish", err)
			}
			h.finish(record, id, refs)
			record = h.claim(uuid.NewString(), nil)
			status, packet = h.packet(record, session, sandboxPublicationWorkspace(t, "second report"), []byte("second opaque session"))
			change(&status)
			before := h.changeCount()
			engine = h.engine()
			if _, _, err := engine.publishSandboxResult(t.Context(), record, status, bytes.NewReader(packet)); err == nil {
				t.Fatal("changed continuation parent was accepted")
			}
			if h.changeCount() != before || h.adapter.nativeCalls.Load() != 0 {
				t.Fatal("rejected parent drift wrote assets or invoked native code")
			}
		})
	}
}

func TestSandboxPublicationRequiresActualAuthorityAndFreshWorkspace(t *testing.T) {
	for _, condition := range []string{"read", "write", "execute", "scope", "cancelled-task", "cancelled-context", "expired-context", "existing-workspace", "overlapping-state"} {
		t.Run(condition, func(t *testing.T) {
			h := newSandboxPublicationHarness(t)
			record := h.claim("", nil)
			status, packet := h.packet(record, uuid.NewString(), sandboxPublicationWorkspace(t, "report"), []byte("opaque original session"))
			e := h.engine()
			grant := platform.Grant{Scope: h.scope, Read: true, Write: true, Execute: true}
			ctx := t.Context()
			switch condition {
			case "read":
				grant.Read = false
			case "write":
				grant.Write = false
			case "execute":
				grant.Execute = false
			case "scope":
				grant.Scope.Tenant = "foreign"
			case "cancelled-task":
				var cancelled task.Record
				if err := h.saas.JSON(ctx, "POST", h.route("")+"/cancel", nil, &cancelled); err != nil {
					t.Fatal(err)
				}
			case "cancelled-context", "expired-context":
				var cancel context.CancelFunc
				if condition == "cancelled-context" {
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				} else {
					ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				}
				defer cancel()
			case "existing-workspace":
				if err := os.Mkdir(e.Config.Workspace, 0700); err != nil {
					t.Fatal(err)
				}
				put(t, e.Config.Workspace, "sentinel", "unchanged")
			case "overlapping-state":
				e.Config.StateDir = filepath.Join(e.Config.Workspace, "state")
			}
			e.Client = h.client(grant)
			before := h.changeCount()
			if _, _, err := e.publishSandboxResult(ctx, record, status, bytes.NewReader(packet)); err == nil {
				t.Fatal("publication accepted missing authority or a non-fresh workspace")
			}
			if h.changeCount() != before || h.adapter.nativeCalls.Load() != 0 {
				t.Fatal("rejected publication wrote assets or invoked native code")
			}
			if condition == "existing-workspace" {
				raw, err := os.ReadFile(filepath.Join(e.Config.Workspace, "sentinel"))
				if err != nil || string(raw) != "unchanged" {
					t.Fatal("existing workspace was overwritten or removed")
				}
			}
		})
	}
}

var _ adapter.Adapter = (*sandboxPublicationAdapter)(nil)
var _ adapter.SessionSource = (*sandboxPublicationAdapter)(nil)
var _ adapter.NativeSessions = (*sandboxPublicationAdapter)(nil)
var _ task.Executor = (*sandboxPublicationAdapter)(nil)
var _ task.Continuation = (*sandboxPublicationAdapter)(nil)
var _ task.FileExecutor = (*sandboxPublicationAdapter)(nil)
var _ task.SkillInstaller = (*sandboxPublicationAdapter)(nil)
