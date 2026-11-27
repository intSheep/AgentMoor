package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/adapters/codex"
)

func TestGuestRunCLIRejectsInvalidHandoffBeforeNativeAndRestoresEnvironment(t *testing.T) {
	for _, field := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_MODEL", "CODEX_FILE_TOOL"} {
		t.Setenv(field, "synthetic-previous-"+field)
	}
	for _, field := range []string{"AGENTMOOR_TOKEN", "AGENTMOOR_TOKEN_FILE", "AGENTMOOR_POSTGRES_DSN", "AGENTMOOR_S3_ENDPOINT", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		t.Setenv(field, "")
	}
	for _, condition := range []string{"bad-digest", "bad-envelope", "wrong-adapter", "missing-model"} {
		t.Run(condition, func(t *testing.T) {
			base := t.TempDir()
			capDir := filepath.Join(base, "capabilities")
			if err := os.Mkdir(capDir, 0700); err != nil {
				t.Fatal(err)
			}
			for _, purpose := range []string{"input", "output", "model"} {
				if err := os.WriteFile(filepath.Join(capDir, purpose), []byte("sbx1."+strings.Repeat("A", 43)), 0400); err != nil {
					t.Fatal(err)
				}
			}
			var requests atomic.Int32
			packet := []byte("deliberately malformed native-guest envelope")
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != "GET" || r.URL.Path != "/input" {
					t.Error("invalid handoff attempted output or model access")
					w.WriteHeader(403)
					return
				}
				_, _ = w.Write(packet)
			}))
			defer srv.Close()
			ca := filepath.Join(base, "ca.pem")
			if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(base, "must-not-run")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'called' > \"$0.invocations\"\nexit 99\n"), 0700); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(packet)
			digest := hex.EncodeToString(hash[:])
			id, model := "codex", "fixture-model"
			switch condition {
			case "bad-digest":
				digest = strings.Repeat("0", 64)
			case "wrong-adapter":
				id = "opencode"
			case "missing-model":
				model = ""
			}
			root := filepath.Join(base, "run")
			args := []string{"guest-run", "--adapter", id, "--model", model, "--runtime-cli", executable, "--root", root,
				"--gateway", srv.URL, "--ca-file", ca, "--capabilities", capDir, "--input-sha256", digest,
				"--input-size", strconv.Itoa(len(packet)), "--deadline", time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
			registry, err := adapter.NewRegistry(codex.New())
			if err != nil {
				t.Fatal(err)
			}
			if err = guestRunCommand(t.Context(), args, registry); err == nil {
				t.Fatal("invalid guest-run unexpectedly succeeded")
			}
			if _, err = os.Stat(executable + ".invocations"); !os.IsNotExist(err) {
				t.Fatal("invalid handoff started a native command", err)
			}
			if _, err = os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("invalid handoff retained a guest root", err)
			}
			want := int32(1)
			if condition == "wrong-adapter" || condition == "missing-model" {
				want = 0
			}
			if requests.Load() != want {
				t.Fatalf("unexpected requests: %d", requests.Load())
			}
			for _, field := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_MODEL", "CODEX_FILE_TOOL"} {
				if os.Getenv(field) != "synthetic-previous-"+field {
					t.Fatal("guest command retained a delegated model environment")
				}
			}
		})
	}
}
