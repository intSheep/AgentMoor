package worker

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"agentmoor/internal/adapter"
	"agentmoor/internal/asset"
	"agentmoor/internal/snapshot"
	"agentmoor/internal/task"
)

func (e *Engine) Restore(ctx context.Context, k asset.Key, version int64, dest string) error {
	g, err := e.authorize(ctx)
	if err != nil {
		return err
	}
	if !g.Read {
		return fmt.Errorf("restore requires read permission")
	}
	rev, r, err := e.Client.Content(ctx, k, version)
	if err != nil {
		return err
	}
	defer r.Close()
	if rev.Scope != g.Scope {
		return fmt.Errorf("revision scope mismatch")
	}
	return snapshot.Restore(ctx, r, rev.Metadata, dest, rev.MediaType == "application/x-tar" && rev.Kind != "artifact")
}

func (e *Engine) RestoreNative(ctx context.Context, id string, version int64, dest string) error {
	native, ok := e.Adapter.(adapter.NativeSessions)
	if !ok || e.Config.RuntimeCLI == "" {
		return fmt.Errorf("native import requires a supported adapter and runtime_cli")
	}
	g, err := e.authorize(ctx)
	if err != nil {
		return err
	}
	if !g.Read {
		return fmt.Errorf("restore requires read permission")
	}
	root, err := os.OpenRoot(e.Config.Workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := adapter.Inspect(ctx, e.Adapter, root, e.Config.RuntimeCLI)
	if err != nil {
		return err
	}
	if !info.Tested || (e.Config.RuntimeVersion != "" && info.RuntimeVersion != e.Config.RuntimeVersion) {
		return fmt.Errorf("unverified native importer %q", info.RuntimeVersion)
	}
	temp, err := os.MkdirTemp(filepath.Dir(dest), ".agentmoor-input-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	file := filepath.Join(temp, "session.json")
	if err = e.Restore(ctx, asset.Key{Kind: "session", ID: id}, version, file); err != nil {
		return err
	}
	return snapshot.BuildDirectory(dest, func(stage string) error {
		return native.ImportNative(ctx, e.Config.RuntimeCLI, e.Config.Workspace, file, stage)
	})
}

func (e *Engine) RestoreSessions(ctx context.Context, dest string) error {
	g, err := e.authorize(ctx)
	if err != nil {
		return err
	}
	if !g.Read {
		return fmt.Errorf("restore requires read permission")
	}
	var heads []asset.Revision
	if err = e.Client.JSON(ctx, "GET", "/v1/heads", nil, &heads); err != nil {
		return err
	}
	// When several immutable turn bundles contain the same native ID, select
	// the last successfully committed conversation checkpoint for a bulk import.
	var cp task.Checkpoint
	for _, h := range heads {
		if h.Kind == "session" && strings.HasPrefix(h.ID, "turns/") {
			if err := e.Client.JSON(ctx, "GET", "/v1/tasks/"+url.PathEscape(g.Scope.Runtime)+"/checkpoint", nil, &cp); err != nil {
				return err
			}
			break
		}
	}
	return snapshot.BuildDirectory(dest, func(stage string) error {
		root, err := os.OpenRoot(stage)
		if err != nil {
			return err
		}
		defer root.Close()
		for _, item := range heads {
			if item.Kind != "session" || strings.HasPrefix(item.ID, "turns/") {
				continue
			}
			key, version := item.Key, item.Version
			if item.ID == cp.SessionID {
				key, version = cp.Session.Key, cp.Session.Version
			}
			rev, stream, err := e.Client.Content(ctx, key, version)
			if err != nil {
				return err
			}
			if rev.Scope != g.Scope {
				stream.Close()
				return fmt.Errorf("session scope mismatch")
			}
			rev.Metadata.ID = item.ID
			err = snapshot.AddSession(ctx, root, stream, rev.Metadata, filepath.Dir(stage))
			stream.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
}
