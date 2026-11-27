package worker

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/platform"
	"github.com/intSheep/AgentMoor/internal/task"
)

// Bulk restore must materialize a native ID once, even when independent
// published turns and an unfinished publication each have their own asset key.
func TestBulkRestoreSelectsCommittedNativeSession(t *testing.T) {
	scope := asset.Scope{Tenant: "t", Project: "p", Runtime: "r", Adapter: "opencode"}
	native := asset.Key{Kind: "session", ID: "ses_original"}
	success := task.TurnKey("ad31b87c-50be-4da7-a52a-4c4a0ce5f9dd", native)
	partial := task.TurnKey("dad9401b-51d9-4933-88b7-e22c2bc23e22", native)
	body := []byte(`{"info":{"id":"ses_original"},"messages":["first","follow-up"]}`)
	revision := asset.Revision{Scope: scope, Version: 1, Metadata: asset.Metadata{
		Key: success, AdapterVersion: "test", Format: "opencode-export-json.v1", MediaType: "application/json",
		Name: "session.json", Origin: "runtime", Completeness: "persisted-files", Size: int64(len(body)), SHA256: fmt.Sprintf("%x", sha256.Sum256(body)),
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		switch r.URL.Path {
		case "/v1/context":
			enc.Encode(platform.Grant{Read: true, Scope: scope})
		case "/v1/heads":
			enc.Encode([]asset.Revision{{Metadata: asset.Metadata{Key: partial}, Version: 1}, revision, {Metadata: asset.Metadata{Key: native}, Version: 1}})
		case "/v1/tasks/r/checkpoint":
			enc.Encode(task.Checkpoint{SessionID: native.ID, Session: task.Ref{Key: success, Version: 1}})
		case "/v1/content":
			if r.URL.Query().Get("id") != success.ID || r.URL.Query().Get("version") != "1" {
				t.Error("wrong revision requested", r.URL)
			}
			meta, _ := json.Marshal(revision)
			w.Header().Set("X-Agentmoor-Revision", base64.RawURLEncoding.EncodeToString(meta))
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client, err := platform.NewClient(srv.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	e := Engine{Config: Config{Scope: scope}, Client: client}
	dest := filepath.Join(t.TempDir(), "history")
	if err := e.RestoreSessions(t.Context(), dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, native.ID+".json"))
	if err != nil || string(got) != string(body) {
		t.Fatal(string(got), err)
	}
}
