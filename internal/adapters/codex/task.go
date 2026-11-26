package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"agentmoor/internal/adapter"
	"agentmoor/internal/adapters/sessionfile"
	"agentmoor/internal/asset"
	"agentmoor/internal/task"
)

var nativeSkillName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func (a Adapter) SkillDirectory(env task.Environment, name string) (string, error) {
	if !filepath.IsAbs(env.Home) || !nativeSkillName.MatchString(name) {
		return "", fmt.Errorf("Codex skills require a private absolute home and a valid name")
	}
	return filepath.Join(env.Home, ".codex", "skills", name), nil
}

// The Worker restores verified packages into a private native home. Resolve the
// same paths Codex advertises, so explicit mentions select these packages even
// when the workspace has another Skill with the same name.
func loadedSkills(env task.Environment) ([]task.LoadedSkill, error) {
	if len(env.Skills) == 0 {
		return nil, nil
	}
	if len(env.Skills) > task.MaxSkills || !filepath.IsAbs(env.Home) {
		return nil, fmt.Errorf("invalid Codex private skills")
	}
	home, err := filepath.EvalSymlinks(env.Home)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, ".codex", "skills")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil || resolvedRoot != root {
		return nil, fmt.Errorf("Codex skill root must stay in its private home")
	}
	seen := make(map[string]bool, len(env.Skills))
	loaded := make([]task.LoadedSkill, 0, len(env.Skills))
	for _, skill := range env.Skills {
		if !nativeSkillName.MatchString(skill.Name) || seen[skill.Name] || !filepath.IsAbs(skill.Manifest) {
			return nil, fmt.Errorf("invalid or duplicate Codex skill")
		}
		manifest, err := filepath.EvalSymlinks(skill.Manifest)
		if err != nil || manifest != filepath.Join(root, skill.Name, "SKILL.md") || strings.ContainsAny(manifest, ")\r\n") {
			return nil, fmt.Errorf("Codex skill manifest must be in its assigned private directory")
		}
		info, err := os.Stat(manifest)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("Codex skill needs a regular SKILL.md")
		}
		seen[skill.Name] = true
		loaded = append(loaded, task.LoadedSkill{Name: skill.Name, Manifest: manifest})
	}
	return loaded, nil
}

func (a Adapter) ExecuteTask(ctx context.Context, env task.Environment, spec task.Spec) (task.Execution, error) {
	return runTurn(ctx, env, spec.Prompt(), "", nil, nil)
}

func (a Adapter) ContinueTask(ctx context.Context, env task.Environment, id, message string, restore task.RestoreHistory) (task.Execution, error) {
	return runTurn(ctx, env, []byte(message), id, restore, nil)
}

// The deployment chooses this executable, never the task or the restored workspace.
// Its single render tool is responsible for constraining its own inputs and side effects.
func (a Adapter) ExecuteFile(ctx context.Context, env task.Environment, spec task.Spec, result, session string, restore task.RestoreHistory) (task.Execution, error) {
	tool := os.Getenv("CODEX_FILE_TOOL")
	if !filepath.IsAbs(tool) || !spec.Valid() || spec.Mode != task.FileTurn || !asset.SafePath(result) || filepath.Base(result) != result {
		return task.Execution{}, fmt.Errorf("file turn requires a deployment-selected absolute CODEX_FILE_TOOL and a workspace-relative result file")
	}
	tool, err := filepath.EvalSymlinks(tool)
	if err != nil {
		return task.Execution{}, err
	}
	workspace, err := filepath.EvalSymlinks(env.Workspace)
	if err != nil {
		return task.Execution{}, err
	}
	rel, err := filepath.Rel(workspace, tool)
	if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return task.Execution{}, fmt.Errorf("CODEX_FILE_TOOL must be outside the agent workspace")
	}
	info, err := os.Stat(tool)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return task.Execution{}, fmt.Errorf("CODEX_FILE_TOOL must be an executable regular file")
	}
	settings := []string{
		"-c", "mcp_servers.report.command=" + strconv.Quote(tool),
		"-c", "mcp_servers.report.cwd=" + strconv.Quote(env.Workspace),
		"-c", "mcp_servers.report.env.REPORT_RESULT=" + strconv.Quote(result),
		"-c", "mcp_servers.report.env.REPORT_PREVIOUS=" + strconv.Quote(filepath.Join("out", spec.Output)),
		"-c", `mcp_servers.report.enabled_tools=["render"]`,
		// Approve just the tool installed by the operator. Other tools and shell
		// execution retain the read-only, non-interactive policy below.
		"-c", `mcp_servers.report.tools.render.approval_mode="approve"`,
	}
	skills, err := loadedSkills(env)
	if err != nil {
		return task.Execution{}, err
	}
	if len(skills) > 0 {
		paths := make(map[string]string, len(skills))
		for _, skill := range skills {
			paths[skill.Name] = skill.Manifest
		}
		raw, err := json.Marshal(paths)
		if err != nil {
			return task.Execution{}, err
		}
		settings = append(settings, "-c", "mcp_servers.report.env.AGENTMOOR_SKILLS="+strconv.Quote(string(raw)))
	}
	return runTurn(ctx, env, []byte(spec.Instruction), session, restore, settings)
}

func runTurn(ctx context.Context, env task.Environment, prompt []byte, id string, restore task.RestoreHistory, fileSettings []string) (task.Execution, error) {
	skills, err := loadedSkills(env)
	if err != nil {
		return task.Execution{}, err
	}
	if len(skills) > 0 {
		var nativePrompt bytes.Buffer
		nativePrompt.Write(prompt)
		nativePrompt.WriteString("\n\nUse the following pinned native skills for this turn:\n")
		for _, skill := range skills {
			fmt.Fprintf(&nativePrompt, "[$%s](%s)\n", skill.Name, skill.Manifest)
		}
		prompt = nativePrompt.Bytes()
	}
	home := filepath.Join(env.Home, ".codex")
	if err := os.MkdirAll(home, 0700); err != nil {
		return task.Execution{}, err
	}
	vars := []string{"HOME=" + env.Home, "CODEX_HOME=" + home}
	args := []string{
		"exec", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--sandbox", "read-only",
		"--disable", "shell_tool", "--disable", "multi_agent", "--disable", "apps", "--disable", "hooks",
		"-c", `web_search="disabled"`, "--json",
	}
	args = append(args, fileSettings...)
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
	result, err := decodeTurn(&out, fileSettings != nil)
	result.HistoryDir = filepath.Join(home, "sessions")
	return result, err
}

func decodeTurn(r io.Reader, fileMode bool) (task.Execution, error) {
	var result task.Execution
	var started, done, rendered bool
	dec := json.NewDecoder(r)
	for {
		var e struct {
			Type   string
			Thread string `json:"thread_id"`
			Item   struct {
				Type, Text, Server, Tool, Status string
				Error                            json.RawMessage
				Result                           *struct {
					IsError bool `json:"isError"`
				}
			}
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
			case "mcp_tool_call":
				if !fileMode || !started || e.Item.Server != "report" || e.Item.Tool != "render" ||
					e.Item.Status != "completed" || e.Item.Result == nil || e.Item.Result.IsError ||
					(len(e.Item.Error) > 0 && string(e.Item.Error) != "null") {
					return task.Execution{}, fmt.Errorf("unexpected or failed Codex file tool")
				}
				rendered = true
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
	if !started || !done || result.SessionID == "" || result.Text == "" || (fileMode && !rendered) {
		return task.Execution{}, fmt.Errorf("incomplete Codex turn")
	}
	return result, nil
}
