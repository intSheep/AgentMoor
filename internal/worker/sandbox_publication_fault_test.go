package worker

import (
	"archive/tar"
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
	"testing"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/nativeguest"
	"agentmoor/internal/platform"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

type sandboxPublicationFault struct {
	h           *sandboxPublicationHarness
	api         http.Handler
	point, mode string
	limit       int
	workspace   string
	target      asset.Key
	checkpoint  task.Checkpoint
	mu          sync.Mutex
	failures    int
	changes     []asset.Change
	uploads     [][]byte
}

func (f *sandboxPublicationFault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	stage := ""
	var change asset.Change
	var uploaded []byte
	if r.Method == "POST" && r.URL.Path == "/v1/changes" {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if json.Unmarshal(raw, &change) != nil {
			f.h.t.Error("invalid publication request")
		}
		stage = "begin"
	} else if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/content") {
		uploaded, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(uploaded))
		stage = "upload"
	} else if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/commit") {
		stage = "commit"
	}
	f.mu.Lock()
	match := stage == "begin" && change.Key == f.target
	if stage != "begin" && len(f.changes) > 0 {
		prefix := "/v1/changes/" + asset.Fingerprint(f.changes[0]) + "/"
		match = (stage == "upload" && r.URL.Path == prefix+"content") || (stage == "commit" && r.URL.Path == prefix+"commit")
	}
	if match {
		if stage == "begin" {
			f.changes = append(f.changes, change)
		}
		if stage == "upload" {
			f.uploads = append(f.uploads, append([]byte(nil), uploaded...))
		}
	}
	selected := match && strings.HasPrefix(f.point, stage+"-") && f.failures < f.limit
	if selected {
		f.failures++
	}
	f.mu.Unlock()
	if !selected {
		f.api.ServeHTTP(w, r)
		return
	}
	// A changed live file must never replace bytes already frozen in the
	// journal. This mutation is a host-side fault fixture, not a native agent.
	if err := os.WriteFile(filepath.Join(f.workspace, "out", "report.txt"), []byte("changed after immutable capture\n"), 0600); err != nil {
		f.h.t.Error(err)
	}
	var current task.Checkpoint
	if err := f.h.saas.JSON(r.Context(), "GET", f.h.route("")+"/checkpoint", nil, &current); err != nil || !reflect.DeepEqual(current, f.checkpoint) {
		f.h.t.Error("partial publication changed the successful checkpoint", err)
	}
	if f.mode == "cancel" {
		var record task.Record
		turn := strings.Split(f.target.ID, "/")[1]
		if err := f.h.saas.JSON(r.Context(), "POST", f.h.route(turn)+"/cancel", nil, &record); err != nil || record.State != task.Cancelling {
			f.h.t.Error("could not cancel publication fixture", err)
		}
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			f.h.t.Error("isolated publication did not observe task cancellation")
		}
		return
	}
	if f.mode == "deadline" {
		<-r.Context().Done()
		return
	}
	if strings.HasSuffix(f.point, "-after") {
		f.api.ServeHTTP(httptest.NewRecorder(), r)
	}
	if f.mode == "drop" {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			f.h.t.Error(err)
			return
		}
		connection.Close()
		return
	}
	http.Error(w, "owned synthetic publication fault", 503)
}

func sandboxPublicationFollowup(t *testing.T) (*sandboxPublicationHarness, Engine, task.Record, platform.SandboxTaskStatus, []byte, task.Checkpoint) {
	t.Helper()
	h := newSandboxPublicationHarness(t)
	record := h.claim("", nil)
	session := uuid.NewString()
	status, raw := h.packet(record, session, sandboxPublicationWorkspace(t, "first immutable report\n"), []byte("first opaque native session"))
	engine := h.engine()
	got, refs, err := engine.publishSandboxResult(t.Context(), record, status, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	h.finish(record, got, refs)
	var cp task.Checkpoint
	if err := h.saas.JSON(t.Context(), "GET", h.route("")+"/checkpoint", nil, &cp); err != nil {
		t.Fatal(err)
	}
	record = h.claim(uuid.NewString(), nil)
	status, raw = h.packet(record, session, sandboxPublicationWorkspace(t, "revised immutable report\n"), []byte("revised opaque native session"))
	engine = h.engine()
	return h, engine, record, status, raw, cp
}

func TestSandboxPublicationRetriesOnlyFrozenBytesAcrossHTTPFailures(t *testing.T) {
	for _, mode := range []string{"503", "drop"} {
		for _, point := range []string{"begin-before", "begin-after", "upload-before", "upload-after", "commit-before", "commit-after"} {
			t.Run(mode+"/"+point, func(t *testing.T) {
				h, e, record, status, raw, cp := sandboxPublicationFollowup(t)
				fault := &sandboxPublicationFault{h: h, api: h.api, point: point, mode: mode, limit: 1, workspace: e.Config.Workspace, target: task.TurnKey(record.TurnID, asset.Key{Kind: "artifact", ID: "out/report.txt"}), checkpoint: cp}
				h.api = fault
				session, refs, err := e.publishSandboxResult(t.Context(), record, status, bytes.NewReader(raw))
				if err != nil || len(refs) != 2 || session != cp.SessionID {
					t.Fatal("frozen isolated publication did not recover", err)
				}
				fault.mu.Lock()
				changes := append([]asset.Change(nil), fault.changes...)
				uploads := append([][]byte(nil), fault.uploads...)
				failures := fault.failures
				fault.mu.Unlock()
				if failures != 1 || len(changes) != 2 {
					t.Fatal("owned fault/retry was not exercised", failures, len(changes))
				}
				for _, ch := range changes {
					if !reflect.DeepEqual(ch, changes[0]) {
						t.Fatal("retry changed the frozen operation")
					}
				}
				for _, body := range uploads {
					if string(body) != "revised immutable report\n" {
						t.Fatal("retry read a changed live file")
					}
				}
				_, body := h.read(fault.target, 1)
				if string(body) != "revised immutable report\n" {
					t.Fatal("committed bytes differ")
				}
				before := sandboxCheckpoint(t, h)
				if !reflect.DeepEqual(before, cp) {
					t.Fatal("publication alone advanced checkpoint")
				}
				h.finish(record, session, refs)
				if !strings.HasPrefix(sandboxCheckpoint(t, h).Workspace.ID, "turns/"+record.TurnID+"/") {
					t.Fatal("successful finish did not move checkpoint")
				}
				assertSandboxPending(t, h, e, record, false)
				_, old := h.read(asset.Key{Kind: "artifact", ID: "out/report.txt"}, 1)
				if string(old) != "first immutable report\n" || h.adapter.nativeCalls.Load() != 0 {
					t.Fatal("old bytes or native boundary changed")
				}
			})
		}
	}
}

func sandboxCheckpoint(t *testing.T, h *sandboxPublicationHarness) task.Checkpoint {
	t.Helper()
	var cp task.Checkpoint
	if err := h.saas.JSON(t.Context(), "GET", h.route("")+"/checkpoint", nil, &cp); err != nil {
		t.Fatal(err)
	}
	return cp
}

func assertSandboxPending(t *testing.T, h *sandboxPublicationHarness, e Engine, record task.Record, pending bool) {
	t.Helper()
	c := e.Config
	c.StateDir = filepath.Join(c.StateDir, "publish")
	j, err := load(c)
	if err != nil {
		t.Fatal(err)
	}
	key := task.TurnKey(record.TurnID, asset.Key{Kind: "artifact", ID: "out/report.txt"})
	if pending {
		if j.Pending == nil || j.Pending.Change.Key != key || j.Cursors[asset.Fingerprint(key)].Version != 0 {
			t.Fatal("failed publication lost frozen pending state")
		}
		body, err := os.ReadFile(filepath.Join(c.StateDir, j.Pending.Blob))
		if err != nil || string(body) != "revised immutable report\n" {
			t.Fatal("failed publication lost original pending bytes", err)
		}
	} else if j.Pending != nil || j.Cursors[asset.Fingerprint(key)].Version != 1 {
		t.Fatal("successful publication did not commit exactly one cursor")
	}
	entries, err := os.ReadDir(e.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "sandbox-result-") {
			t.Fatal("isolated result validation spool leaked")
		}
	}
}

func TestSandboxPublicationFailureRetainsPendingAndOldCheckpoint(t *testing.T) {
	for _, mode := range []string{"exhausted", "cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			h, e, record, status, raw, cp := sandboxPublicationFollowup(t)
			fault := &sandboxPublicationFault{h: h, api: h.api, point: "begin-before", mode: mode, limit: 3, workspace: e.Config.Workspace, target: task.TurnKey(record.TurnID, asset.Key{Kind: "artifact", ID: "out/report.txt"}), checkpoint: cp}
			h.api = fault
			ctx := t.Context()
			if mode == "deadline" {
				var cancel context.CancelFunc
				// Reach the frozen artifact's HTTP fault before testing expiry.
				// Setup includes authenticated reads, restore and durable SQL
				// commits; a 150ms budget can expire before that boundary under
				// the full race suite and thus test an unrelated early exit.
				ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
			}
			if _, _, err := e.publishSandboxResult(ctx, record, status, bytes.NewReader(raw)); err == nil {
				t.Fatal("owned publication fault was hidden")
			}
			fault.mu.Lock()
			entered := fault.failures > 0
			fault.mu.Unlock()
			if !entered {
				t.Fatal("publication stopped before the selected frozen-artifact HTTP boundary")
			}
			if !reflect.DeepEqual(sandboxCheckpoint(t, h), cp) {
				t.Fatal("failed publication moved checkpoint")
			}
			assertSandboxPending(t, h, e, record, true)
			if h.adapter.nativeCalls.Load() != 0 {
				t.Fatal("failed publication invoked native code")
			}
			if mode == "exhausted" {
				// A later attempt uses the original sealed result and a new
				// restore path. The pending operation resumes before scanning.
				e.Config.Workspace = filepath.Join(t.TempDir(), "recovered-workspace")
				fault.workspace = e.Config.Workspace
				fault.limit = 0
				session, refs, err := e.publishSandboxResult(t.Context(), record, status, bytes.NewReader(raw))
				if err != nil || len(refs) != 2 {
					t.Fatal("pending publication could not recover without native execution", err)
				}
				h.finish(record, session, refs)
				assertSandboxPending(t, h, e, record, false)
			}
		})
	}
}

func TestSandboxInvalidResultCannotFlushAnEarlierPendingOperation(t *testing.T) {
	h, e, record, status, raw, _ := sandboxPublicationFollowup(t)
	fault := &sandboxPublicationFault{h: h, api: h.api, point: "begin-before", limit: 3, workspace: e.Config.Workspace, target: task.TurnKey(record.TurnID, asset.Key{Kind: "artifact", ID: "out/report.txt"}), checkpoint: sandboxCheckpoint(t, h)}
	h.api = fault
	if _, _, err := e.publishSandboxResult(t.Context(), record, status, bytes.NewReader(raw)); err == nil {
		t.Fatal("owned exhaustion was not exercised")
	}
	before := h.changeCount()
	e.Config.Workspace = filepath.Join(t.TempDir(), "invalid-retry")
	bad := append([]byte(nil), raw...)
	bad[len(bad)-1] ^= 1
	if _, _, err := e.publishSandboxResult(t.Context(), record, status, bytes.NewReader(bad)); err == nil {
		t.Fatal("invalid envelope recovered a pending operation")
	}
	if h.changeCount() != before {
		t.Fatal("invalid envelope flushed previous pending bytes")
	}
	assertSandboxPending(t, h, e, record, true)
}

func TestSandboxPublicationBoundsHostMetadataAndImplicitDirectoryExpansion(t *testing.T) {
	for _, kind := range []string{"metadata-depth", "metadata-tokens", "implicit-directories"} {
		t.Run(kind, func(t *testing.T) {
			h := newSandboxPublicationHarness(t)
			record := h.claim("", nil)
			session := uuid.NewString()
			workspace := sandboxPublicationWorkspace(t, "bounded synthetic result")
			if kind == "implicit-directories" {
				var entries []sandboxPublicationEntry
				for i := range 1100 {
					entries = append(entries, sandboxPublicationEntry{name: fmt.Sprintf("branch%04d/", i) + strings.Repeat("d/", 63) + "leaf"})
				}
				workspace = sandboxPublicationTree(t, entries)
			}
			sessionRaw := []byte("opaque synthetic native session")
			result := sandboxPublicationResult(h.adapter.Descriptor(), session, workspace, sessionRaw)
			metadata, _ := json.Marshal(result)
			if kind == "metadata-depth" {
				metadata = append(metadata[:len(metadata)-1], []byte(`,"extra":`+strings.Repeat("[", 2000)+"0"+strings.Repeat("]", 2000)+"}")...)
			}
			if kind == "metadata-tokens" {
				metadata = append(metadata[:len(metadata)-1], []byte(`,"extra":[`+strings.Repeat("0,", 65536)+"0]}")...)
			}
			var packet bytes.Buffer
			tw := tar.NewWriter(&packet)
			for _, slot := range []struct {
				name string
				raw  []byte
			}{{"result.json", metadata}, {"workspace.tar", workspace}, {"session.blob", sessionRaw}} {
				if err := tw.WriteHeader(&tar.Header{Name: slot.name, Size: int64(len(slot.raw)), Mode: 0600, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write(slot.raw); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			// The raw store intentionally accepts opaque sealed bytes. Model a
			// malicious guest's real upload, not a forged status/header checksum.
			a := platform.SandboxAddress{Scope: h.scope, Attempt: h.attempts[""]}
			run, _, err := h.catalog.PrepareSandboxInput(t.Context(), a, h.profile())
			if err != nil {
				t.Fatal(err)
			}
			permit, err := h.catalog.FenceSandboxCreate(t.Context(), a, run.Deadline)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.catalog.SealSandboxPodTemplate(t.Context(), a, "synthetic-sandbox", sandboxPublicationDigest([]byte("fixed bounded template"))); err != nil {
				t.Fatal(err)
			}
			uid := uuid.NewString()
			if _, err = h.catalog.BindSandboxPod(t.Context(), a, uid); err != nil {
				t.Fatal(err)
			}
			if _, err = h.catalog.SealSandboxOutput(t.Context(), permit.Capabilities.Output, sandboxPublicationBlob(packet.Bytes()), bytes.NewReader(packet.Bytes())); err != nil {
				t.Fatal(err)
			}
			if _, err = h.catalog.RecordSandboxTerminal(t.Context(), a, uid, 0); err != nil {
				t.Fatal(err)
			}
			if err = fixtureGuestStopped(t.Context(), h.catalog, a, uid, 0); err != nil {
				t.Fatal(err)
			}
			if _, err = h.catalog.ConfirmSandboxDeleted(t.Context(), a, uid); err != nil {
				t.Fatal(err)
			}
			status, err := h.worker.SandboxTask(t.Context(), h.route("")+"/sandbox", "/status", a.Attempt)
			if err != nil {
				t.Fatal(err)
			}
			e := h.engine()
			if _, _, err = e.publishSandboxResult(t.Context(), record, status, bytes.NewReader(packet.Bytes())); err == nil || !strings.Contains(err.Error(), "guest ") {
				t.Fatal("untrusted resource expansion reached host restore/publication", err)
			}
			if h.changeCount() != 0 || h.adapter.nativeCalls.Load() != 0 {
				t.Fatal("invalid guest resource expansion wrote assets or called native code")
			}
			if _, err := os.Lstat(e.Config.Workspace); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid guest resource expansion materialized a host workspace", err)
			}
			if int64(packet.Len()) > nativeguest.MaxBundleBytes {
				t.Fatal("resource expansion fixture exceeds the allowed wire size")
			}
		})
	}
}

func TestSandboxPublicationChecksIndependentEnvelopeDigestAfterValidInnerDecode(t *testing.T) {
	h := newSandboxPublicationHarness(t)
	record := h.claim("", nil)
	status, packet := h.packet(record, uuid.NewString(), sandboxPublicationWorkspace(t, "original report"), []byte("opaque original native session"))
	corrupt := bytes.Replace(packet, []byte("synthetic file-delivery summary"), []byte("Synthetic file-delivery summary"), 1)
	if len(corrupt) != len(packet) || bytes.Equal(corrupt, packet) {
		t.Fatal("equal-size corruption fixture did not alter result metadata")
	}
	// This change leaves every inner hash, fixed slot and JSON field valid.
	// Only the separately sealed whole-envelope checksum can reject it.
	if _, err := nativeguest.ReadResult(t.Context(), bytes.NewReader(corrupt), filepath.Join(t.TempDir(), "inner-positive-control"), status.Request, nativeguest.DefaultLimits()); err != nil {
		t.Fatal("corruption fixture must still pass the inner bundle checks", err)
	}
	e := h.engine()
	if _, _, err := e.publishSandboxResult(t.Context(), record, status, bytes.NewReader(corrupt)); err == nil {
		t.Fatal("valid inner envelope bypassed the independent output digest", err)
	}
	if h.changeCount() != 0 || h.adapter.nativeCalls.Load() != 0 {
		t.Fatal("whole-envelope corruption wrote assets or invoked native code")
	}
	if _, err := os.Lstat(e.Config.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("whole-envelope corruption materialized a host workspace", err)
	}
}

func TestSandboxPublicationCleansInvalidWorkspaceDespiteUntrustedPermissions(t *testing.T) {
	for _, locked := range []string{".", "out", "out/nested", "private"} {
		t.Run(locked, func(t *testing.T) {
			h := newSandboxPublicationHarness(t)
			record := h.claim("", nil)
			var workspace bytes.Buffer
			tw := tar.NewWriter(&workspace)
			// The missing primary result guarantees rejection even when tests
			// run as root and directory permission checks are bypassed.
			for _, entry := range []struct {
				name string
				kind byte
			}{{locked, tar.TypeDir}, {locked + "/denied.txt", tar.TypeReg}, {"out/secondary.txt", tar.TypeReg}} {
				name := strings.TrimPrefix(entry.name, "./")
				mode := int64(0600)
				if entry.kind == tar.TypeDir {
					mode = 0
				}
				if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: entry.kind, Mode: mode, Format: tar.FormatUSTAR}); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			status, packet := h.packet(record, uuid.NewString(), workspace.Bytes(), []byte("opaque native session"))
			e := h.engine()
			if _, _, err := e.publishSandboxResult(t.Context(), record, status, bytes.NewReader(packet)); err == nil {
				t.Fatal("invalid delivery with unreadable directories was accepted")
			}
			if h.changeCount() != 0 || h.adapter.nativeCalls.Load() != 0 {
				t.Fatal("invalid delivery wrote assets or invoked native code")
			}
			if _, err := os.Lstat(e.Config.Workspace); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("untrusted permissions blocked invalid workspace cleanup", err)
			}
		})
	}
}
