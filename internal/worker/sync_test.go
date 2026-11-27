package worker

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/adapters/claudecode"
	"github.com/intSheep/AgentMoor/internal/adapters/codex"
	"github.com/intSheep/AgentMoor/internal/adapters/gemini"
	"github.com/intSheep/AgentMoor/internal/adapters/opencode"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/platform"
)

const fixtureID = "e05387eb-0457-4a25-ab65-21669e60923b"

func put(t *testing.T, dir, name, value string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFourRuntimeRecovery(t *testing.T) {
	for _, tc := range []struct {
		a                        adapter.Adapter
		skill, session, data, id string
	}{
		{claudecode.New(), ".claude/skills/greet/SKILL.md", fixtureID + ".jsonl", `{"type":"user","sessionId":"` + fixtureID + `"}`, fixtureID},
		{codex.New(), ".agents/skills/greet/SKILL.md", "2026/11/22/rollout-test.jsonl", `{"type":"session_meta","payload":{"id":"` + fixtureID + `"}}`, fixtureID},
		{gemini.New(), ".gemini/skills/greet/SKILL.md", "session-today.jsonl", `{"sessionId":"` + fixtureID + `","projectHash":"demo"}`, fixtureID},
		{opencode.New(), ".opencode/skills/greet/SKILL.md", "export.json", `{"info":{"id":"ses_test123"},"messages":[]}`, "ses_test123"},
	} {
		t.Run(tc.a.Descriptor().ID, func(t *testing.T) {
			db, err := platform.OpenCatalog(t.Context(), t.TempDir(), "", platform.DiskObjects{Dir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			secret := bytes.Repeat([]byte{1}, 32)
			api := (&platform.API{Catalog: db, Secret: secret, Temp: t.TempDir()}).Handler()
			var drop atomic.Bool
			drop.Store(true)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/commit") && drop.CompareAndSwap(true, false) {
					api.ServeHTTP(httptest.NewRecorder(), r)
					http.Error(w, "simulated lost acknowledgement", 503)
					return
				}
				api.ServeHTTP(w, r)
			}))
			defer srv.Close()
			s := asset.Scope{Tenant: "team", Project: "demo", Runtime: "main", Adapter: tc.a.Descriptor().ID}
			token, err := platform.Issue(secret, platform.Grant{Scope: s, Read: true, Write: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			client, err := platform.NewClient(srv.URL, token)
			if err != nil {
				t.Fatal(err)
			}
			c := Config{Scope: s, Workspace: t.TempDir(), Sessions: t.TempDir(), StateDir: t.TempDir(), Artifacts: "out", Endpoint: srv.URL}
			c.ManagedSkills = []string{tc.skill}
			put(t, c.Workspace, tc.skill, "---\nname: friendly\nmetadata:\n  version: '2.0'\ndependencies: [helper]\n---\nSkill body")
			put(t, c.Workspace, filepath.Join(filepath.Dir(tc.skill), "helper"), "support")
			put(t, c.Workspace, "out/result.txt", "first")
			put(t, c.Sessions, tc.session, tc.data)
			sidecars := s.Adapter == "claude-code" || s.Adapter == "gemini-cli"
			if sidecars {
				put(t, c.Sessions, fixtureID+"/subagents/child.jsonl", "child transcript")
			}
			e := Engine{c, tc.a, client}
			if err := e.Sync(t.Context()); err == nil {
				t.Fatal("lost acknowledgement not reported")
			}
			j, err := load(c)
			if err != nil || j.Pending == nil || j.State != "failed" {
				t.Fatal(j, err)
			}
			if err := e.Sync(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := e.Sync(t.Context()); err != nil {
				t.Fatal(err)
			}
			items, err := db.List(t.Context(), s)
			if err != nil || len(items) != 4 {
				t.Fatal(items, err)
			}
			for _, item := range items {
				if item.Kind == "skill" && (item.Name != "friendly" || item.DeclaredVersion != "2.0" || item.Origin != "platform-managed" || len(item.Dependencies) != 1) {
					t.Fatal("skill metadata", item)
				}
			}
			put(t, c.Workspace, "out/result.txt", "second")
			if err := e.Sync(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err = os.RemoveAll(c.Workspace); err != nil {
				t.Fatal(err)
			}
			if err = os.RemoveAll(c.Sessions); err != nil {
				t.Fatal(err)
			}
			all := filepath.Join(t.TempDir(), "history")
			if err = e.RestoreSessions(t.Context(), all); err != nil {
				t.Fatal(err)
			}
			if sidecars {
				b, err := os.ReadFile(filepath.Join(all, fixtureID, "subagents", "child.jsonl"))
				if err != nil || string(b) != "child transcript" {
					t.Fatal("lost sidecar", err)
				}
			}
			for _, tc2 := range []struct {
				kind, id, read, want string
				version              int64
			}{
				{"workspace", "root", "out/result.txt", "second", 0}, {"skill", tc.skill, "helper", "support", 1}, {"artifact", "out/result.txt", "", "first", 1}, {"artifact", "out/result.txt", "", "second", 2}, {"session", tc.id, tc.session, tc.data, 1},
			} {
				dst := filepath.Join(t.TempDir(), "restored")
				if err = e.Restore(t.Context(), asset.Key{Kind: tc2.kind, ID: tc2.id}, tc2.version, dst); err != nil {
					t.Fatal(err)
				}
				name := dst
				if tc2.read != "" && !(tc2.kind == "session" && s.Adapter == "opencode") {
					name = filepath.Join(dst, tc2.read)
				}
				b, err := os.ReadFile(name)
				if err != nil || string(b) != tc2.want {
					t.Fatalf("%s: %q %v", name, b, err)
				}
			}
		})
	}
}

func TestWatchAndConflict(t *testing.T) {
	t.Run("watch", func(t *testing.T) { watchAndConflict(t, true) })
	t.Run("periodic", func(t *testing.T) { watchAndConflict(t, false) })
}

func watchAndConflict(t *testing.T, events bool) {
	dir := t.TempDir()
	db, err := platform.OpenCatalog(t.Context(), dir, "", platform.DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	secret := bytes.Repeat([]byte{2}, 32)
	s := asset.Scope{Tenant: "team", Project: "demo", Runtime: "main", Adapter: "codex"}
	tok, _ := platform.Issue(secret, platform.Grant{Scope: s, Read: true, Write: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	srv := httptest.NewServer((&platform.API{Catalog: db, Secret: secret, Temp: t.TempDir()}).Handler())
	defer srv.Close()
	client, err := platform.NewClient(srv.URL, tok)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Scope: s, Workspace: t.TempDir(), StateDir: t.TempDir(), Endpoint: srv.URL}
	e := Engine{config, codex.New(), client}
	ctx, cancel := context.WithCancel(t.Context())
	var logs bytes.Buffer
	done := make(chan error, 1)
	interval := time.Hour
	if !events {
		interval = 40 * time.Millisecond
	}
	go func() { done <- e.Watch(ctx, interval, events, &logs) }()
	defer cancel()
	wait := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			items, err := db.List(t.Context(), s)
			if err == nil && len(items) >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("watch did not publish", n)
	}
	wait(1)
	put(t, config.Workspace, "hello", "first")
	wait(2)
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	other := config
	other.Workspace = t.TempDir()
	other.StateDir = t.TempDir()
	put(t, other.Workspace, "hello", "divergent")
	writer := Engine{other, codex.New(), client}
	if err = writer.Sync(t.Context()); !errors.Is(err, platform.ErrConflict) {
		t.Fatal("expected conflict", err)
	}
	if err = writer.Rebase(t.Context(), 1); err == nil {
		t.Fatal("accepted stale review")
	}
	if err = writer.Rebase(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	if err = writer.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A new worker sees the original empty workspace again. Its would-be
	// idempotency key matches the first publication, but the head is now v3.
	stale := config
	stale.Workspace, stale.StateDir = t.TempDir(), t.TempDir()
	restarted := Engine{stale, codex.New(), client}
	for range 2 {
		if err := restarted.Sync(t.Context()); !errors.Is(err, platform.ErrConflict) {
			t.Fatalf("historical replay hid a divergent head: %v", err)
		}
	}
	fresh := other
	fresh.StateDir = t.TempDir()
	adopt := Engine{fresh, codex.New(), client}
	if err := adopt.Sync(t.Context()); err != nil {
		t.Fatalf("new worker could not adopt identical platform head: %v", err)
	}
	revs, err := db.List(t.Context(), s)
	if err != nil || len(revs) != 3 {
		t.Fatalf("unexpected publication after adoption: %v %v", revs, err)
	}

}
