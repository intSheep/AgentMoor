package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/worker"
)

func TestCLIUntrustedTaskPolicyFailsBeforeCredentialsOrExecution(t *testing.T) {
	for _, continuation := range []bool{false, true} {
		name := "initial-task"
		if continuation {
			name = "follow-up"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "CLI reached the platform", 500)
			}))
			defer server.Close()
			executable := filepath.Join(base, "must-not-start")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'invoked\\n' >> \"$0.invocations\"\nexit 99\n"), 0700); err != nil {
				t.Fatal(err)
			}
			config := worker.Config{Scope: asset.Scope{Tenant: "fixture", Project: "policy", Runtime: "report", Adapter: "codex"},
				Endpoint: server.URL, Workspace: filepath.Join(base, "work"), StateDir: filepath.Join(base, "state"),
				RuntimeCLI: executable, ExecutionPolicy: worker.ExecutionUntrustedIsolated}
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(base, "worker.json")
			if err = os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			// The gate must run before even reading an execution credential.
			t.Setenv("AGENTMOOR_TOKEN_FILE", filepath.Join(base, "absent-token"))
			args := []string{"run-task", "--config", file, "--wait", "0s"}
			if continuation {
				args = append(args, "--turn", uuid.NewString())
			}
			var output bytes.Buffer
			if err = run(t.Context(), args, &output); !errors.Is(err, worker.ErrIsolationUnavailable) {
				t.Fatal("CLI must preserve the stable isolated-provider error", err)
			}
			if requests.Load() != 0 || output.Len() != 0 {
				t.Fatal("untrusted CLI request contacted the platform or emitted success", requests.Load(), output.String())
			}
			for _, path := range []string{config.Workspace, config.StateDir, executable + ".invocations"} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("untrusted CLI request started execution or created state", path, err)
				}
			}
		})
	}
}
