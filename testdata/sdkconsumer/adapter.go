// Package sdkconsumer is a maintainer-written, synthetic external consumer.
// Its module path and temporary build directory are both outside AgentMoor.
package sdkconsumer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/intSheep/AgentMoor/sdk"
)

const (
	adapterID     = "external-sdk-native"
	testedVersion = "sdk-consumer-fixture 1"
	sessionID     = "external_session_42"
	sessionFormat = "external-fixture-json.v1"
	skillManifest = ".consumer/skills/greet/SKILL.md"
)

// consumerAdapter owns its format, marker and native commands. It does not
// embed an existing AgentMoor adapter or import its implementation package.
type consumerAdapter struct{}

var (
	_ sdk.Adapter         = consumerAdapter{}
	_ sdk.SkillDiscoverer = consumerAdapter{}
	_ sdk.SessionSource   = consumerAdapter{}
	_ sdk.NativeSessions  = consumerAdapter{}
	// These assertions check the public task-parameter type closure only. The
	// descriptor deliberately advertises no untested task execution modes.
	_ sdk.Executor       = consumerAdapter{}
	_ sdk.Continuation   = consumerAdapter{}
	_ sdk.FileExecutor   = consumerAdapter{}
	_ sdk.SkillInstaller = consumerAdapter{}
)

func (consumerAdapter) Descriptor() sdk.Descriptor {
	return sdk.Descriptor{ID: adapterID, Version: "synthetic.1", TestedRuntime: testedVersion,
		Capabilities: map[sdk.AssetKind][]sdk.Capability{
			sdk.Session:   {{Operation: "export", Mode: "runtime-cli", Reliability: "synthetic-fixture-only"}, {Operation: "restore", Mode: "runtime-cli", Reliability: "synthetic-fixture-only"}},
			sdk.Skill:     {{Operation: "discover", Mode: "filesystem", Reliability: "synthetic-fixture-only"}},
			sdk.Artifact:  {{Operation: "publish", Mode: "immutable-object", Reliability: "synthetic-fixture-only"}},
			sdk.Workspace: {{Operation: "snapshot", Mode: "filesystem", Reliability: "synthetic-fixture-only"}},
		}}
}

func (consumerAdapter) Detect(ctx context.Context, root *os.Root) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := root.Lstat(".consumer/marker")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("invalid synthetic marker")
	}
	return []string{".consumer/marker"}, nil
}

func (consumerAdapter) DiscoverSkills(ctx context.Context, root *os.Root) ([]sdk.SkillRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := root.Lstat(skillManifest)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("invalid synthetic Skill")
	}
	return []sdk.SkillRef{{Manifest: skillManifest}}, nil
}

func (consumerAdapter) Sessions(ctx context.Context, root *os.Root) ([]sdk.NativeSession, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	var sessions []sdk.NativeSession
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, errors.New("unexpected synthetic Session type")
		}
		raw, err := root.ReadFile(entry.Name())
		var document struct{ ID string }
		if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &document) != nil || document.ID != sessionID {
			return nil, errors.New("invalid original synthetic Session")
		}
		sessions = append(sessions, sdk.NativeSession{ID: document.ID, Format: sessionFormat, Raw: true,
			Entries: []sdk.Entry{{Source: entry.Name(), Name: entry.Name()}}})
	}
	return sessions, nil
}

func (consumerAdapter) ExportNative(ctx context.Context, exe, workspace, directory string) error {
	var list bytes.Buffer
	// These are this consumer's own commands, not the builtin OpenCode argv.
	if err := sdk.RunCLI(ctx, exe, workspace, nil, &list, 4096, "list"); err != nil {
		return err
	}
	var rows []struct{ ID, Directory string }
	if err := json.Unmarshal(list.Bytes(), &rows); err != nil {
		return err
	}
	assigned, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return err
	}
	for _, row := range rows {
		resolved, err := filepath.EvalSymlinks(row.Directory)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if resolved != assigned {
			continue
		}
		if row.ID != sessionID {
			return errors.New("unexpected synthetic export identity")
		}
		name := filepath.Join(directory, row.ID+".json")
		file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		runErr := sdk.RunCLI(ctx, exe, assigned, nil, file, 4096, "dump", row.ID)
		if err = errors.Join(runErr, file.Close()); err != nil {
			_ = os.Remove(name)
			return err
		}
	}
	return nil
}

func (consumerAdapter) ImportNative(ctx context.Context, exe, workspace, file, freshState string) error {
	return sdk.RunCLI(ctx, exe, workspace, []string{"XDG_DATA_HOME=" + freshState}, io.Discard, 1024, "load", file)
}

var errCompileOnly = errors.New("synthetic SDK task method is compile-only")

func (consumerAdapter) ExecuteTask(context.Context, sdk.Environment, sdk.Spec) (sdk.Execution, error) {
	return sdk.Execution{}, errCompileOnly
}
func (consumerAdapter) ContinueTask(context.Context, sdk.Environment, string, string, sdk.RestoreHistory) (sdk.Execution, error) {
	return sdk.Execution{}, errCompileOnly
}
func (consumerAdapter) ExecuteFile(context.Context, sdk.Environment, sdk.Spec, string, string, sdk.RestoreHistory) (sdk.Execution, error) {
	return sdk.Execution{}, errCompileOnly
}
func (consumerAdapter) SkillDirectory(sdk.Environment, string) (string, error) {
	return "", errCompileOnly
}
