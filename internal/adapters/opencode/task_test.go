package opencode

import (
	"strings"
	"testing"
)

func TestDecodeTurnRequiresOneSuccessfulNativeSession(t *testing.T) {
	const start = `{"type":"step_start","sessionID":"ses_one"}`
	const text = `{"type":"text","sessionID":"ses_one","part":{"text":"ok"}}`
	const done = `{"type":"step_finish","sessionID":"ses_one","part":{"reason":"stop"}}`
	for _, tc := range []struct {
		name, events string
		valid        bool
	}{
		{"two text parts", start + text + text + done, true},
		{"truncated", start + text, false},
		{"incomplete output", start + text + strings.Replace(done, "stop", "length", 1), false},
		{"wrong session", start + strings.Replace(text, "ses_one", "ses_two", 1) + done, false},
		{"error", start + text + `{"type":"error","sessionID":"ses_one"}`, false},
		{"tool", start + `{"type":"tool_use","sessionID":"ses_one"}` + text + done, false},
		{"late text", start + done + text, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := decodeTurn(strings.NewReader(tc.events))
			if (err == nil) != tc.valid || (tc.valid && (r.SessionID != "ses_one" || r.Text != "okok")) {
				t.Fatalf("result=%+v error=%v", r, err)
			}
		})
	}
}
