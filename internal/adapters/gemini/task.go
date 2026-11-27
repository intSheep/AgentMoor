package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/task"
)

// ExecuteTask allocates a separate Gemini home and pins the native session ID before the turn.
func (a Adapter) ExecuteTask(ctx context.Context, env task.Environment, spec task.Spec) (task.Execution, error) {
	return runText(ctx, env, uuid.NewString(), spec.Prompt(), nil)
}

func (a Adapter) ContinueTask(ctx context.Context, env task.Environment, id, message string, restore task.RestoreHistory) (task.Execution, error) {
	return runText(ctx, env, id, []byte(message), restore)
}

func runText(ctx context.Context, env task.Environment, id string, prompt []byte, restore task.RestoreHistory) (task.Execution, error) {
	dir := filepath.Join(env.Home, ".gemini")
	if err := os.Mkdir(dir, 0700); err != nil {
		return task.Execution{}, err
	}
	settings := []byte(`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), settings, 0600); err != nil {
		return task.Execution{}, err
	}
	policy := filepath.Join(dir, "text-turn.toml")
	if err := os.WriteFile(policy, []byte("[[rule]]\ntoolName = \"*\"\ndecision = \"deny\"\npriority = 999\n"), 0600); err != nil {
		return task.Execution{}, err
	}
	args := []string{"--skip-trust", "--admin-policy", policy, "--output-format", "json"}
	if model := os.Getenv("GEMINI_MODEL"); model != "" {
		args = append(args, "--model", model)
	}
	vars := []string{"HOME=" + env.Home, "GEMINI_CLI_HOME=" + env.Home}
	for _, key := range []string{"GEMINI_API_KEY", "GOOGLE_GEMINI_BASE_URL", "NODE_EXTRA_CA_CERTS"} {
		if val, ok := os.LookupEnv(key); ok {
			vars = append(vars, key+"="+val)
		}
	}
	if restore != nil {
		// Let this pinned CLI choose the project slug, including normalized names.
		if err := adapter.RunCLI(ctx, env.Executable, env.Workspace, vars, io.Discard, 64<<10, "--skip-trust", "--list-sessions"); err != nil {
			return task.Execution{}, err
		}
		history, err := projectHistory(dir, env.Workspace)
		if err != nil {
			return task.Execution{}, err
		}
		if err := restore.Files(history); err != nil {
			return task.Execution{}, err
		}
		args = append(args, "--resume", id)
	} else {
		args = append(args, "--session-id", id)
	}
	var out bytes.Buffer
	if err := adapter.RunCLIInput(ctx, env.Executable, env.Workspace, vars, bytes.NewReader(prompt), &out, 2<<20, args...); err != nil {
		return task.Execution{}, err
	}
	var resp struct {
		Session  string `json:"session_id"`
		Response string
		Error    *json.RawMessage
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil || resp.Session != id || resp.Error != nil || strings.TrimSpace(resp.Response) == "" {
		return task.Execution{}, fmt.Errorf("Gemini returned no successful text for the assigned session")
	}
	history, err := projectHistory(dir, env.Workspace)
	if err != nil {
		return task.Execution{}, err
	}
	return task.Execution{Text: resp.Response, SessionID: id, HistoryDir: history}, nil
}

func projectHistory(home, workspace string) (string, error) {
	data, err := os.ReadFile(filepath.Join(home, "projects.json"))
	if err != nil {
		return "", err
	}
	var registry struct{ Projects map[string]string }
	if err = json.Unmarshal(data, &registry); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", err
	}
	slug := registry.Projects[real]
	if !filepath.IsLocal(slug) || filepath.Base(slug) != slug || slug == "." {
		return "", fmt.Errorf("invalid Gemini project directory")
	}
	return filepath.Join(home, "tmp", slug, "chats"), nil
}
