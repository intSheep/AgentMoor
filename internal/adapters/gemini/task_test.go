package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectHistoryUsesOnlyTheAssignedWorkspace(t *testing.T) {
	home := t.TempDir()
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"work-2", "../other", "nested/work", ".", ""} {
		raw, err := json.Marshal(map[string]map[string]string{"projects": {work: slug, "/other": "another-project"}})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(home, "projects.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		path, err := projectHistory(home, work)
		if slug == "work-2" {
			if err != nil || path != filepath.Join(home, "tmp", slug, "chats") {
				t.Fatalf("path = %q, error = %v", path, err)
			}
		} else if err == nil {
			t.Errorf("accepted invalid project slug %q", slug)
		}
	}
}
