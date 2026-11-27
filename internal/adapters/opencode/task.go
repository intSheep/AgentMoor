package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/adapters/sessionfile"
	"github.com/intSheep/AgentMoor/internal/task"
)

func (a Adapter) ExecuteTask(ctx context.Context, env task.Environment, spec task.Spec) (task.Execution, error) {
	return runText(ctx, env, spec.Prompt(), "", nil)
}

func (a Adapter) ContinueTask(ctx context.Context, env task.Environment, id, message string, restore task.RestoreHistory) (task.Execution, error) {
	return runText(ctx, env, []byte(message), id, restore)
}

func runText(ctx context.Context, env task.Environment, prompt []byte, id string, restore task.RestoreHistory) (task.Execution, error) {
	// A task's private database and configuration must also be used by its exporter.
	vars := []string{
		"HOME=" + env.Home,
		"XDG_DATA_HOME=" + filepath.Join(env.Home, "data"),
		"XDG_CONFIG_HOME=" + filepath.Join(env.Home, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(env.Home, "cache"),
		"XDG_STATE_HOME=" + filepath.Join(env.Home, "state"),
	}
	cfg := map[string]any{"permission": "deny"}
	if url := os.Getenv("OPENAI_BASE_URL"); url != "" {
		cfg["provider"] = map[string]any{"openai": map[string]any{"options": map[string]string{"baseURL": url}}}
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return task.Execution{}, err
	}
	vars = append(vars, "OPENCODE_CONFIG_CONTENT="+string(encoded))
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		vars = append(vars, "OPENAI_API_KEY="+key)
	}
	model := os.Getenv("OPENCODE_MODEL")
	if model == "" {
		model = "openai/gpt-4.1"
	}
	args := []string{"run", "--pure", "--model", model, "--format", "json"}
	if restore != nil {
		if err := restore.NativeDatabase(filepath.Join(env.Home, "data")); err != nil {
			return task.Execution{}, err
		}
		args = append(args, "--session", id)
	}
	var out bytes.Buffer
	if err := adapter.RunCLIInput(ctx, env.Executable, env.Workspace, vars, bytes.NewReader(prompt), &out, 2<<20, args...); err != nil {
		return task.Execution{}, err
	}
	result, err := decodeTurn(&out)
	if err != nil {
		return task.Execution{}, err
	}
	result.HistoryDir = filepath.Join(env.Home, "export")
	if err = os.Mkdir(result.HistoryDir, 0700); err != nil {
		return task.Execution{}, err
	}
	if err = exportSession(ctx, env.Executable, env.Workspace, vars, result.HistoryDir, result.SessionID); err != nil {
		return task.Execution{}, err
	}
	return result, nil
}

func decodeTurn(r io.Reader) (task.Execution, error) {
	var id string
	var text strings.Builder
	var started, done bool
	dec := json.NewDecoder(r)
	for {
		var e struct {
			Type, SessionID string
			Part            struct{ Text, Reason string }
		}
		err := dec.Decode(&e)
		if err == io.EOF {
			break
		}
		if err != nil {
			return task.Execution{}, err
		}
		if done || (!started && e.Type != "step_start") {
			return task.Execution{}, fmt.Errorf("unexpected OpenCode event order")
		}
		if !sessionfile.OpenCodeID.MatchString(e.SessionID) || (id != "" && e.SessionID != id) {
			return task.Execution{}, fmt.Errorf("unexpected OpenCode session identity")
		}
		id = e.SessionID
		switch e.Type {
		case "step_start":
			if started {
				return task.Execution{}, fmt.Errorf("duplicate OpenCode step")
			}
			started = true
		case "text":
			text.WriteString(e.Part.Text)
		case "reasoning":
		case "step_finish":
			if e.Part.Reason != "stop" {
				return task.Execution{}, fmt.Errorf("OpenCode did not finish a text turn")
			}
			done = true
		default:
			return task.Execution{}, fmt.Errorf("unexpected event in OpenCode text turn")
		}
	}
	if !started || !done || strings.TrimSpace(text.String()) == "" {
		return task.Execution{}, fmt.Errorf("incomplete OpenCode result")
	}
	return task.Execution{SessionID: id, Text: text.String()}, nil
}
