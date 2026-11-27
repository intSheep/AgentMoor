package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/intSheep/AgentMoor/internal/task"
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

func TestDecodeFileTurnRequiresSuccessfulAssignedTool(t *testing.T) {
	const start = `{"type":"thread.started","thread_id":"7d65d0f5-5c08-467b-b022-97cfff41e16a"}{"type":"turn.started"}`
	const end = `{"type":"item.completed","item":{"type":"agent_message","text":"done"}}{"type":"turn.completed"}`
	const rendered = `{"type":"item.completed","item":{"type":"mcp_tool_call","server":"report","tool":"render","status":"completed","result":{"content":[{"type":"text","text":"written"}],"isError":false},"error":null}}`
	for _, tc := range []struct {
		name, event string
		file, valid bool
	}{
		{"completed render", rendered, true, true},
		{"text mode rejects render", rendered, false, false},
		{"missing render", "", true, false},
		{"failed tool", strings.Replace(rendered, `"status":"completed"`, `"status":"failed"`, 1), true, false},
		{"tool result error", strings.Replace(rendered, `"isError":false`, `"isError":true`, 1), true, false},
		{"missing result", `{"type":"item.completed","item":{"type":"mcp_tool_call","server":"report","tool":"render","status":"completed"}}`, true, false},
		{"null result", `{"type":"item.completed","item":{"type":"mcp_tool_call","server":"report","tool":"render","status":"completed","result":null}}`, true, false},
		{"native error", strings.Replace(rendered, `"error":null`, `"error":{"message":"denied"}`, 1), true, false},
		{"other server", strings.Replace(rendered, `"server":"report"`, `"server":"other"`, 1), true, false},
		{"other tool", strings.Replace(rendered, `"tool":"render"`, `"tool":"remove"`, 1), true, false},
		{"shell remains disallowed", `{"type":"item.completed","item":{"type":"command_execution","exit_code":0}}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := decodeTurn(strings.NewReader(start+tc.event+end), tc.file)
			if (err == nil) != tc.valid || (tc.valid && result.Text != "done") {
				t.Fatalf("turn = %+v, error = %v", result, err)
			}
		})
	}
	if _, err := decodeTurn(strings.NewReader(rendered+start+end), true); err == nil {
		t.Fatal("accepted a file tool before the native turn started")
	}
}

func TestFileTurnRejectsUntrustedToolAndResultPaths(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(work, "report-tool")
	outside := filepath.Join(base, "report-tool")
	for _, path := range []string{inside, outside} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "tool-alias")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	workAlias := filepath.Join(base, "work-alias")
	if err := os.Symlink(work, workAlias); err != nil {
		t.Fatal(err)
	}
	notExecutable := filepath.Join(base, "not-executable")
	if err := os.WriteFile(notExecutable, nil, 0600); err != nil {
		t.Fatal(err)
	}
	spec := task.Spec{Mode: task.FileTurn, Instruction: "render the report", Output: "report.json"}
	for _, tc := range []struct{ name, tool, workspace, result string }{
		{"relative tool", "report-tool", work, ".result-worker"},
		{"workspace tool", inside, work, ".result-worker"},
		{"symlink into workspace", link, work, ".result-worker"},
		{"symlinked workspace", inside, workAlias, ".result-worker"},
		{"non executable", notExecutable, work, ".result-worker"},
		{"directory", base, work, ".result-worker"},
		{"missing tool", filepath.Join(base, "missing"), work, ".result-worker"},
		{"absolute result", outside, work, filepath.Join(base, "result")},
		{"traversing result", outside, work, "../result"},
		{"nested result", outside, work, "out/result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_FILE_TOOL", tc.tool)
			home := t.TempDir()
			_, err := New().ExecuteFile(context.Background(), task.Environment{
				Executable: filepath.Join(base, "must-not-run"), Workspace: tc.workspace, Home: home,
			}, spec, tc.result, "", nil)
			if err == nil {
				t.Fatal("accepted an untrusted file turn")
			}
			if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
				t.Fatalf("started native setup before rejecting invalid paths: %v", err)
			}
		})
	}
}

func TestFileTurnAuthorizesOnlyAllocatedRenderAndKeepsPrivateHome(t *testing.T) {
	base := t.TempDir()
	work, home := filepath.Join(base, "work"), filepath.Join(base, "home")
	for _, path := range []string{work, home} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	tool := filepath.Join(base, "report tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	canonicalTool, err := filepath.EvalSymlinks(tool)
	if err != nil {
		t.Fatal(err)
	}
	argsPath, envPath, promptPath := filepath.Join(base, "args"), filepath.Join(base, "env"), filepath.Join(base, "prompt")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	cli := filepath.Join(base, "fake-codex")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + quote(argsPath) + "\n" +
		"/usr/bin/env > " + quote(envPath) + "\n" +
		"/bin/cat > " + quote(promptPath) + "\n" +
		"/bin/cat <<'EVENTS'\n" +
		`{"type":"thread.started","thread_id":"7d65d0f5-5c08-467b-b022-97cfff41e16a"}` + "\n" +
		`{"type":"turn.started"}` + "\n" +
		`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"report","tool":"render","status":"completed","result":{"isError":false}}}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}` + "\n" +
		`{"type":"turn.completed"}` + "\nEVENTS\n"
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_FILE_TOOL", tool)
	t.Setenv("CODEX_HOME", filepath.Join(base, "unrelated-native-state"))
	t.Setenv("AGENTMOOR_TOKEN", "platform-secret-must-not-reach-cli")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "storage-secret-must-not-reach-cli")
	t.Setenv("OPENAI_API_KEY", "synthetic-model-key")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("CODEX_MODEL", "")
	spec := task.Spec{Mode: task.FileTurn, Instruction: "render from the allocated files", Input: "must stay in the workspace", Output: "report.json"}
	result, err := New().ExecuteFile(context.Background(), task.Environment{Executable: cli, Workspace: work, Home: home}, spec, ".result-worker-assigned", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "7d65d0f5-5c08-467b-b022-97cfff41e16a" || result.Text != "done" || result.HistoryDir != filepath.Join(home, ".codex", "sessions") {
		t.Fatalf("execution = %+v", result)
	}
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	args := strings.Split(strings.TrimSuffix(read(argsPath), "\n"), "\n")
	for _, pair := range [][2]string{
		{"--sandbox", "read-only"}, {"--disable", "shell_tool"}, {"--disable", "apps"}, {"--disable", "hooks"},
		{"-c", "mcp_servers.report.command=" + strconv.Quote(canonicalTool)},
		{"-c", "mcp_servers.report.cwd=" + strconv.Quote(work)},
		{"-c", `mcp_servers.report.enabled_tools=["render"]`},
		{"-c", `mcp_servers.report.tools.render.approval_mode="approve"`},
		{"-c", `mcp_servers.report.env.REPORT_RESULT=".result-worker-assigned"`},
		{"-c", `mcp_servers.report.env.REPORT_PREVIOUS="out/report.json"`},
	} {
		found := false
		for i := 0; i+1 < len(args); i++ {
			found = found || (args[i] == pair[0] && args[i+1] == pair[1])
		}
		if !found {
			t.Errorf("missing argument pair %q in %q", pair, args)
		}
	}
	for _, arg := range []string{"--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--json"} {
		if !slices.Contains(args, arg) {
			t.Errorf("missing isolation option %q", arg)
		}
	}
	for _, arg := range args {
		if strings.Contains(arg, "danger") || arg == "--trust" || strings.Contains(arg, "default_tools_approval_mode") {
			t.Errorf("file turn broadened native permissions: %q", arg)
		}
	}
	if args[0] != "exec" || args[len(args)-1] != "-" || read(promptPath) != spec.Instruction {
		t.Fatalf("unexpected native invocation: args=%q prompt=%q", args, read(promptPath))
	}
	vars := strings.Split(strings.TrimSpace(read(envPath)), "\n")
	for _, want := range []string{"HOME=" + home, "CODEX_HOME=" + filepath.Join(home, ".codex"), "OPENAI_API_KEY=synthetic-model-key"} {
		if !slices.Contains(vars, want) {
			t.Errorf("missing private native environment %q", want)
		}
	}
	for _, variable := range vars {
		if strings.HasPrefix(variable, "AGENTMOOR_TOKEN=") || strings.HasPrefix(variable, "AWS_SECRET_ACCESS_KEY=") || strings.HasPrefix(variable, "CODEX_FILE_TOOL=") {
			t.Errorf("unexpected inherited credential or tool setting: %q", variable)
		}
	}
}

type skillHistory struct{ destination string }

func (h *skillHistory) Files(destination string) error {
	h.destination = destination
	return os.Mkdir(destination, 0700)
}

func (h *skillHistory) NativeDatabase(string) error { return os.ErrInvalid }

func TestPinnedSkillsUseCanonicalNativeMentionsForEveryTurn(t *testing.T) {
	for _, mode := range []string{"text", "file", "continue"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			work, home := filepath.Join(base, "work"), filepath.Join(base, "home")
			for _, dir := range []string{work, home} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			alias := filepath.Join(base, "home-link")
			if err := os.Symlink(home, alias); err != nil {
				t.Fatal(err)
			}
			env := task.Environment{Home: alias, Workspace: work}
			dir, err := New().SkillDirectory(env, "report-style")
			if err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(dir, "SKILL.md")
			body := []byte("---\nname: report-style\ndescription: A pinned report rule\n---\noriginal-body-must-be-loaded-by-codex\n")
			if err = os.WriteFile(manifest, body, 0600); err != nil {
				t.Fatal(err)
			}
			canonical, err := filepath.EvalSymlinks(manifest)
			if err != nil {
				t.Fatal(err)
			}
			env.Skills = []task.LoadedSkill{{Name: "report-style", Manifest: manifest}}
			argsPath, promptPath := filepath.Join(base, "args"), filepath.Join(base, "prompt")
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
			env.Executable = filepath.Join(base, "fake-codex")
			events := `{"type":"thread.started","thread_id":"7d65d0f5-5c08-467b-b022-97cfff41e16a"}` + "\n" + `{"type":"turn.started"}` + "\n"
			if mode == "file" {
				events += `{"type":"item.completed","item":{"type":"mcp_tool_call","server":"report","tool":"render","status":"completed","result":{"isError":false}}}` + "\n"
			}
			events += `{"type":"item.completed","item":{"type":"agent_message","text":"done"}}` + "\n" + `{"type":"turn.completed"}` + "\n"
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + quote(argsPath) + "\n/bin/cat > " + quote(promptPath) + "\n/bin/cat <<'EVENTS'\n" + events + "EVENTS\n"
			if err = os.WriteFile(env.Executable, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			spec := task.Spec{Instruction: "Produce the report", Input: "workspace input", Output: "report.json"}
			original := spec.Instruction
			var history skillHistory
			switch mode {
			case "text":
				original = string(spec.Prompt())
				_, err = New().ExecuteTask(t.Context(), env, spec)
			case "continue":
				_, err = New().ContinueTask(t.Context(), env, "7d65d0f5-5c08-467b-b022-97cfff41e16a", spec.Instruction, &history)
				if history.destination != filepath.Join(alias, ".codex", "sessions") {
					t.Fatalf("restored history to %q", history.destination)
				}
			case "file":
				tool := filepath.Join(base, "trusted-tool")
				if err = os.WriteFile(tool, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CODEX_FILE_TOOL", tool)
				spec.Mode = task.FileTurn
				_, err = New().ExecuteFile(t.Context(), env, spec, ".result-worker", "", nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			prompt, err := os.ReadFile(promptPath)
			if err != nil {
				t.Fatal(err)
			}
			mention := "[$report-style](" + canonical + ")"
			if !strings.HasPrefix(string(prompt), original+"\n\n") || strings.Count(string(prompt), mention) != 1 || strings.Contains(string(prompt), "original-body-must-be-loaded-by-codex") {
				t.Fatalf("expected canonical native selection without rewritten Skill contents: %s", prompt)
			}
			args, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(args), "--sandbox\nread-only\n") || !strings.Contains(string(args), "--disable\nshell_tool\n") || strings.Contains(string(args), "danger") {
				t.Fatalf("native skill selection changed runtime restrictions: %s", args)
			}
			if mode == "file" {
				raw, err := json.Marshal(map[string]string{"report-style": canonical})
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(args), "mcp_servers.report.env.AGENTMOOR_SKILLS="+strconv.Quote(string(raw))) {
					t.Fatalf("trusted tool did not receive the verified Skill path: %s", args)
				}
			} else if strings.Contains(string(args), "AGENTMOOR_SKILLS") {
				t.Fatal("text mode supplied MCP-only Skill environment")
			}
		})
	}
}

func TestPinnedSkillsRejectUnassignedOrEscapingManifests(t *testing.T) {
	for _, scenario := range []string{"outside home", "workspace", "wrong package", "relative manifest", "duplicate", "invalid name", "symlink manifest", "symlink skill root", "directory manifest"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			home, work := filepath.Join(base, "home"), filepath.Join(base, "work")
			root := filepath.Join(home, ".codex", "skills")
			manifest := filepath.Join(root, "demo", "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(manifest), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(work, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifest, []byte("original Skill"), 0600); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(base, "SKILL.md")
			if err := os.WriteFile(outside, []byte("unassigned Skill"), 0600); err != nil {
				t.Fatal(err)
			}
			env := task.Environment{Home: home, Workspace: work, Skills: []task.LoadedSkill{{Name: "demo", Manifest: manifest}}}
			switch scenario {
			case "outside home":
				env.Skills[0].Manifest = outside
			case "workspace":
				env.Skills[0].Manifest = filepath.Join(work, "SKILL.md")
				if err := os.WriteFile(env.Skills[0].Manifest, []byte("workspace Skill"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong package":
				env.Skills[0].Name = "other"
			case "relative manifest":
				env.Skills[0].Manifest = ".codex/skills/demo/SKILL.md"
			case "duplicate":
				env.Skills = append(env.Skills, env.Skills[0])
			case "invalid name":
				env.Skills[0].Name = "../demo"
			case "symlink manifest":
				if err := os.Remove(manifest); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, manifest); err != nil {
					t.Fatal(err)
				}
			case "symlink skill root":
				moved := filepath.Join(base, "moved-skills")
				if err := os.Rename(root, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, root); err != nil {
					t.Fatal(err)
				}
			case "directory manifest":
				if err := os.Remove(manifest); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(manifest, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadedSkills(env); err == nil {
				t.Fatal("accepted an unassigned native Skill")
			}
		})
	}
	for _, name := range []string{"", "../demo", "demo/name", "UPPER", "demo)\nother"} {
		if _, err := New().SkillDirectory(task.Environment{Home: t.TempDir()}, name); err == nil {
			t.Fatalf("accepted invalid native Skill name %q", name)
		}
	}
}
