package kubejobs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"agentmoor/internal/asset"
	"github.com/google/uuid"
)

func TestEnsureReusesJobAfterAmbiguousCreate(t *testing.T) {
	s := asset.Scope{Tenant: "company", Project: "report", Adapter: "codex", Runtime: "a"}
	turn := uuid.NewString()
	name := identity(s, turn)
	other := s
	other.Runtime = "b"
	if identity(other, turn) == name {
		t.Fatal("Job name shared between scopes")
	}
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("test-only"), 0600); err != nil {
		t.Fatal(err)
	}
	var created int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-only" {
			t.Error("missing dispatcher credential")
		}
		switch r.Method {
		case "GET":
			if r.URL.Path != "/jobs/"+name {
				t.Error(r.URL.Path)
			}
			if created == 0 {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"labels": map[string]string{"agentmoor.io/turn": turn, "agentmoor.io/allocation": name}}})
		case "POST":
			created++
			var job struct {
				Metadata struct{ Name string }
				Spec     struct {
					Template struct {
						Spec struct {
							AutomountServiceAccountToken *bool
							Containers                   []struct{ Command, Args []string }
						}
					}
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
				t.Error(err)
				return
			}
			pod := job.Spec.Template.Spec
			if job.Metadata.Name != name || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || len(pod.Containers) != 1 {
				t.Errorf("wrong Job: %+v", job)
				return
			}
			cmd := pod.Containers[0].Command
			if len(cmd) != 6 || cmd[5] != turn || len(pod.Containers[0].Args) != 0 {
				t.Error(cmd)
			}
			w.WriteHeader(503) // the object exists even though the caller did not get success
		default:
			t.Error(r.Method)
		}
	}))
	defer server.Close()
	d := Dispatcher{profiles: map[asset.Scope]Profile{s: {Scope: s, PodSpec: json.RawMessage(`{"containers":[{"name":"worker","args":["original"]}]}`)}}, jobsURL: server.URL + "/jobs", client: server.Client(), tokenFile: token}
	if _, err := d.ensure(t.Context(), s, turn); err == nil {
		t.Fatal("expected ambiguous create")
	}
	if failed, err := d.ensure(t.Context(), s, turn); failed || err != nil || created != 1 {
		t.Fatal(failed, err, created)
	}
}
