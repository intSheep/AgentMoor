package nativeguest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/snapshot"
)

type guestHistory struct {
	ctx                                    context.Context
	a                                      adapter.Adapter
	executable, workspace, home, temporary string
	file                                   string
	meta                                   BlobMeta
}

func (h guestHistory) parent(destination string) error {
	if !filepath.IsAbs(destination) || destination == h.home {
		return fmt.Errorf("native session restore must stay in the private guest home")
	}
	rel, err := filepath.Rel(h.home, destination)
	if err != nil || !asset.SafePath(filepath.ToSlash(rel)) {
		return fmt.Errorf("native session restore must stay in the private guest home")
	}
	root, err := os.OpenRoot(h.home)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.MkdirAll(filepath.Dir(rel), 0700); err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil || !insideDirectory(h.home, parent) {
		return fmt.Errorf("native session parent escapes the private guest home")
	}
	return nil
}

func (h guestHistory) Files(destination string) error {
	if h.meta.MediaType != "application/x-tar" {
		return fmt.Errorf("guest continuation requires a native session tree")
	}
	if err := h.parent(destination); err != nil {
		return err
	}
	return restoreBlob(h.ctx, h.file, h.meta, destination, true)
}

func (h guestHistory) NativeDatabase(destination string) error {
	if h.meta.MediaType == "application/x-tar" {
		return fmt.Errorf("guest continuation requires an original native export")
	}
	native, ok := h.a.(adapter.NativeSessions)
	if !ok {
		return fmt.Errorf("assigned guest adapter has no native importer")
	}
	if err := h.parent(destination); err != nil {
		return err
	}
	input, err := os.MkdirTemp(h.temporary, "native-import-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(input)
	file := filepath.Join(input, "session.json")
	if err = restoreBlob(h.ctx, h.file, h.meta, file, false); err != nil {
		return err
	}
	return snapshot.BuildDirectory(destination, func(stage string) error {
		return native.ImportNative(h.ctx, h.executable, h.workspace, file, stage)
	})
}
