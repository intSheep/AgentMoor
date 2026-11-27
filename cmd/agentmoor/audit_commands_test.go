package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/platform"
	"agentmoor/internal/task"
)

func TestAuditCLIReadsLiveCatalogWithoutWriterLeaseOrSigningKey(t *testing.T) {
	t.Setenv("AGENTMOOR_POSTGRES_DSN", "")
	dir := filepath.Join(t.TempDir(), "private ? readonly # store")
	catalog, err := platform.OpenCatalog(t.Context(), dir, "", platform.DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	scope := asset.Scope{Tenant: "fixture", Project: "reports", Runtime: "report-job", Adapter: "codex"}
	secret := bytes.Repeat([]byte{42}, 32)
	token, err := platform.Issue(secret, platform.Grant{Scope: scope, Read: true, Submit: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&platform.API{Catalog: catalog, Secret: secret, Temp: t.TempDir(), TaskAdapters: map[string]bool{"codex": true}}).Handler())
	defer server.Close()
	input, _ := json.Marshal(task.Spec{Instruction: "private-prompt-do-not-audit", Input: "private-input-do-not-audit", Output: "report.json"})
	request, _ := http.NewRequestWithContext(t.Context(), "PUT", server.URL+"/v1/tasks/"+scope.Runtime, bytes.NewReader(input))
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("task did not queue", response.StatusCode)
	}
	// The command must not open a credential even when such a file is present.
	if err = os.WriteFile(filepath.Join(dir, "signing.key"), []byte("private-signing-key-do-not-read"), 0000); err != nil {
		t.Fatal(err)
	}
	args := []string{"audit", "--data", dir, "--tenant", scope.Tenant, "--project", scope.Project, "--runtime", scope.Runtime, "--adapter", scope.Adapter, "--limit", "1"}
	var out bytes.Buffer
	if err = run(t.Context(), args, &out); err != nil {
		t.Fatal("cannot read the live audit store", err)
	}
	var page platform.AuditPage
	if err = json.Unmarshal(out.Bytes(), &page); err != nil || len(page.Events) != 1 || page.Next < 1 || page.Total != 1 {
		t.Fatalf("invalid audit page: %s, %v", out.Bytes(), err)
	}
	for _, forbidden := range []string{token, "private-prompt-do-not-audit", "private-input-do-not-audit", "private-signing-key-do-not-read"} {
		if bytes.Contains(out.Bytes(), []byte(forbidden)) {
			t.Fatal("audit output contained credentials or task text")
		}
	}
	// A second page reads no earlier event; querying another scope reveals none.
	for _, extra := range [][]string{{"--after", "1"}, {"--tenant", "another-tenant"}} {
		out.Reset()
		if err = run(t.Context(), append(append([]string{}, args...), extra...), &out); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(out.Bytes(), &page); err != nil || len(page.Events) != 0 {
			t.Fatal("cursor or scope disclosed another audit event")
		}
	}
}

func TestAuditCLIRejectsInvalidQueriesWithoutCreatingStore(t *testing.T) {
	t.Setenv("AGENTMOOR_POSTGRES_DSN", "")
	dir := filepath.Join(t.TempDir(), "never-created")
	base := []string{"audit", "--data", dir, "--tenant", "fixture", "--project", "reports", "--runtime", "report-job", "--adapter", "codex"}
	for _, extra := range [][]string{{"--after", "-1"}, {"--limit", "0"}, {"--limit", "1001"}, {"--timeout", "0s"}, {"--timeout", "2m"}, {"--tenant", "../outside"}, {"unexpected"}, {}} {
		var out strings.Builder
		if err := run(t.Context(), append(append([]string{}, base...), extra...), &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid or nonexistent audit store accepted")
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatal("audit reader created private state")
		}
	}
}
