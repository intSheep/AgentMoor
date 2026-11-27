// Package codexcase supplies original synthetic Codex bytes to the shared suite.
package codexcase

import (
	"os"
	"path"
	"testing"

	"agentmoor/internal/adapters/codex"
	"agentmoor/internal/worker/contracttest"
)

// New exercises file-backed sessions, without invoking a Codex executable.
func New() contracttest.Case {
	return contracttest.Case{Name: "codex-filesystem", Prepare: func(t *testing.T, paths contracttest.Paths) contracttest.Fixture {
		t.Helper()
		workspace, err := os.OpenRoot(paths.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		defer workspace.Close()
		if err := workspace.MkdirAll(".codex", 0700); err != nil {
			t.Fatal(err)
		}

		const id = "7d65d0f5-5c08-467b-b022-97cfff41e16a"
		// The filename deliberately differs from the header's native Session ID.
		const name = "2026/11/22/rollout-different-file-name.jsonl"
		raw := []byte(`{ "type" : "session_meta", "payload" : { "id" : "` + id + `", "fixture" : "synthetic-conformance" } }` + "\n" +
			`{"type":"event_msg","payload":{"message":"synthetic original bytes: \/ and \u0068","fixture_order":[2,1]}}` + "\n")
		sessions, err := os.OpenRoot(paths.Sessions)
		if err != nil {
			t.Fatal(err)
		}
		defer sessions.Close()
		if err := sessions.MkdirAll(path.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := sessions.WriteFile(name, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return contracttest.Fixture{
			Adapter:       codex.New(),
			Mechanism:     contracttest.Filesystem,
			SessionID:     id,
			SessionFormat: "codex-rollout-jsonl.v1",
			SkillManifest: ".agents/skills/greet/SKILL.md",
			SessionFiles:  map[string][]byte{name: raw},
			Native:        nil,
		}
	}}
}
