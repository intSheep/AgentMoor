package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"agentmoor/internal/adapter"
	"agentmoor/internal/task"
)

var supported = []string{"claude-code", "codex", "gemini-cli", "opencode"}

func command[T any](t *testing.T, args ...string) T {
	t.Helper()
	var out bytes.Buffer
	if err := run(t.Context(), args, &out); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	var result T
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("invalid command output %q: %v", out.String(), err)
	}
	return result
}

func TestBuiltinAdapters(t *testing.T) {
	var ids []string
	for _, d := range command[[]adapter.Descriptor](t, "adapters") {
		modes := []string{task.TextTurn, task.ContinueTurn}
		if d.ID == "codex" {
			modes = append(modes, task.FileTurn)
		}
		if !slices.Equal(d.TaskModes, modes) {
			t.Errorf("%s task modes = %v", d.ID, d.TaskModes)
		}
		ids = append(ids, d.ID)
	}
	if !slices.Equal(ids, supported) {
		t.Fatalf("adapters = %v", ids)
	}
}

func TestFixtureCommands(t *testing.T) {
	for _, tc := range []struct{ id, marker, manifest string }{
		{"claude-code", ".claude/", ".claude/skills/greeting/SKILL.md"},
		{"codex", ".codex/", ".agents/skills/greeting/SKILL.md"},
		{"gemini-cli", ".gemini/", ".gemini/skills/greeting/SKILL.md"},
		{"opencode", ".opencode/", ".opencode/skills/greeting/SKILL.md"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			dir := filepath.Join("..", "..", "testdata", tc.id)
			matches := command[[]match](t, "detect", dir)
			if len(matches) != 1 || matches[0].Adapter != tc.id || !slices.Equal(matches[0].Evidence, []string{tc.marker}) {
				t.Fatalf("matches = %v", matches)
			}
			skills := command[[]adapter.SkillRef](t, "skills", tc.id, dir)
			if !slices.Equal(skills, []adapter.SkillRef{{Manifest: tc.manifest}}) {
				t.Fatalf("skills = %v", skills)
			}
		})
	}
}

func TestSharedSkillDirectory(t *testing.T) {
	dir := t.TempDir()
	dirname := filepath.Join(dir, ".agents", "skills", "shared")
	if err := os.MkdirAll(dirname, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"AGENTS.md", ".agents/skills/shared/SKILL.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if matches := command[[]match](t, "detect", dir); matches == nil || len(matches) != 0 {
		t.Fatalf("unexpected matches from shared markers: %v", matches)
	}
	for _, id := range []string{"codex", "gemini-cli", "opencode"} {
		skills := command[[]adapter.SkillRef](t, "skills", id, dir)
		if !slices.Equal(skills, []adapter.SkillRef{{Manifest: ".agents/skills/shared/SKILL.md"}}) {
			t.Fatalf("%s skills = %v", id, skills)
		}
	}
	if skills := command[[]adapter.SkillRef](t, "skills", "claude-code", dir); skills == nil || len(skills) != 0 {
		t.Fatalf("claude skills = %v", skills)
	}
	for _, marker := range []string{"CLAUDE.md", "GEMINI.md", "opencode.jsonc"} {
		if err := os.WriteFile(filepath.Join(dir, marker), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var ids []string
	for _, m := range command[[]match](t, "detect", dir) {
		ids = append(ids, m.Adapter)
	}
	if !slices.Equal(ids, []string{"claude-code", "gemini-cli", "opencode"}) {
		t.Fatalf("shared workspace matches = %v", ids)
	}
}

func TestEmptyWorkspace(t *testing.T) {
	dir := t.TempDir()
	for _, id := range supported {
		got := command[[]adapter.SkillRef](t, "skills", id, dir)
		if got == nil || len(got) != 0 {
			t.Fatalf("%s skills = %v, want empty JSON array", id, got)
		}
	}
}

func TestInitializePreservesFirstKey(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	cmd := []string{"init", "--data", dir}
	if err := run(t.Context(), cmd, &out); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "signing.key")
	first, err := os.ReadFile(path)
	if err != nil || len(first) != 32 {
		t.Fatal("incomplete key", err)
	}
	if err := run(t.Context(), cmd, &out); err == nil {
		t.Fatal("initialized twice")
	}
	second, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("first key changed", err)
	}
}
