package contracttest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/platform"
	"github.com/intSheep/AgentMoor/internal/worker"
)

var skillBytes = []byte("---\nname: contract-greeting\nmetadata:\n  version: '2.0'\ndependencies: [helper]\n---\nOriginal Skill body.\n")
var helperBytes = []byte("support\x00original\n")

// Run exercises one mechanism through the same public HTTP asset contract.
// The case supplies native fixtures, never platform assertions or a pass flag.
func Run(t *testing.T, c Case) {
	t.Helper()
	t.Run(c.Name, func(t *testing.T) {
		paths := Paths{Workspace: t.TempDir(), Sessions: t.TempDir(), Runtime: t.TempDir()}
		for _, name := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
			dir := filepath.Join(paths.Runtime, strings.ToLower(name))
			must(t, os.MkdirAll(dir, 0700))
			t.Setenv(name, dir)
		}
		for _, name := range []string{"AGENTMOOR_TOKEN", "AGENTMOOR_TOKEN_FILE", "AGENTMOOR_POSTGRES_DSN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "OPENAI_API_KEY", "KUBECONFIG"} {
			t.Setenv(name, "synthetic-contract-canary")
		}
		if c.Prepare == nil {
			t.Fatal("case has no fixture constructor")
		}
		f := c.Prepare(t, paths)
		if f.Adapter == nil || f.SessionID == "" || f.SessionFormat == "" || len(f.SessionFiles) == 0 || !asset.SafePath(f.SkillManifest) {
			t.Fatal("incomplete asset fixture")
		}
		for name, raw := range f.SessionFiles {
			if name != "" && !asset.SafePath(name) || name == "" && len(f.SessionFiles) != 1 || len(raw) > 1<<20 {
				t.Fatal("fixture must use bounded original bytes and safe relative paths")
			}
		}
		must(t, ValidateDescriptor(f.Adapter))
		switch f.Mechanism {
		case Filesystem:
			if f.Native != nil {
				t.Fatal("file mechanism unexpectedly supplied a native CLI")
			}
		case NativeCLI:
			if f.Native == nil || f.Native.Executable == "" || f.Native.Calls == nil || f.Native.SetVersion == nil || f.Native.AssertImported == nil {
				t.Fatal("native mechanism requires an observable CLI fixture")
			}
			if _, ok := f.Adapter.(adapter.NativeSessions); !ok {
				t.Fatal("native mechanism has no NativeSessions implementation")
			}
		default:
			t.Fatal("unsupported test mechanism", f.Mechanism)
		}
		root, err := os.OpenRoot(paths.Workspace)
		must(t, err)
		evidence, err := f.Adapter.Detect(t.Context(), root)
		must(t, root.Close())
		must(t, err)
		if len(evidence) == 0 {
			t.Fatal("the assigned workspace has no adapter-specific marker")
		}
		write(t, filepath.Join(paths.Workspace, f.SkillManifest), skillBytes)
		write(t, filepath.Join(paths.Workspace, filepath.Dir(f.SkillManifest), "helper"), helperBytes)
		write(t, filepath.Join(paths.Workspace, "out/result.txt"), []byte("first\n"))

		catalog, err := platform.OpenCatalog(t.Context(), t.TempDir(), "", platform.DiskObjects{Dir: t.TempDir()})
		must(t, err)
		t.Cleanup(func() { must(t, catalog.Close()) })
		secret := bytes.Repeat([]byte{19}, 32)
		api := (&platform.API{Catalog: catalog, Secret: secret, Temp: t.TempDir()}).Handler()
		var drop, corrupt atomic.Bool
		drop.Store(true)
		var dropped atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/commit") && drop.CompareAndSwap(true, false) {
				reply := httptest.NewRecorder()
				api.ServeHTTP(reply, r)
				if reply.Code != http.StatusOK {
					t.Errorf("post-commit fault never reached a successful commit: %d", reply.Code)
				}
				dropped.Add(1)
				http.Error(w, "synthetic post-commit response failure", http.StatusServiceUnavailable)
				return
			}
			if r.URL.Path == "/v1/content" && corrupt.CompareAndSwap(true, false) {
				reply := httptest.NewRecorder()
				api.ServeHTTP(reply, r)
				for k, values := range reply.Header() {
					w.Header()[k] = append([]string(nil), values...)
				}
				body := reply.Body.Bytes()
				if reply.Code == http.StatusOK && len(body) > 0 {
					body[0] ^= 1 // Preserve the real revision header and byte length.
				} else {
					t.Errorf("checksum fault did not reach valid content: HTTP %d", reply.Code)
				}
				w.WriteHeader(reply.Code)
				_, _ = w.Write(body)
				return
			}
			api.ServeHTTP(w, r)
		}))
		t.Cleanup(server.Close)
		scope := asset.Scope{Tenant: "contract", Project: "assets", Runtime: "roundtrip", Adapter: f.Adapter.Descriptor().ID}
		client := scopedClient(t, server.URL, secret, scope)
		cfg := worker.Config{Scope: scope, Endpoint: server.URL, Workspace: paths.Workspace, Sessions: paths.Sessions,
			StateDir: t.TempDir(), Artifacts: "out", ManagedSkills: []string{f.SkillManifest}}
		if f.Mechanism == NativeCLI {
			cfg.Sessions, cfg.NativeSessions = "", true
			cfg.RuntimeCLI, cfg.RuntimeVersion = f.Native.Executable, f.Adapter.Descriptor().TestedRuntime
		}
		engine := worker.Engine{Config: cfg, Adapter: f.Adapter, Client: client}
		if engine.Sync(t.Context()) == nil || dropped.Load() != 1 {
			t.Fatal("the actual post-commit HTTP failure was not exercised")
		}
		must(t, engine.Sync(t.Context()))
		before := heads(t, client)
		if len(before) != 4 {
			t.Fatal("expected exactly the four asset kinds", len(before))
		}
		for key, rev := range before {
			if rev.Version != 1 || rev.AdapterVersion != f.Adapter.Descriptor().Version {
				t.Fatal("retry duplicated a revision or changed its adapter", key, rev.Version)
			}
		}
		sessionKey := asset.Key{Kind: "session", ID: f.SessionID}
		if s, ok := before[sessionKey]; !ok || s.Format != f.SessionFormat {
			t.Fatal("public session identity or format changed")
		}
		skill := before[asset.Key{Kind: "skill", ID: f.SkillManifest}]
		if skill.Name != "contract-greeting" || skill.DeclaredVersion != "2.0" || skill.Origin != "platform-managed" || !reflect.DeepEqual(skill.Dependencies, []string{"helper"}) {
			t.Fatal("public Skill metadata differs from the original manifest")
		}
		must(t, engine.Sync(t.Context()))
		if !reflect.DeepEqual(before, heads(t, client)) {
			t.Fatal("an unchanged rescan advanced a public asset")
		}
		write(t, filepath.Join(paths.Workspace, "out/result.txt"), []byte("second\n"))
		must(t, engine.Sync(t.Context()))
		after := heads(t, client)
		if len(after) != 4 || !reflect.DeepEqual(after[sessionKey], before[sessionKey]) {
			t.Fatal("unrelated edit changed the native session")
		}
		artifact := asset.Key{Kind: "artifact", ID: "out/result.txt"}
		if after[artifact].Version != 2 || after[asset.Key{Kind: "workspace", ID: "root"}].Version != 2 {
			t.Fatal("the changed file did not advance its two public assets")
		}
		for _, check := range []struct {
			key     asset.Key
			version int64
			files   map[string][]byte
		}{
			{asset.Key{Kind: "workspace", ID: "root"}, 1, map[string][]byte{"out/result.txt": []byte("first\n")}},
			{asset.Key{Kind: "workspace", ID: "root"}, 2, map[string][]byte{"out/result.txt": []byte("second\n")}},
			{asset.Key{Kind: "skill", ID: f.SkillManifest}, 1, map[string][]byte{filepath.Base(f.SkillManifest): skillBytes, "helper": helperBytes}},
			{artifact, 1, map[string][]byte{"": []byte("first\n")}},
			{artifact, 2, map[string][]byte{"": []byte("second\n")}},
			{sessionKey, 1, f.SessionFiles},
		} {
			dest := filepath.Join(t.TempDir(), "restored")
			must(t, engine.Restore(t.Context(), check.key, check.version, dest))
			if check.key.Kind == "session" || check.key.Kind == "skill" {
				assertExactFiles(t, dest, check.files)
			} else {
				assertFiles(t, dest, check.files)
			}
		}
		// All assertions below observe public reads and fresh-path publication.
		existing := filepath.Join(t.TempDir(), "existing")
		write(t, existing, []byte("keep-existing"))
		if engine.Restore(t.Context(), artifact, 2, existing) == nil {
			t.Fatal("restore replaced an existing destination")
		}
		assertFiles(t, existing, map[string][]byte{"": []byte("keep-existing")})
		bad := filepath.Join(t.TempDir(), "bad-digest")
		corrupt.Store(true)
		if engine.Restore(t.Context(), artifact, 1, bad) == nil {
			t.Fatal("restore accepted corrupted HTTP content")
		}
		if corrupt.Load() {
			t.Fatal("restore failed before consuming the content corruption")
		}
		absent(t, bad)
		missing := filepath.Join(t.TempDir(), "missing")
		if err := engine.Restore(t.Context(), artifact, 99, missing); !errors.Is(err, platform.ErrMissing) {
			t.Fatal("missing immutable version was not rejected", err)
		}
		absent(t, missing)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		cancelled := filepath.Join(t.TempDir(), "cancelled")
		if err := engine.Restore(ctx, artifact, 1, cancelled); !errors.Is(err, context.Canceled) {
			t.Fatal("pre-cancelled restore did not report cancellation", err)
		}
		absent(t, cancelled)
		if f.Mechanism == NativeCLI {
			unverified := engine
			unverified.Config.Scope.Runtime = "unverified-runtime"
			unverified.Config.StateDir, unverified.Config.RuntimeVersion = t.TempDir(), ""
			unverified.Client = scopedClient(t, server.URL, secret, unverified.Config.Scope)
			checkNative(t, &engine, &unverified, f, paths, after, &corrupt)
		}
		must(t, os.RemoveAll(paths.Workspace))
		must(t, os.RemoveAll(paths.Sessions))
		must(t, engine.Restore(t.Context(), asset.Key{Kind: "workspace", ID: "root"}, 2, paths.Workspace))
		assertFiles(t, paths.Workspace, map[string][]byte{"out/result.txt": []byte("second\n"), f.SkillManifest: skillBytes})
		if f.Mechanism == NativeCLI {
			dest := filepath.Join(t.TempDir(), "native-home")
			must(t, engine.RestoreNative(t.Context(), f.SessionID, 1, dest))
			f.Native.AssertImported(t, dest)
			calls := f.Native.Calls(t)
			if calls.List == 0 || calls.Export == 0 || calls.Import != 1 {
				t.Fatal("the native CLI mechanism was bypassed", calls)
			}
			t.Logf("ADAPTER_CONTRACT mechanism=%s runtime=%s calls=%+v four_assets=true old_versions=true", f.Mechanism, cfg.Scope.Adapter, calls)
		} else {
			must(t, engine.Restore(t.Context(), sessionKey, 1, paths.Sessions))
			assertExactFiles(t, paths.Sessions, f.SessionFiles)
			t.Logf("ADAPTER_CONTRACT mechanism=%s runtime=%s four_assets=true old_versions=true", f.Mechanism, cfg.Scope.Adapter)
		}
		if !reflect.DeepEqual(after, heads(t, client)) {
			t.Fatal("restore or a rejected operation changed published revisions")
		}
	})
}

func heads(t *testing.T, client *platform.Client) map[asset.Key]asset.Revision {
	t.Helper()
	var rows []asset.Revision
	must(t, client.JSON(t.Context(), "GET", "/v1/heads", nil, &rows))
	result := make(map[asset.Key]asset.Revision)
	for _, row := range rows {
		if _, exists := result[row.Key]; exists {
			t.Fatal("duplicate public head", row.Key)
		}
		result[row.Key] = row
	}
	return result
}

func checkNative(t *testing.T, e, unverified *worker.Engine, f Fixture, p Paths, published map[asset.Key]asset.Revision, corrupt *atomic.Bool) {
	t.Helper()
	if len(heads(t, unverified.Client)) != 0 {
		t.Fatal("unverified export probe needs a fresh allocation, not a conflicting head")
	}
	before := f.Native.Calls(t)
	f.Native.SetVersion(t, "unsupported-contract-fixture")
	root, err := os.OpenRoot(p.Workspace)
	must(t, err)
	info, err := adapter.Inspect(t.Context(), f.Adapter, root, f.Native.Executable)
	must(t, root.Close())
	must(t, err)
	if info.Tested || info.RuntimeVersion != "unsupported-contract-fixture" {
		t.Fatal("version observation concealed the incompatible CLI")
	}
	if unverified.Sync(t.Context()) == nil {
		t.Fatal("unverified native export succeeded")
	}
	// Import uses the original allocation's real session. A missing reference
	// must not conceal an accidentally disabled importer version gate either.
	bad := *e
	bad.Config.RuntimeVersion = ""
	dest := filepath.Join(t.TempDir(), "unverified-home")
	if bad.RestoreNative(t.Context(), f.SessionID, 1, dest) == nil {
		t.Fatal("unverified native import succeeded")
	}
	absent(t, dest)
	after := f.Native.Calls(t)
	if after.Version <= before.Version || after.List != before.List || after.Export != before.Export || after.Import != before.Import || len(heads(t, unverified.Client)) != 0 || !reflect.DeepEqual(published, heads(t, e.Client)) {
		t.Fatal("unverified CLI ran native commands or published an asset", before, after)
	}
	f.Native.SetVersion(t, f.Adapter.Descriptor().TestedRuntime)
	corrupt.Store(true)
	dest = filepath.Join(t.TempDir(), "corrupt-native-home")
	if e.RestoreNative(t.Context(), f.SessionID, 1, dest) == nil {
		t.Fatal("native importer accepted unverified downloaded bytes")
	}
	if corrupt.Load() {
		t.Fatal("native restore failed before the checksum fault was exercised")
	}
	absent(t, dest)
	if f.Native.Calls(t).Import != before.Import {
		t.Fatal("native import was invoked before checksum verification")
	}
}

func scopedClient(t *testing.T, endpoint string, secret []byte, scope asset.Scope) *platform.Client {
	t.Helper()
	token, err := platform.Issue(secret, platform.Grant{Scope: scope, Read: true, Write: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	must(t, err)
	client, err := platform.NewClient(endpoint, token)
	must(t, err)
	return client
}

func assertExactFiles(t *testing.T, dest string, expected map[string][]byte) {
	t.Helper()
	if _, raw := expected[""]; raw {
		info, err := os.Lstat(dest)
		must(t, err)
		if !info.Mode().IsRegular() {
			t.Fatal("raw session was not restored as one regular file")
		}
	} else {
		files := make(map[string]bool)
		must(t, filepath.WalkDir(dest, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return errors.New("unexpected non-regular restored file")
			}
			relative, err := filepath.Rel(dest, name)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(relative)] = true
			return nil
		}))
		if len(files) != len(expected) {
			t.Fatal("restored file set differs from original", len(files), len(expected))
		}
		for name := range expected {
			if !files[name] {
				t.Fatal("missing original restored file", name)
			}
		}
	}
	assertFiles(t, dest, expected)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func write(t *testing.T, name string, b []byte) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(name), 0700))
	must(t, os.WriteFile(name, b, 0600))
}
func absent(t *testing.T, name string) {
	t.Helper()
	if _, err := os.Lstat(name); !os.IsNotExist(err) {
		t.Fatal("failed restore published its destination", err)
	}
}
func assertFiles(t *testing.T, dest string, files map[string][]byte) {
	t.Helper()
	for relative, want := range files {
		name := dest
		if relative != "" {
			name = filepath.Join(dest, relative)
		}
		file, err := os.Open(name)
		must(t, err)
		got, err := io.ReadAll(io.LimitReader(file, int64(len(want))+1))
		must(t, err)
		must(t, file.Close())
		if !bytes.Equal(got, want) {
			t.Fatalf("restored bytes changed at relative path %q", relative)
		}
	}
}
