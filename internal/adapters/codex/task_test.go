package codex

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"agentmoor/internal/task"
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
