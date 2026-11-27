package worker

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intSheep/AgentMoor/internal/adapters/codex"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/platform"
	"github.com/intSheep/AgentMoor/internal/snapshot"
	"github.com/intSheep/AgentMoor/internal/task"
)

func TestPinnedSkillUsesOriginalVersionInFreshPrivateHomes(t *testing.T) {
	catalog, err := platform.OpenCatalog(t.Context(), t.TempDir(), "", platform.DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	secret := bytes.Repeat([]byte{7}, 32)
	scope := asset.Scope{Tenant: "team", Project: "reports", Runtime: "skills", Adapter: "codex"}
	srv := httptest.NewServer((&platform.API{Catalog: catalog, Secret: secret, Temp: t.TempDir()}).Handler())
	defer srv.Close()
	token, err := platform.Issue(secret, platform.Grant{Scope: scope, Read: true, Write: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	client, err := platform.NewClient(srv.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Scope: scope, Endpoint: srv.URL, Workspace: t.TempDir(), StateDir: t.TempDir()}
	e := Engine{Config: config, Adapter: codex.New(), Client: client}
	put(t, config.Workspace, ".agents/skills/native/SKILL.md", "---\nname: native\n---\nNative content stays untouched.")
	packageDir := t.TempDir()
	manifest := "---\nname: demo\ndescription: pinned rule\n---\nUse the original rule.\n"
	put(t, packageDir, "SKILL.md", manifest)
	put(t, packageDir, "scripts/check.py", "print('original script')\n")
	put(t, packageDir, "references/rule.txt", "original reference\n")
	if err := os.Chmod(filepath.Join(packageDir, "scripts/check.py"), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(packageDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	key := asset.Key{Kind: "skill", ID: "catalog/skills/demo"}
	publish := func(base int64) task.Ref {
		t.Helper()
		blob, err := snapshot.Tree(t.Context(), root, []snapshot.Entry{{Source: ".", Name: "."}}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer blob.Remove()
		meta := asset.Metadata{Key: key, AdapterVersion: "fixture", Format: "skill-tar.v1", MediaType: "application/x-tar", Name: "demo", Origin: "platform-managed", Completeness: "persisted-files", SHA256: blob.SHA256, Size: blob.Size}
		version, err := client.Publish(t.Context(), asset.Change{Metadata: meta, Base: base}, blob.Path)
		if err != nil {
			t.Fatal(err)
		}
		return task.Ref{Key: key, Version: version, SHA256: blob.SHA256, ContentPath: key.ContentPath(version)}
	}
	first := publish(0)
	put(t, packageDir, "references/rule.txt", "new reference that must not replace the pin\n")
	second := publish(1)
	pins := []task.SkillPin{{Source: first, Name: "demo"}}
	var firstHome string
	for i := 0; i < 2; i++ {
		home := t.TempDir()
		if i == 0 {
			firstHome = home
		}
		loaded, err := e.installTaskSkills(t.Context(), task.Environment{Workspace: config.Workspace, Home: home}, pins)
		if err != nil || len(loaded) != 1 {
			t.Fatal(loaded, err)
		}
		for name, want := range map[string]string{"SKILL.md": manifest, "scripts/check.py": "print('original script')\n", "references/rule.txt": "original reference\n"} {
			raw, err := os.ReadFile(filepath.Join(filepath.Dir(loaded[0].Manifest), name))
			if err != nil || string(raw) != want {
				t.Fatalf("%s: %q %v", name, raw, err)
			}
		}
		info, err := os.Stat(filepath.Join(filepath.Dir(loaded[0].Manifest), "scripts/check.py"))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal(info, err)
		}
	}
	if _, err := e.installTaskSkills(t.Context(), task.Environment{Workspace: config.Workspace, Home: firstHome}, pins); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatal("existing private Skill overwritten", err)
	}
	for _, tc := range []struct {
		name string
		pin  task.SkillPin
	}{
		{"wrong checksum", task.SkillPin{Source: task.Ref{Key: first.Key, Version: 1, SHA256: strings.Repeat("0", 64), ContentPath: first.ContentPath}, Name: "demo"}},
		{"manifest identity", task.SkillPin{Source: first, Name: "different"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := e.installTaskSkills(t.Context(), task.Environment{Workspace: config.Workspace, Home: t.TempDir()}, []task.SkillPin{tc.pin}); err == nil {
				t.Fatal("invalid Skill installed")
			}
		})
	}
	outside := t.TempDir()
	e.Adapter = escapingSkillInstaller{Adapter: codex.New(), target: outside}
	if _, err := e.installTaskSkills(t.Context(), task.Environment{Workspace: config.Workspace, Home: t.TempDir()}, pins); err == nil {
		t.Fatal("adapter escaped private home")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	items, err := catalog.Heads(t.Context(), scope)
	if err != nil || len(items) != 1 || items[0].Version != second.Version || items[0].Origin != "platform-managed" {
		t.Fatal("installation changed source assets", items, err)
	}
	raw, err := os.ReadFile(filepath.Join(config.Workspace, ".agents/skills/native/SKILL.md"))
	if err != nil || !strings.Contains(string(raw), "Native content stays untouched") {
		t.Fatal(string(raw), err)
	}
}

type escapingSkillInstaller struct {
	codex.Adapter
	target string
}

func (a escapingSkillInstaller) SkillDirectory(task.Environment, string) (string, error) {
	return a.target, nil
}
