package platform

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSandboxClientChecksOuterSealedBytesAndBinaryFailure(t *testing.T) {
	f := newHandoffHTTPFixture(t, false)
	f.prepare(t)
	server := httptest.NewServer(f.api.Handler())
	defer server.Close()
	token, err := Issue(f.secret, Grant{Scope: f.scope, Read: true, Execute: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.SandboxTask(t.Context(), f.route(), "/status", f.address.Attempt)
	if err != nil || status.Run.State != SandboxReady || status.Request.Spec.Instruction != "synthetic handoff only" {
		t.Fatal("cannot read private sandbox status", err)
	}
	reader, err := client.SandboxOutput(t.Context(), f.route(), f.address.Attempt, status.Run.Output)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(raw) != "synthetic sealed result" {
		t.Fatal("complete output differs", err)
	}
	// Equal-size corruption might complete an HTTP Content-Length; checking
	// only the transport EOF or the archive's own hashes is insufficient.
	objects := f.c.objects.(DiskObjects)
	if err = os.WriteFile(filepath.Join(objects.Dir, sandboxObjectKey(f.scope, SandboxOutput, status.Run.Output.SHA256)), bytes.Repeat([]byte{'x'}, int(status.Run.Output.Size)), 0600); err != nil {
		t.Fatal(err)
	}
	reader, err = client.SandboxOutput(t.Context(), f.route(), f.address.Attempt, status.Run.Output)
	if err == nil {
		raw, err = io.ReadAll(reader)
		reader.Close()
	}
	if err == nil || bytes.Contains(raw, []byte("platform operation failed")) {
		t.Fatal("corrupt binary output was accepted or mixed with an API error")
	}
}

func TestSandboxClientRejectsDestinationAndManifestOverrides(t *testing.T) {
	attempt := "1c6912a3-cfc6-4268-a08a-3e6a2edb71de"
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("X-Agentmoor-SHA256", strings.Repeat("0", 64))
		w.Header().Set("X-Agentmoor-Size", "4")
		io.WriteString(w, "body")
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "explicit-synthetic-sdk-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"@other.example/v1/tasks/id/sandbox", "//other.example/v1/tasks/id/sandbox", "/v1/tasks/id/sandbox?attempt=override", "/v1/tasks/id/sandbox#fragment"} {
		if _, err = client.SandboxTask(t.Context(), path, "", attempt); err == nil || calls != 0 {
			t.Fatal("mutable destination reached network", path)
		}
	}
	expected := sandboxTestBlob("body")
	expected.Sealed = true
	if _, err = client.SandboxOutput(t.Context(), "/v1/tasks/id/sandbox", attempt, expected); err == nil || calls != 1 {
		t.Fatal("wrong whole-envelope metadata accepted")
	}
}
