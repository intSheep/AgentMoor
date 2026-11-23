package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"agentmoor/internal/adapter"
	"agentmoor/internal/adapters/sessionfile"
	"agentmoor/internal/asset"
)

func (a Adapter) ExportNative(ctx context.Context, exe, workspace, dir string) error {
	var list bytes.Buffer
	if err := adapter.RunCLI(ctx, exe, workspace, nil, &list, 16<<20, "session", "list", "--format", "json", "--pure"); err != nil {
		return err
	}
	if len(bytes.TrimSpace(list.Bytes())) == 0 {
		return nil
	}
	var rows []struct{ ID, Directory string }
	if err := json.Unmarshal(list.Bytes(), &rows); err != nil {
		return err
	}
	cwd, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if !filepath.IsAbs(r.Directory) {
			return fmt.Errorf("native session has no absolute project directory")
		}
		resolved, err := filepath.EvalSymlinks(r.Directory)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if resolved != cwd {
			continue
		}
		if err := exportSession(ctx, exe, cwd, nil, dir, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// Both capture paths use the native export format and bind it to one validated ID.
func exportSession(ctx context.Context, exe, workspace string, vars []string, dir, id string) error {
	if !sessionfile.OpenCodeID.MatchString(id) {
		return fmt.Errorf("unexpected native session ID")
	}
	path := filepath.Join(dir, id+".json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	runErr := adapter.RunCLI(ctx, exe, workspace, vars, f, asset.MaxBytes, "export", id, "--pure")
	if err = errors.Join(runErr, f.Close()); err != nil {
		os.Remove(path)
	}
	return err
}

func (a Adapter) ImportNative(ctx context.Context, exe, workspace, file, freshState string) error {
	return adapter.RunCLI(ctx, exe, workspace, []string{"XDG_DATA_HOME=" + freshState}, io.Discard, 1<<20, "import", file, "--pure")
}
