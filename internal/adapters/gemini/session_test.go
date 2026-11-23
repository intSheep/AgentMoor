package gemini

import (
	"os"
	"testing"
)

func TestHistoricalJSONEnvelope(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	const id = "7d65d0f5-5c08-467b-b022-97cfff41e16a"
	const file = "session-previous.json"
	// Older JSON files may contain the message array before either identifier.
	body := `{"messages":[{"content":["hello",{"text":"world"}]}],"projectHash":"project","sessionId":"` + id + `"}`
	if err := root.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := sessions(t.Context(), root)
	if err != nil || len(got) != 1 || got[0].ID != id || got[0].Format != "gemini-chats.json.v1" {
		t.Fatalf("sessions = %+v, error = %v", got, err)
	}
	if err := root.WriteFile(file, []byte(`{"sessionId":"`+id+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions(t.Context(), root); err == nil {
		t.Fatal("accepted missing project identity")
	}
}
