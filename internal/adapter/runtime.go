package adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// NativeSessions is optional. The operator supplies the executable path and a
// dedicated runtime home. Import always receives a fresh data directory.
type NativeSessions interface {
	ExportNative(context.Context, string, string, string) error
	ImportNative(context.Context, string, string, string, string) error
}

type RuntimeInfo struct {
	Descriptor     Descriptor `json:"adapter"`
	Evidence       []string   `json:"evidence"`
	RuntimeVersion string     `json:"runtime_version,omitempty"`
	Tested         bool       `json:"matches_tested_version"`
}

func Inspect(ctx context.Context, a Adapter, root *os.Root, exe string) (RuntimeInfo, error) {
	info := RuntimeInfo{Descriptor: a.Descriptor()}
	evidence, err := a.Detect(ctx, root)
	if err != nil {
		return info, err
	}
	info.Evidence = evidence
	if exe == "" {
		return info, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var b bytes.Buffer
	// Some CLIs initialize their data directory even for --version. Keep the
	// probe away from both live user state and a yet-to-be-restored target.
	probe, err := os.MkdirTemp("", "agentmoor-version-*")
	if err != nil {
		return info, err
	}
	defer os.RemoveAll(probe)
	var env []string
	for _, key := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "GEMINI_CLI_HOME"} {
		env = append(env, key+"="+probe)
	}
	if err = RunCLI(ctx, exe, probe, env, &b, 4096, "--version"); err != nil {
		return info, err
	}
	info.RuntimeVersion = strings.TrimSpace(b.String())
	info.Tested = info.RuntimeVersion == info.Descriptor.TestedRuntime
	return info, nil
}

type outputLimit struct {
	dst       io.Writer
	remaining int64
}

func (w *outputLimit) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("runtime output exceeds limit")
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	return n, err
}

// RunCLI invokes an explicitly trusted executable with no shell or platform credentials.
func RunCLI(ctx context.Context, exe, workspace string, env []string, out io.Writer, limit int64, args ...string) error {
	return RunCLIInput(ctx, exe, workspace, env, nil, out, limit, args...)
}

func RunCLIInput(ctx context.Context, exe, workspace string, env []string, input io.Reader, out io.Writer, limit int64, args ...string) error {
	if !filepath.IsAbs(exe) {
		return fmt.Errorf("runtime_cli must be an absolute executable path")
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	// Node and native launchers can fork. Cancel every process in this CLI's group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Dir = workspace
	cmd.Stdin = input
	cmd.WaitDelay = 3 * time.Second
	inherited := []string{"PATH", "TMPDIR", "LANG"}
	// A supplied HOME starts a private allocation. Only native sync against an
	// existing installation inherits that installation's state directories.
	if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "HOME=") }) {
		inherited = append(inherited, "HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "GEMINI_CLI_HOME")
	}
	for _, name := range inherited {
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	cmd.Env = append(cmd.Env, "OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_MODELS_FETCH=1", "NO_COLOR=1")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdout = &outputLimit{out, limit}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("runtime %s: %w", args[0], err)
	}
	return nil
}
