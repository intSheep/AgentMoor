package sandboxgateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

var toolName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]{0,127}$`)

// strictObject rejects duplicate keys at every depth before decoding. Every
// routing-bearing object below then has an explicit schema; schemas/arguments
// are data for caller-owned functions, never provider resource selectors.
func strictObject(raw []byte) (map[string]any, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("invalid model JSON")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return fmt.Errorf("model JSON exceeds depth bound")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				token, err := d.Token()
				key, ok := token.(string)
				if err != nil || !ok || seen[key] {
					return fmt.Errorf("duplicate model JSON key")
				}
				seen[key] = true
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid model JSON")
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing model JSON")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var out map[string]any
	if err := d.Decode(&out); err != nil || out == nil {
		return nil, fmt.Errorf("model request must be an object")
	}
	return out, nil
}
func keys(m map[string]any, allowed string) bool {
	set := map[string]bool{}
	for _, s := range strings.Fields(allowed) {
		set[s] = true
	}
	for k := range m {
		if !set[k] {
			return false
		}
	}
	return true
}
func object(v any) (map[string]any, bool)            { m, ok := v.(map[string]any); return m, ok && m != nil }
func text(v any, empty bool) bool                    { s, ok := v.(string); return ok && (empty || s != "") }
func optionalText(m map[string]any, key string) bool { v, ok := m[key]; return !ok || text(v, true) }
func optionalBool(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok {
		return true
	}
	_, valid := v.(bool)
	return valid
}
func in(v any, values string) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	for _, value := range strings.Fields(values) {
		if s == value {
			return true
		}
	}
	return false
}
func integer(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

func validateRequest(raw []byte, p Profile, cacheKey string) ([]byte, error) {
	m, err := strictObject(raw)
	if err != nil {
		return nil, err
	}
	invalid := func() ([]byte, error) { return nil, fmt.Errorf("unsupported stateless text model request") }
	if !keys(m, "model input instructions tools tool_choice parallel_tool_calls reasoning include stream store background max_output_tokens service_tier text prompt_cache_key client_metadata") || m["model"] != p.Model || !optionalText(m, "instructions") || !optionalBool(m, "parallel_tool_calls") {
		return invalid()
	}
	for _, key := range []string{"store", "background"} {
		if value, ok := m[key]; ok && value != false {
			return invalid()
		}
		m[key] = false
	}
	if value, ok := m["stream"]; ok && value != true {
		return invalid()
	}
	m["stream"] = true
	if value, ok := m["service_tier"]; ok && value != "default" {
		return invalid()
	}
	m["service_tier"] = "default"
	max := int64(p.MaxOutputTokens)
	if value, ok := m["max_output_tokens"]; ok {
		n, valid := integer(value)
		if !valid || n < 1 || n > max {
			return invalid()
		}
		max = n
	}
	m["max_output_tokens"] = max
	if value, ok := m["reasoning"]; ok {
		v, valid := object(value)
		if !valid || !keys(v, "effort summary") {
			return invalid()
		}
		if e, ok := v["effort"]; ok && !in(e, "none minimal low medium high xhigh") {
			return invalid()
		}
		if s, ok := v["summary"]; ok && !in(s, "auto concise detailed") {
			return invalid()
		}
	}
	if value, ok := m["include"]; ok {
		a, valid := value.([]any)
		if !valid || len(a) > 1 {
			return invalid()
		}
		for _, item := range a {
			if item != "reasoning.encrypted_content" {
				return invalid()
			}
		}
	}
	if value, ok := m["text"]; ok {
		v, valid := object(value)
		if !valid || !keys(v, "format verbosity") {
			return invalid()
		}
		if f, ok := v["format"]; ok {
			format, valid := object(f)
			if !valid || !keys(format, "type") || format["type"] != "text" {
				return invalid()
			}
		}
		if b, ok := v["verbosity"]; ok && !in(b, "low medium high") {
			return invalid()
		}
	}
	switch input := m["input"].(type) {
	case string:
		if input == "" {
			return invalid()
		}
	case []any:
		if len(input) == 0 || len(input) > 2048 {
			return invalid()
		}
		calls := map[string]string{}
		for _, item := range input {
			v, valid := object(item)
			if !valid || !inputItem(v, calls) {
				return invalid()
			}
		}
	default:
		return invalid()
	}
	if value, ok := m["tools"]; ok {
		tools, valid := value.([]any)
		if !valid || len(tools) > 128 {
			return invalid()
		}
		for _, tool := range tools {
			v, valid := object(tool)
			if !valid || !toolDefinition(v, true) {
				return invalid()
			}
		}
	}
	if value, ok := m["tool_choice"]; ok {
		if !in(value, "auto none required") {
			v, valid := object(value)
			if !valid || !keys(v, "type name namespace") || !in(v["type"], "function custom") || !validName(v["name"]) || !optionalText(v, "namespace") {
				return invalid()
			}
		}
	}
	// Native metadata/cache strings are not provider references, but guests may
	// not select another tenant's routing key or send unbounded tracing fields.
	if value, ok := m["prompt_cache_key"]; ok && !text(value, true) {
		return invalid()
	}
	if value, ok := m["client_metadata"]; ok {
		if _, valid := object(value); !valid {
			return invalid()
		}
	}
	delete(m, "client_metadata")
	m["prompt_cache_key"] = cacheKey
	return json.Marshal(m)
}
func validName(v any) bool { s, ok := v.(string); return ok && toolName.MatchString(s) }
func contents(v any, types string) bool {
	if _, ok := v.(string); ok {
		return true
	}
	a, ok := v.([]any)
	if !ok {
		return false
	}
	for _, raw := range a {
		m, ok := object(raw)
		if !ok || !keys(m, "type text annotations") || !in(m["type"], types) || !text(m["text"], true) {
			return false
		}
		if raw, exists := m["annotations"]; exists {
			annotations, ok := raw.([]any)
			if !ok || len(annotations) != 0 {
				return false
			}
		}
	}
	return true
}
func inputItem(m map[string]any, calls map[string]string) bool {
	if !optionalText(m, "id") || !optionalText(m, "status") {
		return false
	}
	typ, exists := m["type"]
	if !exists {
		typ = "message"
	}
	switch typ {
	case "message":
		return keys(m, "type id role content status phase") && in(m["role"], "system developer user assistant") && contents(m["content"], "input_text output_text") && optionalText(m, "phase")
	case "reasoning":
		if !keys(m, "type id status summary content encrypted_content") || !text(m["encrypted_content"], false) {
			return false
		}
		for key, types := range map[string]string{"summary": "summary_text", "content": "reasoning_text"} {
			if value, ok := m[key]; ok && !contents(value, types) {
				return false
			}
		}
		return true
	case "function_call", "custom_tool_call":
		if !keys(m, "type id status call_id name namespace arguments input") || !validName(m["name"]) || !text(m["call_id"], false) || !optionalText(m, "namespace") {
			return false
		}
		id := m["call_id"].(string)
		if len(id) > 256 || calls[id] != "" {
			return false
		}
		if typ == "function_call" {
			if !text(m["arguments"], true) || m["input"] != nil {
				return false
			}
		} else {
			if !text(m["input"], true) || m["arguments"] != nil {
				return false
			}
		}
		calls[id] = typ.(string)
		return true
	case "function_call_output", "custom_tool_call_output":
		if !keys(m, "type id status call_id output") || !text(m["call_id"], false) || !contents(m["output"], "input_text output_text") {
			return false
		}
		expected := "function_call"
		if typ == "custom_tool_call_output" {
			expected = "custom_tool_call"
		}
		return calls[m["call_id"].(string)] == expected
	default:
		return false
	}
}
func toolDefinition(m map[string]any, namespace bool) bool {
	if !validName(m["name"]) || !optionalText(m, "description") {
		return false
	}
	switch m["type"] {
	case "namespace":
		if !namespace || !keys(m, "type name description tools") {
			return false
		}
		tools, ok := m["tools"].([]any)
		if !ok || len(tools) > 128 {
			return false
		}
		for _, raw := range tools {
			v, ok := object(raw)
			if !ok || !toolDefinition(v, false) {
				return false
			}
		}
		return true
	case "function":
		if !keys(m, "type name description parameters strict") || !optionalBool(m, "strict") {
			return false
		}
		parameters, ok := object(m["parameters"])
		return ok && localSchema(parameters)
	case "custom":
		if !keys(m, "type name description format") {
			return false
		}
		if value, ok := m["format"]; ok {
			format, ok := object(value)
			if !ok {
				return false
			}
			switch format["type"] {
			case "text":
				return keys(format, "type")
			case "grammar":
				return keys(format, "type syntax definition") && in(format["syntax"], "lark regex") && text(format["definition"], false)
			default:
				return false
			}
		}
		return true
	default:
		return false
	}
}
func localSchema(v any) bool {
	switch value := v.(type) {
	case map[string]any:
		for k, item := range value {
			if k == "$ref" {
				s, ok := item.(string)
				if !ok || !strings.HasPrefix(s, "#/") {
					return false
				}
			}
			if !localSchema(item) {
				return false
			}
		}
	case []any:
		for _, item := range value {
			if !localSchema(item) {
				return false
			}
		}
	}
	return true
}
