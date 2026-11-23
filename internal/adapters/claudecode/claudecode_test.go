package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectScope(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.MkdirAll(".claude/skills/group/nested", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(".claude/skills/group/nested/SKILL.md", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".claude/skills/linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "SKILL.md"), filepath.Join(dir, ".claude/skills/group/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	got, err := New().DiscoverSkills(t.Context(), root)
	if err != nil || len(got) != 0 {
		t.Fatalf("skills = %v, error = %v", got, err)
	}
}
