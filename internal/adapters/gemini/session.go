package gemini

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/adapters/sessionfile"
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
		ext := path.Ext(e.Name())
		if !strings.HasPrefix(e.Name(), "session-") || (ext != ".json" && ext != ".jsonl") {
			continue
		}
		var h struct {
			SessionID   string `json:"sessionId"`
			ProjectHash string `json:"projectHash"`
		}
		if err = sessionfile.Fields(root, e.Name(), map[string]any{"sessionId": &h.SessionID, "projectHash": &h.ProjectHash}, ""); err != nil {
			return nil, err
		}
		if !sessionfile.UUID(h.SessionID) || h.ProjectHash == "" {
			return nil, fmt.Errorf("unknown Gemini session format %q", e.Name())
		}
		files, err := sessionfile.Bundle(root, e.Name(), h.SessionID)
		if err != nil {
			return nil, err
		}
		out = append(out, adapter.NativeSession{ID: h.SessionID, Format: "gemini-chats" + ext + ".v1", Entries: files})
	}
	return out, nil
}
