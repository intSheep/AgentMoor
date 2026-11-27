package sdk_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/snapshot"
	"github.com/intSheep/AgentMoor/internal/task"
	"github.com/intSheep/AgentMoor/sdk"
)

func TestSDKTypeIdentity(t *testing.T) {
	for _, pair := range []struct {
		name         string
		public, core reflect.Type
	}{
		{"Adapter", reflect.TypeFor[sdk.Adapter](), reflect.TypeFor[adapter.Adapter]()},
		{"Descriptor", reflect.TypeFor[sdk.Descriptor](), reflect.TypeFor[adapter.Descriptor]()},
		{"Capability", reflect.TypeFor[sdk.Capability](), reflect.TypeFor[adapter.Capability]()},
		{"AssetKind", reflect.TypeFor[sdk.AssetKind](), reflect.TypeFor[adapter.AssetKind]()},
		{"SkillRef", reflect.TypeFor[sdk.SkillRef](), reflect.TypeFor[adapter.SkillRef]()},
		{"SkillDiscoverer", reflect.TypeFor[sdk.SkillDiscoverer](), reflect.TypeFor[adapter.SkillDiscoverer]()},
		{"SessionSource", reflect.TypeFor[sdk.SessionSource](), reflect.TypeFor[adapter.SessionSource]()},
		{"NativeSession", reflect.TypeFor[sdk.NativeSession](), reflect.TypeFor[adapter.NativeSession]()},
		{"NativeSessions", reflect.TypeFor[sdk.NativeSessions](), reflect.TypeFor[adapter.NativeSessions]()},
		{"Registry", reflect.TypeFor[sdk.Registry](), reflect.TypeFor[adapter.Registry]()},
		{"RuntimeInfo", reflect.TypeFor[sdk.RuntimeInfo](), reflect.TypeFor[adapter.RuntimeInfo]()},
		{"Entry", reflect.TypeFor[sdk.Entry](), reflect.TypeFor[snapshot.Entry]()},
		{"Executor", reflect.TypeFor[sdk.Executor](), reflect.TypeFor[task.Executor]()},
		{"Continuation", reflect.TypeFor[sdk.Continuation](), reflect.TypeFor[task.Continuation]()},
		{"FileExecutor", reflect.TypeFor[sdk.FileExecutor](), reflect.TypeFor[task.FileExecutor]()},
		{"SkillInstaller", reflect.TypeFor[sdk.SkillInstaller](), reflect.TypeFor[task.SkillInstaller]()},
		{"Environment", reflect.TypeFor[sdk.Environment](), reflect.TypeFor[task.Environment]()},
		{"Spec", reflect.TypeFor[sdk.Spec](), reflect.TypeFor[task.Spec]()},
		{"Execution", reflect.TypeFor[sdk.Execution](), reflect.TypeFor[task.Execution]()},
		{"LoadedSkill", reflect.TypeFor[sdk.LoadedSkill](), reflect.TypeFor[task.LoadedSkill]()},
		{"RestoreHistory", reflect.TypeFor[sdk.RestoreHistory](), reflect.TypeFor[task.RestoreHistory]()},
	} {
		t.Run(pair.name, func(t *testing.T) {
			if pair.public != pair.core {
				t.Fatal("public contract introduced a different Core type identity", pair.public, pair.core)
			}
		})
	}
	if sdk.Session != adapter.Session || sdk.Skill != adapter.Skill || sdk.Artifact != adapter.Artifact || sdk.Workspace != adapter.Workspace ||
		sdk.TextTurn != task.TextTurn || sdk.ContinueTurn != task.ContinueTurn || sdk.FileTurn != task.FileTurn {
		t.Fatal("SDK changed the existing kind or task-mode identifiers")
	}
	// Alias methods and the nested Entry type remain usable through sdk alone.
	spec := sdk.Spec{Mode: sdk.TextTurn, Instruction: "synthetic", Input: "original bytes", Output: "reply.txt"}
	if !spec.Valid() || !bytes.Contains(spec.Prompt(), []byte("original bytes")) {
		t.Fatal("aliased Spec lost its existing methods")
	}
	var session adapter.NativeSession = sdk.NativeSession{ID: "synthetic", Entries: []sdk.Entry{{Source: "history", Name: "history"}}}
	if session.Entries[0].Name != "history" {
		t.Fatal("nested Session entry did not retain its identity")
	}
}

type sdkFixtureAdapter struct{ descriptor sdk.Descriptor }

func (a *sdkFixtureAdapter) Descriptor() sdk.Descriptor { return a.descriptor }
func (*sdkFixtureAdapter) Detect(_ context.Context, root *os.Root) ([]string, error) {
	if _, err := root.Lstat(".sdk-fixture"); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return []string{".sdk-fixture"}, nil
}

func TestSDKRegistryPreservesSuppliedObjectAndExistingChecks(t *testing.T) {
	a := &sdkFixtureAdapter{sdk.Descriptor{ID: "a", Version: "synthetic-build"}}
	z := &sdkFixtureAdapter{sdk.Descriptor{ID: "z", Version: "synthetic-build"}}
	r, err := sdk.NewRegistry(z, a)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Lookup("z")
	if err != nil || got != z {
		t.Fatal("registry translated or replaced the supplied object", err)
	}
	if all := r.All(); len(all) != 2 || all[0] != a || all[1] != z {
		t.Fatal("registry lost its existing deterministic ordering")
	}
	if _, err := sdk.NewRegistry(a, a); err == nil {
		t.Fatal("duplicate identity passed")
	}
	for _, d := range []sdk.Descriptor{{ID: "missing-version"}, {Version: "missing-id"}} {
		if _, err := sdk.NewRegistry(&sdkFixtureAdapter{d}); err == nil {
			t.Fatal("missing registry identifier passed")
		}
	}
	if _, err := r.Lookup("unknown"); err == nil {
		t.Fatal("unknown lookup passed")
	}
}

func TestSDKInspectKeepsPrivateProbeAndExactVersionReport(t *testing.T) {
	work, runtimeDir, ambient := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(work, ".sdk-fixture"), []byte("original marker"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, key := range []string{"AGENTMOOR_TOKEN", "AGENTMOOR_TOKEN_FILE", "AGENTMOOR_POSTGRES_DSN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "KUBECONFIG"} {
		t.Setenv(key, "synthetic-ambient-secret")
	}
	t.Setenv("HOME", ambient)
	t.Setenv("CODEX_HOME", ambient)
	exe := filepath.Join(runtimeDir, "synthetic-runtime")
	script := `#!/bin/sh
[ "$1" = "--version" ] || exit 2
[ "$HOME" = "$CODEX_HOME" ] && [ "$HOME" = "$XDG_DATA_HOME" ] || exit 3
[ -z "${AGENTMOOR_TOKEN:-}${AGENTMOOR_TOKEN_FILE:-}${AGENTMOOR_POSTGRES_DSN:-}${AWS_ACCESS_KEY_ID:-}${AWS_SECRET_ACCESS_KEY:-}${KUBECONFIG:-}" ] || exit 4
printf '%s\n' "$PWD" "$HOME" > "$0.probe"
printf '%s\n' 'synthetic-sdk 1'
`
	if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := &sdkFixtureAdapter{sdk.Descriptor{ID: "synthetic-sdk", Version: "fixture-build", TestedRuntime: "synthetic-sdk 1"}}
	info, err := sdk.Inspect(t.Context(), a, root, "")
	if err != nil || info.Tested || info.RuntimeVersion != "" || !reflect.DeepEqual(info.Evidence, []string{".sdk-fixture"}) {
		t.Fatal("metadata-only inspection changed its meaning", info, err)
	}
	if _, err := os.Stat(exe + ".probe"); !os.IsNotExist(err) {
		t.Fatal("metadata-only inspection launched the runtime", err)
	}
	info, err = sdk.Inspect(t.Context(), a, root, exe)
	if err != nil || !info.Tested || info.RuntimeVersion != "synthetic-sdk 1" || !reflect.DeepEqual(info.Descriptor, a.Descriptor()) {
		t.Fatal("matching pinned version was not reported", info, err)
	}
	probe, err := os.ReadFile(exe + ".probe")
	if err != nil {
		t.Fatal(err)
	}
	probePaths := strings.Split(strings.TrimSpace(string(probe)), "\n")
	if len(probePaths) != 2 {
		t.Fatal("synthetic probe did not record its actual cwd and assigned home")
	}
	probePath, probeHome := probePaths[0], probePaths[1]
	// macOS may spell the same temporary parent as /var and /private/var.
	// Resolve its still-existing parent after Inspect removed the probe leaf.
	parent, err := filepath.EvalSymlinks(filepath.Dir(probeHome))
	if err != nil {
		t.Fatal(err)
	}
	if probePath != filepath.Join(parent, filepath.Base(probeHome)) || probeHome == work || probeHome == ambient || !filepath.IsAbs(probeHome) {
		t.Fatal("version probe did not use its assigned private home as cwd")
	}
	if _, err := os.Stat(probeHome); !os.IsNotExist(err) {
		t.Fatal("private version probe directory remained after inspection", err)
	}
	a.descriptor.TestedRuntime = "synthetic-sdk 2"
	info, err = sdk.Inspect(t.Context(), a, root, exe)
	if err != nil || info.Tested || info.RuntimeVersion != "synthetic-sdk 1" {
		t.Fatal("Inspect hid mismatch or silently imposed a new rejection policy", info, err)
	}
	if _, err := sdk.Inspect(t.Context(), a, root, "relative-runtime"); err == nil {
		t.Fatal("inspection accepted a relative executable")
	}
	if marker, err := root.ReadFile(".sdk-fixture"); err != nil || string(marker) != "original marker" {
		t.Fatal("inspection changed the workspace marker", err)
	}
	if entries, err := os.ReadDir(work); err != nil || len(entries) != 1 {
		t.Fatal("version probe wrote into the workspace", err)
	}
}

func TestSDKTrustedCLIWrappersKeepInputLimitsAndCancellation(t *testing.T) {
	work, home := t.TempDir(), t.TempDir()
	t.Setenv("AGENTMOOR_TOKEN", "synthetic-ambient-secret")
	var output bytes.Buffer
	raw := "original\x00stdin\n"
	if err := sdk.RunCLIInput(t.Context(), "/bin/sh", work, []string{"HOME=" + home}, strings.NewReader(raw), &output, 1024,
		"-c", `[ -z "${AGENTMOOR_TOKEN:-}" ] || exit 2; cat`); err != nil || output.String() != raw {
		t.Fatal("public CLI input or inherited environment changed", err)
	}
	output.Reset()
	if err := sdk.RunCLI(t.Context(), "/bin/sh", work, []string{"HOME=" + home}, &output, 3, "-c", "printf bounded"); err == nil || output.Len() > 3 {
		t.Fatal("public CLI wrapper ignored its stdout limit", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sdk.RunCLI(ctx, "/bin/sh", work, nil, io.Discard, 1024, "-c", "touch unexpected"); !errors.Is(err, context.Canceled) {
		t.Fatal("public CLI wrapper lost caller cancellation", err)
	}
	if _, err := os.Stat(filepath.Join(work, "unexpected")); !os.IsNotExist(err) {
		t.Fatal("cancelled public CLI wrapper started the command", err)
	}
}

func TestSDKTrustedCLIWithoutArgumentsReturnsOrdinaryResult(t *testing.T) {
	work := t.TempDir()
	missing := filepath.Join(t.TempDir(), "not-an-executable")
	if err := sdk.RunCLI(t.Context(), missing, work, nil, io.Discard, 1024); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("zero-argument failure panicked or lost its ordinary error", err)
	}
	exe := filepath.Join(t.TempDir(), "synthetic-no-arguments")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n[ \"$#\" -eq 0 ] || exit 2\nprintf no-argument-success\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := sdk.RunCLI(t.Context(), exe, work, nil, &output, 1024); err != nil || output.String() != "no-argument-success" {
		t.Fatal("successful zero-argument invocation was rejected or changed", err)
	}
}
