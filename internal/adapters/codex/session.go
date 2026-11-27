package codex

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/adapters/sessionfile"
	"github.com/intSheep/AgentMoor/internal/snapshot"
)

func sessions(ctx context.Context, root *os.Root) ([]adapter.NativeSession, error) {
	var out []adapter.NativeSession
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") || path.Ext(name) != ".jsonl" {
			return nil
		}
		var h struct {
			Type    string
			Payload struct{ ID string }
		}
		if err = sessionfile.Header(root, name, &h); err != nil {
			return err
		}
		if h.Type != "session_meta" || !sessionfile.UUID(h.Payload.ID) {
			return fmt.Errorf("unknown Codex rollout format %q", name)
		}
		out = append(out, adapter.NativeSession{ID: h.Payload.ID, Format: "codex-rollout-jsonl.v1", Entries: []snapshot.Entry{{Source: name, Name: name}}})
		return nil
	})
	return out, err
}
