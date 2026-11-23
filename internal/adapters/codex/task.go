package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"agentmoor/internal/adapter"
	"agentmoor/internal/adapters/sessionfile"
	"agentmoor/internal/task"
)

func (a Adapter) ExecuteTask(ctx context.Context, env task.Environment, spec task.Spec) (task.Execution, error) {
	return runText(ctx, env, spec.Prompt(), "", nil)
}

func (a Adapter) ContinueTask(ctx context.Context, env task.Environment, id, message string, restore task.RestoreHistory) (task.Execution, error) {
	return runText(ctx, env, []byte(message), id, restore)
}

func runText(ctx context.Context, env task.Environment, prompt []byte, id string, restore task.RestoreHistory) (task.Execution, error) {
	home := filepath.Join(env.Home, ".codex")
	if err := os.Mkdir(home, 0700); err != nil {
		return task.Execution{}, err
	}
	vars := []string{"HOME=" + env.Home, "CODEX_HOME=" + home}
	args := []string{
		"exec", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--sandbox", "read-only",
		"--disable", "shell_tool", "--disable", "multi_agent", "--disable", "apps", "--disable", "hooks",
		"-c", `web_search="disabled"`, "--json",
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		vars = append(vars, "OPENAI_API_KEY="+key, "CODEX_API_KEY="+key)
	}
	if url := os.Getenv("OPENAI_BASE_URL"); url != "" {
		args = append(args,
			"-c", `model_provider="operator"`,
			"-c", `model_providers.operator.name="Operator"`,
			"-c", "model_providers.operator.base_url="+strconv.Quote(url),
			"-c", `model_providers.operator.env_key="OPENAI_API_KEY"`,
			"-c", `model_providers.operator.wire_api="responses"`,
		)
	}
	if model := os.Getenv("CODEX_MODEL"); model != "" {
		args = append(args, "--model", model)
	}
	if restore != nil {
		if err := restore.Files(filepath.Join(home, "sessions")); err != nil {
			return task.Execution{}, err
		}
		args = append(args, "resume", id)
	}
	var out bytes.Buffer
	if err := adapter.RunCLIInput(ctx, env.Executable, env.Workspace, vars, bytes.NewReader(prompt), &out, 2<<20, append(args, "-")...); err != nil {
		return task.Execution{}, err
	}
	result, err := decodeTurn(&out)
	result.HistoryDir = filepath.Join(home, "sessions")
	return result, err
}

func decodeTurn(r io.Reader) (task.Execution, error) {
	var result task.Execution
	var started, done bool
	dec := json.NewDecoder(r)
	for {
		var e struct {
			Type   string
			Thread string `json:"thread_id"`
			Item   struct{ Type, Text string }
		}
		err := dec.Decode(&e)
		if err == io.EOF {
			break
		}
		if err != nil {
			return task.Execution{}, err
		}
		if done {
			return task.Execution{}, fmt.Errorf("unexpected event after Codex turn completion")
		}
		switch e.Type {
		case "thread.started":
			if result.SessionID != "" || !sessionfile.UUID(e.Thread) {
				return task.Execution{}, fmt.Errorf("invalid Codex thread identity")
			}
			result.SessionID = e.Thread
		case "turn.started":
			if started || result.SessionID == "" {
				return task.Execution{}, fmt.Errorf("unexpected Codex turn start")
			}
			started = true
		case "item.completed":
			switch e.Item.Type {
			case "agent_message":
				if !started {
					return task.Execution{}, fmt.Errorf("unexpected Codex text before turn start")
				}
				result.Text = e.Item.Text
			case "reasoning":
			case "error":
				// The CLI emits model metadata fallback notices before turn.started.
				if started {
					return task.Execution{}, fmt.Errorf("Codex reported an item error during the turn")
				}
			default:
				return task.Execution{}, fmt.Errorf("unexpected item in Codex text turn")
			}
		case "turn.completed":
			done = true
		case "turn.failed", "error":
			return task.Execution{}, fmt.Errorf("Codex turn failed")
		}
	}
	if !started || !done || result.SessionID == "" || result.Text == "" {
		return task.Execution{}, fmt.Errorf("incomplete Codex turn")
	}
	return result, nil
}
