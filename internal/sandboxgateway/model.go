package sandboxgateway

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"
)

func (g *Gateway) model(w http.ResponseWriter, r *http.Request, op *operation, token string, p *boundProfile) {
	if !jsonContent(r) {
		replyError(w, 400)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 256<<10+1))
	if err != nil || len(raw) == 0 || len(raw) > 256<<10 {
		replyError(w, 400)
		return
	}
	run, err := g.catalog.AuthenticateSandboxCapability(op.ctx, token, "model")
	if err != nil {
		replyError(w, catalogStatus(err))
		return
	}
	sum := sha256.Sum256([]byte(run.Scope.Prefix() + "/" + run.ID))
	cacheKey := "agentmoor-" + hex.EncodeToString(sum[:])
	body, err := validateRequest(raw, p.profile, cacheKey)
	if err != nil || len(body) > 256<<10 {
		replyError(w, 400)
		return
	}
	key, err := readKey(p.profile.APIKeyFile)
	if err != nil {
		replyError(w, 503)
		return
	}
	if _, err = g.catalog.ReserveSandboxModel(op.ctx, token, p.policy, int64(len(body))); err != nil {
		replyError(w, catalogStatus(err))
		return
	}
	request, err := http.NewRequestWithContext(op.ctx, "POST", p.profile.UpstreamURL, bytes.NewReader(body))
	if err != nil {
		replyError(w, 502)
		return
	}
	request.GetBody = nil // An upstream POST is never automatically replayable.
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("User-Agent", "AgentMoor-sandbox-gateway/1")
	response, err := p.client.Do(request)
	if err != nil {
		replyError(w, 502)
		return
	}
	defer response.Body.Close()
	media, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != 200 || parseErr != nil || media != "text/event-stream" {
		replyError(w, 502)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if err = forwardSSE(w, op, response.Body, p.profile.PerRequestOutputBytes, key, token); err != nil {
		panic(http.ErrAbortHandler)
	}
}

// Only complete, valid text/function Responses events are flushed. In
// particular a provider's error body/message is never reflected to a guest.
func forwardSSE(w http.ResponseWriter, op *operation, body io.Reader, limit int64, key, capability string) error {
	reader := bufio.NewReader(io.LimitReader(body, limit+1))
	var frame bytes.Buffer
	var total int64
	completed := false
	for {
		line, err := reader.ReadString('\n')
		total += int64(len(line))
		if total > limit {
			return fmt.Errorf("upstream stream exceeded allowance")
		}
		frame.WriteString(line)
		if line == "\n" || line == "\r\n" {
			raw := frame.Bytes()
			event, data, valid := sseFrame(raw)
			if !valid {
				return fmt.Errorf("invalid upstream event")
			}
			if len(data) > 0 {
				if completed {
					return fmt.Errorf("data after completed response")
				}
				value, parseErr := strictObject(data)
				if parseErr != nil || value["type"] != event || !safeEvent(event) || containsSecret(value, key, capability) || bytes.Contains(data, []byte(key)) || bytes.Contains(data, []byte(capability)) || containsError(value) {
					return fmt.Errorf("unsupported upstream event")
				}
				if event == "response.completed" {
					completed = true
				}
				// Rebuild the vetted event/data pair. Comments in a frame were
				// not part of the parsed JSON and must never be relayed.
				vetted := []byte("event: " + event + "\ndata: " + string(data) + "\n\n")
				if _, writeErr := w.Write(vetted); writeErr != nil {
					return writeErr
				}
				if flushErr := op.controller.Flush(); flushErr != nil {
					return flushErr
				}
			}
			frame.Reset()
		}
		if err != nil {
			if err != io.EOF {
				return fmt.Errorf("upstream stream interrupted")
			}
			if frame.Len() != 0 || !completed {
				return fmt.Errorf("upstream stream incomplete")
			}
			return nil
		}
	}
}
func sseFrame(raw []byte) (string, []byte, bool) {
	if !utf8.Valid(raw) {
		return "", nil, false
	}
	event := ""
	var data []byte
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			if event != "" {
				return "", nil, false
			}
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if data != nil {
				return "", nil, false
			}
			data = []byte(strings.TrimPrefix(line, "data: "))
		default:
			return "", nil, false
		}
	}
	if data == nil {
		return "", nil, true
	}
	return event, data, event != ""
}
func safeEvent(event string) bool {
	switch event {
	case "response.created", "response.in_progress", "response.completed", "response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done",
		"response.output_text.delta", "response.output_text.done", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.delta", "response.reasoning_text.done", "response.refusal.delta", "response.refusal.done":
		return true
	}
	return false
}
func containsSecret(v any, secrets ...string) bool {
	switch value := v.(type) {
	case string:
		for _, secret := range secrets {
			if secret != "" && strings.Contains(value, secret) {
				return true
			}
		}
	case map[string]any:
		for name, item := range value {
			if containsSecret(name, secrets...) || containsSecret(item, secrets...) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if containsSecret(item, secrets...) {
				return true
			}
		}
	}
	return false
}
func containsError(v any) bool {
	switch value := v.(type) {
	case map[string]any:
		for key, item := range value {
			if key == "error" && item != nil {
				return true
			}
			if containsError(item) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if containsError(item) {
				return true
			}
		}
	}
	return false
}
