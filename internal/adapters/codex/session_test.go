package codex

import (
	"os"
	"testing"
)

func TestRolloutUsesNativeHeaderID(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.MkdirAll("2026/11/22", 0700); err != nil {
		t.Fatal(err)
	}
	const name = "2026/11/22/rollout-different-file-name.jsonl"
	const id = "7d65d0f5-5c08-467b-b022-97cfff41e16a"
	raw := `{"type":"session_meta","payload":{"id":"` + id + `"}}` + "\n" + `{"type":"event_msg","payload":{"message":"hello"}}`
	if err := root.WriteFile(name, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := sessions(t.Context(), root)
	if err != nil || len(got) != 1 || got[0].ID != id || got[0].Entries[0].Name != name {
		t.Fatalf("sessions = %+v, error = %v", got, err)
	}
	if err := root.WriteFile(name, []byte(`{"type":"event_msg","payload":{"id":"`+id+`"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions(t.Context(), root); err == nil {
		t.Fatal("accepted a message as rollout metadata")
	}
}
