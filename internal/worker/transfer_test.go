package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	"agentmoor/internal/adapters/workspace"
	"agentmoor/internal/asset"
	"agentmoor/internal/platform"
	"agentmoor/internal/snapshot"
)

type transferRequest struct {
	stage, path, encoding string
	body                  []byte
	failed                bool
}

type transferHarness struct {
	t        *testing.T
	e        Engine
	mu       sync.Mutex
	requests []transferRequest
	fault    string
}

func newTransferHarness(t *testing.T) *transferHarness {
	t.Helper()
	catalog, err := platform.OpenCatalog(t.Context(), t.TempDir(), "", platform.DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { catalog.Close() })
	secret := bytes.Repeat([]byte{9}, 32)
	api := (&platform.API{Catalog: catalog, Secret: secret, Temp: t.TempDir()}).Handler()
	h := &transferHarness{t: t}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stage := ""
		if r.Method == "POST" && r.URL.Path == "/v1/changes" {
			stage = "begin"
		} else if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/parts") {
			stage = "parts"
		} else if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/commit") {
			stage = "commit"
		} else if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/content") {
			stage = "content"
		} else if r.Method == "PUT" && strings.Contains(r.URL.Path, "/parts/") {
			stage = "part"
		}
		if stage != "" {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				http.Error(w, "fixture failed reading request", 500)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			h.mu.Lock()
			fail := h.fault == stage
			if fail {
				h.fault = ""
			}
			h.requests = append(h.requests, transferRequest{stage: stage, path: r.URL.Path, encoding: r.Header.Get("Content-Encoding"), body: raw, failed: fail})
			h.mu.Unlock()
			if fail {
				http.Error(w, "fixture one-time transfer failure", http.StatusServiceUnavailable)
				return
			}
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	scope := asset.Scope{Tenant: "fixture", Project: "transfer", Runtime: "owned", Adapter: "parts-fixture"}
	token, err := platform.Issue(secret, platform.Grant{Scope: scope, Read: true, Write: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	client, err := platform.NewClient(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	h.e = Engine{Config: Config{Scope: scope, Endpoint: server.URL, Workspace: t.TempDir(), StateDir: t.TempDir()}, Adapter: workspace.Layout{ID: scope.Adapter, Version: "1"}, Client: client}
	return h
}

func (h *transferHarness) mark() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

func (h *transferHarness) since(mark int) []transferRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]transferRequest(nil), h.requests[mark:]...)
}

func (h *transferHarness) arm(stage string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fault = stage
}

func (h *transferHarness) frozen() []byte {
	h.t.Helper()
	root, err := os.OpenRoot(h.e.Config.Workspace)
	if err != nil {
		h.t.Fatal(err)
	}
	defer root.Close()
	b, err := snapshot.Tree(h.t.Context(), root, []snapshot.Entry{{Source: ".", Name: "."}}, h.t.TempDir())
	if err != nil {
		h.t.Fatal(err)
	}
	raw, err := os.ReadFile(b.Path)
	if err != nil {
		h.t.Fatal(err)
	}
	return raw
}

func (h *transferHarness) verifyRevision(version int64, expected []byte) {
	h.t.Helper()
	rev, stream, err := h.e.Client.Content(h.t.Context(), asset.Key{Kind: "workspace", ID: "root"}, version)
	if err != nil {
		h.t.Fatal(err)
	}
	raw, err := io.ReadAll(stream)
	stream.Close()
	hash := sha256.Sum256(expected)
	if err != nil || !bytes.Equal(raw, expected) || rev.Scope != h.e.Config.Scope || rev.Format != "workspace-tar.v1" || rev.MediaType != "application/x-tar" || rev.Size != int64(len(expected)) || rev.SHA256 != hex.EncodeToString(hash[:]) {
		h.t.Fatal("fixed-version HTTP tar differs from independently frozen bytes", version, rev, err)
	}
	dest := filepath.Join(h.t.TempDir(), "restored")
	if err := h.e.Restore(h.t.Context(), rev.Key, version, dest); err != nil {
		h.t.Fatal(err)
	}
	verifyTransferredTree(h.t, dest, expected)
	if err := h.e.Restore(h.t.Context(), rev.Key, version, dest); err == nil {
		h.t.Fatal("restore replaced an existing target")
	}
	verifyTransferredTree(h.t, dest, expected)
}

func verifyTransferredTree(t *testing.T, dir string, expected []byte) {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(expected))
	seen := make(map[string]bool)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[header.Name] = true
		path := filepath.Join(dir, header.Name)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		switch header.Typeflag {
		case tar.TypeReg:
			raw, err := os.ReadFile(path)
			want, readErr := io.ReadAll(reader)
			if err != nil || readErr != nil || !info.Mode().IsRegular() || !bytes.Equal(raw, want) || info.Mode().Perm() != os.FileMode(header.Mode) {
				t.Fatal("restored file content or permissions differ", header.Name, err, readErr)
			}
		case tar.TypeDir:
			if !info.IsDir() || info.Mode().Perm() != os.FileMode(header.Mode) {
				t.Fatal("restored directory differs", header.Name, info)
			}
		case tar.TypeSymlink:
			target, err := os.Readlink(path)
			if err != nil || info.Mode()&os.ModeSymlink == 0 || target != header.Linkname {
				t.Fatal("restored relative link differs", header.Name, target, err)
			}
		default:
			t.Fatal("unsupported expected fixture entry", header.Typeflag)
		}
	}
	if err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err == nil && !seen[filepath.ToSlash(rel)] {
			t.Error("restore added unexpected entry", rel)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func transferStageCounts(requests []transferRequest) map[string]int {
	counts := make(map[string]int)
	for _, request := range requests {
		counts[request.stage]++
	}
	return counts
}

func TestWorkspacePartsTransferReusesBodiesAndPreservesTarHistory(t *testing.T) {
	h := newTransferHarness(t)
	put(t, h.e.Config.Workspace, "one", "raw-initial-one")
	put(t, h.e.Config.Workspace, "two", "raw-initial-two")
	var versions [][]byte
	sync := func(expectedParts int, raw bool) {
		t.Helper()
		expected := h.frozen()
		mark := h.mark()
		if err := h.e.Sync(t.Context()); err != nil {
			t.Fatal(err)
		}
		requests := h.since(mark)
		want := map[string]int{"begin": 1, "commit": 1}
		if raw {
			want["content"] = 1
		} else {
			want["parts"] = 1
			if expectedParts > 0 {
				want["part"] = expectedParts
			}
		}
		if got := transferStageCounts(requests); !reflect.DeepEqual(got, want) {
			t.Fatal("unexpected workspace transfer requests", got, want)
		}
		var change asset.Change
		for _, request := range requests {
			if request.stage == "begin" {
				if err := json.Unmarshal(request.body, &change); err != nil {
					t.Fatal(err)
				}
			}
			if request.stage == "content" && !bytes.Equal(request.body, expected) {
				t.Fatal("legacy upload did not send exact tar bytes")
			}
			if request.stage == "parts" {
				if request.encoding != "gzip" {
					t.Fatal("manifest was not compressed on the wire")
				}
				reader, err := gzip.NewReader(bytes.NewReader(request.body))
				if err != nil {
					t.Fatal(err)
				}
				var parts asset.TarParts
				err = json.NewDecoder(reader).Decode(&parts)
				reader.Close()
				if err != nil || parts.Validate(change) != nil {
					t.Fatal("wire manifest does not describe the logical change", err)
				}
			}
			if request.stage == "part" {
				digest := sha256.Sum256(request.body)
				if !strings.HasSuffix(request.path, "/"+hex.EncodeToString(digest[:])) {
					t.Fatal("uploaded body differs from its scoped content address")
				}
			}
		}
		versions = append(versions, expected)
		j, err := load(h.e.Config)
		if err != nil || j.Pending != nil || j.State != "complete" || j.Cursors[asset.Fingerprint(asset.Key{Kind: "workspace", ID: "root"})].Version != int64(len(versions)) {
			t.Fatal("transfer did not commit exactly one cursor version", j, err)
		}
	}
	sync(0, true)
	h.e.Config.WorkspaceTransfer = asset.TarPartsFormat
	put(t, h.e.Config.Workspace, "one", "first-parts-a")
	put(t, h.e.Config.Workspace, "two", "first-parts-b")
	sync(2, false)
	put(t, h.e.Config.Workspace, "one", "other-parts-a") // same size, different body
	sync(1, false)
	mark := h.mark()
	if err := h.e.Sync(t.Context()); err != nil || len(h.since(mark)) != 0 {
		t.Fatal("unchanged workspace sent a mutating HTTP request", h.since(mark), err)
	}
	if err := os.Rename(filepath.Join(h.e.Config.Workspace, "two"), filepath.Join(h.e.Config.Workspace, "renamed")); err != nil {
		t.Fatal(err)
	}
	sync(0, false)
	if err := os.Chmod(filepath.Join(h.e.Config.Workspace, "one"), 0751); err != nil {
		t.Fatal(err)
	}
	sync(0, false)
	if err := os.Mkdir(filepath.Join(h.e.Config.Workspace, "links"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../one", filepath.Join(h.e.Config.Workspace, "links", "alias")); err != nil {
		t.Fatal(err)
	}
	sync(0, false)
	if err := os.Remove(filepath.Join(h.e.Config.Workspace, "renamed")); err != nil {
		t.Fatal(err)
	}
	sync(0, false)
	var revisions []asset.Revision
	if err := h.e.Client.JSON(t.Context(), "GET", "/v1/assets", nil, &revisions); err != nil || len(revisions) != len(versions) {
		t.Fatal("metadata-only updates created the wrong revision count", revisions, err)
	}
	for index, expected := range versions {
		h.verifyRevision(int64(index+1), expected)
	}
	h.verifyRevision(0, versions[len(versions)-1])
}

func TestWorkspacePendingTransferSurvivesConfigurationChanges(t *testing.T) {
	for _, test := range []struct {
		name, initial, next, fault string
	}{
		{"parts prepare to raw", asset.TarPartsFormat, "", "parts"},
		{"part upload to raw", asset.TarPartsFormat, "", "part"},
		{"parts commit to raw", asset.TarPartsFormat, "", "commit"},
		{"raw upload to parts", "", asset.TarPartsFormat, "content"},
		{"raw commit to parts", "", asset.TarPartsFormat, "commit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newTransferHarness(t)
			h.e.Config.WorkspaceTransfer = test.initial
			put(t, h.e.Config.Workspace, "report", "original frozen body")
			original := h.frozen()
			h.arm(test.fault)
			if err := h.e.Sync(t.Context()); err == nil {
				t.Fatal("ordinary Sync hid the first transfer failure")
			}
			failed, err := load(h.e.Config)
			key := asset.Key{Kind: "workspace", ID: "root"}
			if err != nil || failed.State != "failed" || failed.Pending == nil || failed.Pending.Transfer != test.initial || failed.Cursors[asset.Fingerprint(key)].Version != 0 {
				t.Fatal("failure changed pending mode or advanced the cursor", failed, err)
			}
			old := *failed.Pending
			frozen, err := os.ReadFile(filepath.Join(h.e.Config.StateDir, old.Blob))
			if err != nil || !bytes.Equal(frozen, original) {
				t.Fatal("pending tar bytes changed", err)
			}
			op := asset.Fingerprint(old.Change)
			var before []asset.Revision
			if err := h.e.Client.JSON(t.Context(), "GET", "/v1/assets", nil, &before); err != nil || len(before) != 0 {
				t.Fatal("uncommitted transfer became visible", before, err)
			}
			h.e.Config.WorkspaceTransfer = test.next
			put(t, h.e.Config.Workspace, "report", "new live content after the failure")
			latest := h.frozen()
			mark := h.mark()
			if err := h.e.Sync(t.Context()); err != nil {
				t.Fatal(err)
			}
			var changes []asset.Change
			for _, request := range h.since(mark) {
				if request.stage == "begin" {
					var change asset.Change
					if err := json.Unmarshal(request.body, &change); err != nil {
						t.Fatal(err)
					}
					changes = append(changes, change)
				}
				if !strings.HasPrefix(request.path, "/v1/changes/"+op+"/") {
					continue
				}
				if test.initial == asset.TarPartsFormat && request.stage == "content" || test.initial == "" && (request.stage == "parts" || request.stage == "part") {
					t.Fatal("recovery used the new configuration for the frozen old operation", request.stage)
				}
				if request.stage == "content" && !bytes.Equal(request.body, original) {
					t.Fatal("raw pending recovery uploaded the changed live workspace")
				}
				if request.stage == "part" && string(request.body) != "original frozen body" {
					t.Fatal("parts pending recovery uploaded the changed live workspace")
				}
			}
			if len(changes) != 2 || !reflect.DeepEqual(changes[0], old.Change) || asset.Fingerprint(changes[0]) != op || changes[1].Base != 1 || changes[1].SHA256 == changes[0].SHA256 {
				t.Fatal("recovery changed old operation identity or skipped new content", changes, old)
			}
			newOp := asset.Fingerprint(changes[1])
			var newStages []transferRequest
			for _, request := range h.since(mark) {
				if strings.HasPrefix(request.path, "/v1/changes/"+newOp+"/") {
					newStages = append(newStages, request)
				}
			}
			wantNew := map[string]int{"commit": 1, "content": 1}
			if test.next == asset.TarPartsFormat {
				wantNew = map[string]int{"commit": 1, "parts": 1, "part": 1}
			}
			if got := transferStageCounts(newStages); !reflect.DeepEqual(got, wantNew) {
				t.Fatal("new live content did not adopt the new transfer configuration", got, wantNew)
			}
			allBegins := 0
			for _, request := range h.since(0) {
				if request.stage != "begin" {
					continue
				}
				var change asset.Change
				if err := json.Unmarshal(request.body, &change); err != nil {
					t.Fatal(err)
				}
				if asset.Fingerprint(change) == op {
					allBegins++
				}
			}
			if allBegins != 2 {
				t.Fatal("ordinary Sync retried early or changed the operation key", allBegins)
			}
			complete, err := load(h.e.Config)
			if err != nil || complete.State != "complete" || complete.Pending != nil || complete.Cursors[asset.Fingerprint(key)].Version != 2 || complete.Cursors[asset.Fingerprint(key)].Fingerprint != asset.Fingerprint(changes[1].Metadata) {
				t.Fatal("recovery cursor did not end at the new committed version", complete, err)
			}
			var revisions []asset.Revision
			if err := h.e.Client.JSON(t.Context(), "GET", "/v1/assets", nil, &revisions); err != nil || len(revisions) != 2 || revisions[0].Version != 1 || revisions[1].Version != 2 {
				t.Fatal("recovery created duplicate or missing versions", revisions, err)
			}
			var operations []asset.Job
			if err := h.e.Client.JSON(t.Context(), "GET", "/v1/status", nil, &operations); err != nil || len(operations) != 2 {
				t.Fatal("recovery created an extra operation", operations, err)
			}
			for _, operation := range operations {
				if operation.State != "committed" {
					t.Fatal("recovery left an uncommitted operation", operation)
				}
			}
			h.verifyRevision(1, original)
			h.verifyRevision(2, latest)
		})
	}
}
