package notebook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intSheep/AgentMoor/sdk"
	"github.com/intSheep/AgentMoor/sdk/contracttest"
)

const originalSession = "{\n  \"session_id\": \"notebook_session_1\",\n  \"messages\": [\"original notebook history\"],\n  \"opaque\": {\"keep\":true}\n}\n"

func writeFile(t *testing.T, directory, name, content string) {
	t.Helper()
	file := filepath.Join(directory, name)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func openRoot(t *testing.T, directory string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func TestNotebookRegistryAndDiscovery(t *testing.T) {
	a := Adapter{}
	registry, err := sdk.NewRegistry(a)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := registry.Lookup(a.Descriptor().ID)
	if err != nil || registered.Descriptor().ID != "notebook-example" {
		t.Fatal("example adapter did not register", err)
	}
	if err := contracttest.ValidateDescriptor(a); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	root := openRoot(t, directory)
	writeFile(t, directory, "skills/greet/SKILL.md", "shared Skill directory alone is not runtime evidence\n")
	evidence, err := a.Detect(t.Context(), root)
	if err != nil || len(evidence) != 0 {
		t.Fatal("shared Skill directory identified a runtime", evidence, err)
	}
	writeFile(t, directory, marker, "synthetic notebook\n")
	evidence, err = a.Detect(t.Context(), root)
	if err != nil || len(evidence) != 1 || evidence[0] != marker {
		t.Fatal("assigned notebook marker was not discovered", evidence, err)
	}
	writeFile(t, directory, skillManifest, "original Skill body\n")
	skills, err := a.DiscoverSkills(t.Context(), root)
	if err != nil || len(skills) != 1 || skills[0].Manifest != skillManifest {
		t.Fatal("assigned notebook Skill was not discovered", skills, err)
	}
}

func TestNotebookAssetContract(t *testing.T) {
	contracttest.Run(t, contracttest.Case{Name: "notebook-files", Prepare: func(t *testing.T, paths contracttest.Paths) contracttest.Fixture {
		writeFile(t, paths.Workspace, marker, "synthetic notebook\n")
		writeFile(t, paths.Sessions, "original.json", originalSession)
		return contracttest.Fixture{Adapter: Adapter{}, Mechanism: contracttest.Filesystem,
			SessionID: "notebook_session_1", SessionFormat: sessionFormat, SkillManifest: skillManifest,
			SessionFiles: map[string][]byte{"": []byte(originalSession)}}
	}})
}

func TestNotebookRejectsInvalidSessions(t *testing.T) {
	for name, content := range map[string]string{
		"invalid-json": "{",
		"missing-id":   `{ "messages": [] }`,
		"invalid-id":   `{ "session_id": "../another" }`,
		"oversized":    `{"session_id":"too_large","padding":"` + strings.Repeat("x", maxSession) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			writeFile(t, directory, "original.json", content)
			if _, err := (Adapter{}).Sessions(t.Context(), openRoot(t, directory)); err == nil {
				t.Fatal("invalid Session was accepted")
			}
		})
	}
	t.Run("inside-symlink", func(t *testing.T) {
		directory := t.TempDir()
		writeFile(t, directory, "original.json", originalSession)
		if err := os.Symlink("original.json", filepath.Join(directory, "linked.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := (Adapter{}).Sessions(t.Context(), openRoot(t, directory)); err == nil {
			t.Fatal("Session symlink inside the assigned root was accepted")
		}
	})
	t.Run("outside-symlink", func(t *testing.T) {
		directory, outside := t.TempDir(), t.TempDir()
		writeFile(t, outside, "outside.json", originalSession)
		if err := os.Symlink(filepath.Join(outside, "outside.json"), filepath.Join(directory, "linked.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := (Adapter{}).Sessions(t.Context(), openRoot(t, directory)); err == nil {
			t.Fatal("Session symlink outside the assigned root was accepted")
		}
	})
	t.Run("entry-limit", func(t *testing.T) {
		directory := t.TempDir()
		for index := 0; index <= maxEntries; index++ {
			writeFile(t, directory, strings.Repeat("x", index+1), "unrelated entry\n")
		}
		if _, err := (Adapter{}).Sessions(t.Context(), openRoot(t, directory)); err == nil {
			t.Fatal("Session directory exceeded its declared entry limit")
		}
	})
}

func TestNotebookCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	root := openRoot(t, t.TempDir())
	a := Adapter{}
	if _, err := a.Detect(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatal("Detect ignored cancellation", err)
	}
	if _, err := a.DiscoverSkills(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatal("DiscoverSkills ignored cancellation", err)
	}
	if _, err := a.Sessions(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatal("Sessions ignored cancellation", err)
	}
}
