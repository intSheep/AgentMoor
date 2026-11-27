package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentmoor/internal/platform"
	"golang.org/x/sys/unix"
)

func authCLI(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := run(t.Context(), args, &out); err != nil {
		t.Fatal("authentication command failed", err)
	}
	return strings.TrimSpace(out.String())
}

func authIssueCLI(t *testing.T, policy string, extra ...string) string {
	t.Helper()
	args := []string{"issue", "--auth-policy", policy, "--tenant", "tenant", "--project", "project", "--runtime", "runtime", "--adapter", "codex"}
	return authCLI(t, append(args, extra...)...)
}

type authReadyWriter chan string

func (w authReadyWriter) Write(raw []byte) (int, error) {
	line := strings.TrimSpace(string(raw))
	if strings.HasPrefix(line, "listening ") {
		w <- strings.TrimPrefix(line, "listening ")
	}
	return len(raw), nil
}

func authCLIRequest(t *testing.T, endpoint, token string, want int) {
	t.Helper()
	request, err := http.NewRequest("GET", endpoint+"/v1/context", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	io.Copy(io.Discard, response.Body)
	if response.StatusCode != want {
		t.Fatalf("credential response %d, want %d", response.StatusCode, want)
	}
}

func TestAuthCLILivePolicyWithoutSigningKey(t *testing.T) {
	legacyData := t.TempDir()
	authCLI(t, "init", "--data", legacyData)
	scopeArgs := []string{"--tenant", "tenant", "--project", "project", "--runtime", "runtime", "--adapter", "codex"}
	legacy := authCLI(t, append([]string{"issue", "--data", legacyData}, scopeArgs...)...)
	policy := filepath.Join(t.TempDir(), "auth.json")
	authCLI(t, "auth-init", "--policy", policy, "--from-key", filepath.Join(legacyData, "signing.key"), "--key-id", "original")
	first, second := authIssueCLI(t, policy), authIssueCLI(t, policy)
	if first == second {
		t.Fatal("CLI issued identical credential identities")
	}
	executor := authIssueCLI(t, policy, "--execute")
	grant, err := platform.AuthenticateAuthPolicy(policy, executor, time.Now())
	if err != nil || !grant.Read || !grant.Write || !grant.Execute || grant.Submit || grant.Collect {
		t.Fatal("policy CLI changed role grants", err)
	}

	data := filepath.Join(t.TempDir(), "metadata")
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(authReadyWriter, 1)
	result := make(chan error, 1)
	go func() {
		result <- run(ctx, []string{"serve", "--data", data, "--auth-policy", policy, "--listen", "127.0.0.1:0"}, ready)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-result:
			if err != nil {
				t.Error("policy server failed", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("policy server did not shut down")
		}
	})
	var endpoint string
	select {
	case addr := <-ready:
		endpoint = "http://" + addr
	case err := <-result:
		result <- err
		t.Fatal("policy server failed to start", err)
	case <-time.After(5 * time.Second):
		t.Fatal("policy server did not start")
	}
	if _, err := os.Stat(filepath.Join(data, "signing.key")); !os.IsNotExist(err) {
		t.Fatal("policy server unexpectedly required a metadata signing.key", err)
	}
	for _, token := range []string{legacy, first, second} {
		authCLIRequest(t, endpoint, token, 200)
	}
	authCLI(t, "auth-rotate", "--policy", policy, "--key-id", "replacement")
	replacement := authIssueCLI(t, policy)
	for _, token := range []string{legacy, first, second, replacement} {
		authCLIRequest(t, endpoint, token, 200)
	}
	file := filepath.Join(t.TempDir(), "credential.token")
	if err := os.WriteFile(file, []byte(first+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		authCLI(t, "auth-revoke", "--policy", policy, "--token-file", file)
	}
	authCLIRequest(t, endpoint, first, 401)
	authCLIRequest(t, endpoint, second, 200)
	if err := os.WriteFile(file, []byte(legacy+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	authCLI(t, "auth-revoke", "--policy", policy, "--token-file", file)
	authCLIRequest(t, endpoint, legacy, 401)
	authCLIRequest(t, endpoint, second, 200)
	authCLI(t, "auth-retire", "--policy", policy, "--key-id", "original")
	authCLIRequest(t, endpoint, second, 401)
	authCLIRequest(t, endpoint, replacement, 200)
}

func TestAuthCLIPrivateInputsAndFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	message := authCLI(t, "auth-init", "--policy", path)
	if !strings.Contains(message, "active key key-") {
		t.Fatal("fresh policy did not report a safe generated key ID")
	}
	policy, err := platform.LoadAuthPolicy(path)
	if err != nil || policy.Keys[0].Legacy {
		t.Fatal("fresh policy implicitly enabled legacy verification", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secretMarker := "SYNTHETIC-CREDENTIAL-MUST-NOT-APPEAR"
	bad := filepath.Join(t.TempDir(), "bad.token")
	if err := os.WriteFile(bad, []byte(secretMarker), 0600); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(t.TempDir(), "large.token")
	if err := os.WriteFile(oversized, bytes.Repeat([]byte("x"), 4099), 0600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"auth-init", "--policy", path},
		{"auth-revoke", "--policy", path, "--token-file", bad},
		{"auth-revoke", "--policy", path, "--token-file", oversized},
		{"auth-revoke", "--policy", path, "--token-file", fifo},
		{"auth-revoke", "--policy", path, "--token", secretMarker},
		{"auth-retire", "--policy", path},
		{"auth-retire", "--policy", path, "--key-id", policy.Active},
		{"auth-rotate", "--policy", path, "--key-id", "../invalid"},
		{"issue", "--auth-policy", path, "--tenant", "tenant"},
	} {
		var out bytes.Buffer
		err := run(t.Context(), args, &out)
		if err == nil {
			t.Fatal("invalid authentication command unexpectedly succeeded")
		}
		if strings.Contains(err.Error(), secretMarker) || strings.Contains(out.String(), secretMarker) {
			t.Fatal("command error or output leaked credential contents")
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatal("failed management command changed the policy", readErr)
		}
	}
	badKey := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(badKey, bytes.Repeat([]byte{7}, 33), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(t.Context(), []string{"auth-init", "--policy", filepath.Join(t.TempDir(), "new.json"), "--from-key", badKey}, &out); err == nil {
		t.Fatal("oversized legacy key imported")
	}
	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, []byte(secretMarker), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"serve", "--data", t.TempDir(), "--auth-policy", broken}, &out); err == nil || strings.Contains(err.Error(), secretMarker) {
		t.Fatal("broken policy did not fail startup without secrets")
	}
}
