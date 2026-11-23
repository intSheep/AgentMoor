package opencode

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"

	"agentmoor/internal/adapter"
	"agentmoor/internal/adapters/sessionfile"
	"agentmoor/internal/snapshot"
)

func sessions(ctx context.Context, root *os.Root) ([]adapter.NativeSession, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	var out []adapter.NativeSession
	for _, e := range entries {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if e.IsDir() || path.Ext(e.Name()) != ".json" {
			continue
		}
		var info struct{ ID string }
		if err = sessionfile.Fields(root, e.Name(), map[string]any{"info": &info}, "messages"); err != nil {
			return nil, err
		}
		if !sessionfile.OpenCodeID.MatchString(info.ID) {
			return nil, fmt.Errorf("unknown OpenCode export %q", e.Name())
		}
		out = append(out, adapter.NativeSession{ID: info.ID, Format: "opencode-export-json.v1", Raw: true, Entries: []snapshot.Entry{{Source: e.Name(), Name: e.Name()}}})
	}
	return out, nil
}
