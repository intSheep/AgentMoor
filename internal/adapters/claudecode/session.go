package claudecode

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"agentmoor/internal/adapter"
	"agentmoor/internal/adapters/sessionfile"
)

func sessions(ctx context.Context, root *os.Root) ([]adapter.NativeSession, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	var out []adapter.NativeSession
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || !sessionfile.UUID(id) {
			continue
		}
		if !e.Type().IsRegular() {
			return nil, fmt.Errorf("unsupported transcript type %q", e.Name())
		}
		var header map[string]any
		if err = sessionfile.Header(root, e.Name(), &header); err != nil {
			return nil, err
		}
		if _, ok := header["type"].(string); !ok {
			return nil, fmt.Errorf("unknown Claude transcript %q", e.Name())
		}
		files, err := sessionfile.Bundle(root, e.Name(), id)
		if err != nil {
			return nil, err
		}
		out = append(out, adapter.NativeSession{ID: id, Format: "claude-project-jsonl.v1", Entries: files})
	}
	return out, nil
}
