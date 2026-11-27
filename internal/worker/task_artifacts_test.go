package worker

import (
	"bytes"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/intSheep/AgentMoor/internal/adapters/codex"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/platform"
	"github.com/intSheep/AgentMoor/internal/task"
)

func TestTaskDeliveryCollectsAllArtifactsWithoutSkippingSkills(t *testing.T) {
	catalog, err := platform.OpenCatalog(t.Context(), t.TempDir(), "", platform.DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	secret := bytes.Repeat([]byte{3}, 32)
	scope := asset.Scope{Tenant: "team", Project: "reports", Runtime: "delivery", Adapter: "codex"}
	srv := httptest.NewServer((&platform.API{Catalog: catalog, Secret: secret, Temp: t.TempDir()}).Handler())
	defer srv.Close()
	token, err := platform.Issue(secret, platform.Grant{Scope: scope, Read: true, Write: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	client, err := platform.NewClient(srv.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Scope: scope, Endpoint: srv.URL, Workspace: t.TempDir(), StateDir: t.TempDir(), Artifacts: "out", ArtifactSession: fixtureID}
	e := Engine{Config: c, Adapter: codex.New(), Client: client}
	put(t, c.Workspace, ".agents/skills/greet/SKILL.md", "---\nname: greet\n---\nA native skill.")
	put(t, c.Workspace, "out/report.json", "first report")
	put(t, c.Workspace, "out/detail.txt", "details")
	put(t, c.Workspace, "out/deleted.txt", "removed later")
	put(t, c.Workspace, "out/static.txt", "unchanged")
	var first []task.Ref
	if err := e.sync(t.Context(), "", &first); err != nil || len(first) != 4 {
		t.Fatal(first, err)
	}
	put(t, c.Workspace, "out/report.json", "revised report")
	if err := os.MkdirAll(filepath.Join(c.Workspace, "out/renamed"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(c.Workspace, "out/detail.txt"), filepath.Join(c.Workspace, "out/renamed/detail.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.Workspace, "out/deleted.txt")); err != nil {
		t.Fatal(err)
	}
	turn := uuid.NewString()
	var second []task.Ref
	if err := e.sync(t.Context(), turn, &second); err != nil || len(second) != 3 {
		t.Fatal(second, err)
	}
	want := map[string]string{"out/report.json": "revised report", "out/renamed/detail.txt": "details", "out/static.txt": "unchanged"}
	for _, ref := range second {
		id := ref.ID[len("turns/"+turn+"/"):]
		if ref.Version != 1 || ref.ContentPath != ref.Key.ContentPath(1) || want[id] == "" {
			t.Fatal("incorrect turn receipt", ref)
		}
		meta, body, err := catalog.Read(t.Context(), scope, ref.Key, ref.Version)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(body)
		body.Close()
		if err != nil || string(raw) != want[id] || meta.SessionID != fixtureID || meta.Scope != scope || meta.AdapterVersion != e.Adapter.Descriptor().Version || meta.MediaType == "" || meta.SHA256 != ref.SHA256 {
			t.Fatal(meta, string(raw), err)
		}
	}
	for _, ref := range first {
		_, body, err := catalog.Read(t.Context(), scope, ref.Key, ref.Version)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(body)
		body.Close()
		if err != nil || (ref.ID == "out/report.json" && string(raw) != "first report") {
			t.Fatal("old delivery changed", ref, string(raw), err)
		}
	}
	for _, key := range []asset.Key{{Kind: "skill", ID: ".agents/skills/greet/SKILL.md"}, task.TurnKey(turn, asset.Key{Kind: "skill", ID: ".agents/skills/greet/SKILL.md"})} {
		if _, body, err := catalog.Read(t.Context(), scope, key, 1); err != nil {
			t.Fatal("task delivery skipped native skill discovery", key, err)
		} else {
			body.Close()
		}
	}
}
