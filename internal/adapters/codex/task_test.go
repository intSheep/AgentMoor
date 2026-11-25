package codex

import (
	"strings"
	"testing"
)

func TestDecodeTurnRequiresCompletedTextAndNativeIdentity(t *testing.T) {
	const start = `{"type":"thread.started","thread_id":"7d65d0f5-5c08-467b-b022-97cfff41e16a"}`
	const started = `{"type":"turn.started"}`
	const notice = `{"type":"item.completed","item":{"type":"error"}}`
	const msg = `{"type":"item.completed","item":{"type":"agent_message","text":"report"}}`
	const done = `{"type":"turn.completed"}`
	for _, tc := range []struct {
		name, events string
		valid        bool
	}{
		{"complete", start + notice + started + msg + done, true},
		{"truncated", start + msg, false},
		{"missing session", msg + done, false},
		{"item error", start + started + notice + msg + done, false},
		{"tool", start + started + `{"type":"item.completed","item":{"type":"command_execution"}}` + msg + done, false},
		{"failed", start + msg + `{"type":"turn.failed"}`, false},
		{"conflicting identity", start + start + msg + done, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := decodeTurn(strings.NewReader(tc.events), false)
			if (err == nil) != tc.valid || (tc.valid && result.Text != "report") {
				t.Fatalf("turn = %+v, err = %v", result, err)
			}
		})
	}
}
