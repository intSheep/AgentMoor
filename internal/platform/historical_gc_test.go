package platform

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
)

func TestHistoricalArtifactAndSkillReferencesSurviveGC(t *testing.T) {
	objects := DiskObjects{Dir: t.TempDir()}
	catalog, err := OpenCatalog(t.Context(), t.TempDir(), "", objects)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	checkHistoricalReferencesSurviveGC(t, catalog, objects, scope("history-gc"))
}

// The same assertions can be called with a PostgreSQL catalog and S3 Objects.
// No filesystem-specific object layout or database timestamps are assumed.
func checkHistoricalReferencesSurviveGC(t *testing.T, catalog *Catalog, objects Objects, s asset.Scope) {
	t.Helper()
	type pinned struct {
		change  asset.Change
		value   string
		version int64
	}
	var retained []pinned
	for _, key := range []asset.Key{
		{Kind: "artifact", ID: "out/report.txt"},
		{Kind: "skill", ID: ".agents/skills/report-rules/SKILL.md"},
	} {
		for version := int64(1); version <= 2; version++ {
			value := "original report bytes"
			if version == 2 {
				value = "revised report bytes"
			}
			ch := change(value, version-1)
			ch.Key = key
			if key.Kind == "skill" {
				value = historicalSkillPackage(t, version)
				sum := sha256.Sum256([]byte(value))
				ch.SHA256, ch.Size = hex.EncodeToString(sum[:]), int64(len(value))
				ch.Format, ch.MediaType = "skill-tar.v1", "application/x-tar"
				ch.Name, ch.Origin = "report-rules", "platform-managed"
			}
			ticket := upload(t, catalog, s, ch, value)
			if got, err := catalog.Commit(t.Context(), s, ticket.ID); err != nil || got != version {
				t.Fatalf("commit %s v%d: got %d, err %v", key.Kind, version, got, err)
			}
			retained = append(retained, pinned{ch, value, version})
		}
	}
	assertHeads := func() {
		t.Helper()
		heads, err := catalog.Heads(t.Context(), s)
		if err != nil || len(heads) != 2 {
			t.Fatalf("heads = %v, err %v", heads, err)
		}
		for _, head := range heads {
			if head.Version != 2 {
				t.Fatalf("expected advanced head, got %s v%d", head.Kind, head.Version)
			}
			var original, latest *pinned
			for i := range retained {
				p := &retained[i]
				if p.change.Key == head.Key {
					if p.version == 1 {
						original = p
					} else {
						latest = p
					}
				}
			}
			if original == nil || latest == nil || head.SHA256 == original.change.SHA256 || head.SHA256 != latest.change.SHA256 {
				t.Fatalf("head did not move to different bytes: %+v", head)
			}
		}
	}
	assertHeads()
	// Abandon attempts to publish the original bytes again on top of v2. Their
	// expired operations make those old digests GC candidates, so retention must
	// rely on the historical v1 revisions rather than just the current heads.
	for _, ref := range retained {
		if ref.version != 1 {
			continue
		}
		attempt := ref.change
		attempt.Base = 2
		ticket := upload(t, catalog, s, attempt, ref.value)
		if ticket.Upload || ticket.Version != 0 {
			t.Fatalf("expected an uncommitted operation reusing retained bytes: %+v", ticket)
		}
	}

	// This uploaded object has no committed revision. Its removal proves that
	// collection actually ran while old, non-head revisions stayed referenced.
	orphan := change("unreferenced upload", 2)
	upload(t, catalog, s, orphan, "unreferenced upload")
	if exists, err := objects.Exists(t.Context(), objectKey(s, orphan.SHA256)); err != nil || !exists {
		t.Fatalf("orphan upload missing before collection: %t %v", exists, err)
	}
	if removed, err := catalog.Collect(t.Context(), s, time.Now().Add(time.Second)); err != nil || removed != 1 {
		t.Fatalf("collection removed %d objects, err %v", removed, err)
	}
	if exists, err := objects.Exists(t.Context(), objectKey(s, orphan.SHA256)); err != nil || exists {
		t.Fatalf("unreferenced upload survived collection: %t %v", exists, err)
	}
	if removed, err := catalog.Collect(t.Context(), s, time.Now().Add(time.Second)); err != nil || removed != 0 {
		t.Fatalf("repeated collection removed %d objects, err %v", removed, err)
	}
	assertHeads()

	secret := bytes.Repeat([]byte{6}, 32)
	token, err := Issue(secret, Grant{Scope: s, Read: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&API{Catalog: catalog, Secret: secret, Temp: t.TempDir()}).Handler())
	defer server.Close()
	client, err := NewClient(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range retained {
		metadata, body, err := client.Content(t.Context(), ref.change.Key, ref.version)
		if err != nil {
			t.Fatalf("versioned HTTP read after GC: %s v%d: %v", ref.change.Kind, ref.version, err)
		}
		raw, readErr := io.ReadAll(body)
		closeErr := body.Close()
		sum := sha256.Sum256(raw)
		if readErr != nil || closeErr != nil || string(raw) != ref.value || metadata.Scope != s || metadata.Version != ref.version || metadata.SHA256 != ref.change.SHA256 || hex.EncodeToString(sum[:]) != ref.change.SHA256 {
			t.Fatalf("historical bytes or reference changed: %s v%d: %+v %v %v", ref.change.Kind, ref.version, metadata, readErr, closeErr)
		}
	}
}

func historicalSkillPackage(t *testing.T, version int64) string {
	t.Helper()
	marker := "original-v1"
	if version == 2 {
		marker = "new-head-v2"
	}
	var output bytes.Buffer
	archive := tar.NewWriter(&output)
	for _, file := range []struct{ name, body string }{
		{"SKILL.md", "---\nname: report-rules\ndescription: synthetic retained package\n---\n" + marker + "\n"},
		{"scripts/render.py", "print('" + marker + "')\n"},
		{"references/rule.txt", marker + " reference\n"},
	} {
		if err := archive.WriteHeader(&tar.Header{Name: file.name, Mode: 0644, Size: int64(len(file.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(file.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return output.String()
}
