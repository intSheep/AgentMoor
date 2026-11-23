package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeExportSelectsAssignedWorkspace(t *testing.T) {
	dir, work, other, exported := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	// This small executable stands in for the CLI and returns two real paths.
	// Exit on an attempt to export the session that belongs to the other one.
	path := filepath.Join(dir, "runtime")
	script := `#!/bin/sh
case "$1" in
  session) cat "$0.list" ;;
  export) [ "$2" = "ses_local" ] || exit 3
          [ -z "${AGENTMOOR_TOKEN:-}" ] || exit 4
          echo '{"info":{"id":"ses_local"},"messages":[]}' ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	rows, err := json.Marshal([]map[string]string{{"id": "ses_foreign", "directory": other}, {"id": "ses_local", "directory": work}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".list", rows, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTMOOR_TOKEN", "synthetic-test-token")
	if err := New().ExportNative(t.Context(), path, work, exported); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(exported)
	if err != nil || len(entries) != 1 || entries[0].Name() != "ses_local.json" {
		t.Fatalf("exports = %v, error = %v", entries, err)
	}
}
