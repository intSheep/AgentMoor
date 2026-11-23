package workspace_test

import (
	"context"
	"errors"
	"os"
	"path"
	"slices"
	"testing"

	"agentmoor/internal/adapter"
	"agentmoor/internal/adapters/gemini"
	"agentmoor/internal/adapters/opencode"
)

func TestManifestConventions(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{
		".agents/skills/review.md", ".agents/skills/shared/SKILL.md",
		".claude/skills/shared/SKILL.md", ".gemini/skills/SKILL.md",
		".gemini/skills/shared/SKILL.md", ".gemini/skills/team/nested/SKILL.md",
		".opencode/skills/team/release/SKILL.md", ".opencode/skills/team/release/example/SKILL.md",
		".opencode/skills/.cache/SKILL.md", ".opencode/skills/node_modules/some/SKILL.md",
		".opencode/skills/one/two/three/four/five/six/SKILL.md",
		".opencode/skills/a/b/c/d/e/f/g/SKILL.md",
	} {
		if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		id         string
		discoverer adapter.SkillDiscoverer
		want       []string
	}{
		{"gemini-cli", gemini.New(), []string{
			".agents/skills/shared/SKILL.md", ".gemini/skills/SKILL.md", ".gemini/skills/shared/SKILL.md",
		}},
		{"opencode", opencode.New(), []string{
			".opencode/skills/one/two/three/four/five/six/SKILL.md",
			".opencode/skills/team/release/SKILL.md", ".claude/skills/shared/SKILL.md",
			".agents/skills/review.md", ".agents/skills/shared/SKILL.md",
		}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			skills, err := tc.discoverer.DiscoverSkills(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, s := range skills {
				paths = append(paths, s.Manifest)
			}
			if !slices.Equal(paths, tc.want) {
				t.Fatalf("manifests = %v, want %v", paths, tc.want)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := tc.discoverer.DiscoverSkills(ctx, root); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled discovery returned %v", err)
			}
		})
	}
}
