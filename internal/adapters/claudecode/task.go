package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/adapters/sessionfile"
	"github.com/intSheep/AgentMoor/internal/task"
)

// ExecuteTask supports one text-to-text turn. Workspace state and credentials
// belong to this allocation. Model credentials come only from the operator.
func (a Adapter) ExecuteTask(ctx context.Context, env task.Environment, spec task.Spec) (task.Execution, error) {
	return runText(ctx, env, uuid.NewString(), spec.Prompt(), nil)
}

func (a Adapter) ContinueTask(ctx context.Context, env task.Environment, id, message string, restore task.RestoreHistory) (task.Execution, error) {
	if !sessionfile.UUID(id) {
		return task.Execution{}, fmt.Errorf("invalid Claude session ID")
	}
	return runText(ctx, env, id, []byte(message), restore)
}

func runText(ctx context.Context, env task.Environment, id string, prompt []byte, restore task.RestoreHistory) (task.Execution, error) {
	var result task.Execution
	configDir := filepath.Join(env.Home, ".claude")
	vars := []string{"HOME=" + env.Home, "CLAUDE_CONFIG_DIR=" + configDir, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1"}
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS"} {
		if v, ok := os.LookupEnv(key); ok {
			vars = append(vars, key+"="+v)
		}
	}
	// Send data on stdin; credentials and user input never appear in argv.
	args := []string{"--bare", "--setting-sources", "", "--tools", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--output-format", "json", "-p"}
	if restore != nil {
		// Claude can resume and append directly to a transcript path. This avoids
		// reproducing its project-directory encoding for long or non-ASCII paths.
		history := filepath.Join(configDir, "projects", "restored")
		if err := restore.Files(history); err != nil {
			return result, err
		}
		args = append(args, "--resume", filepath.Join(history, id+".jsonl"))
	} else {
		args = append(args, "--session-id", id)
	}
	var output bytes.Buffer
	err := adapter.RunCLIInput(ctx, env.Executable, env.Workspace, vars, bytes.NewReader(prompt), &output, 2<<20, args...)
	if err != nil {
		return result, err
	}
	var response struct {
		Session string `json:"session_id"`
		Text    string `json:"result"`
		Failed  bool   `json:"is_error"`
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.Failed || response.Session != id || response.Text == "" {
		return result, fmt.Errorf("Claude returned no successful text result for the assigned session")
	}
	projects := filepath.Join(configDir, "projects")
	entries, err := os.ReadDir(projects)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		history := filepath.Join(projects, entry.Name())
		info, err := os.Lstat(filepath.Join(history, id+".jsonl"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return result, err
		}
		if info.Mode().IsRegular() {
			return task.Execution{Text: response.Text, SessionID: id, HistoryDir: history}, nil
		}
	}
	return result, fmt.Errorf("missing native transcript for task")
}
