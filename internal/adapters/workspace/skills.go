// Package workspace implements read-only, filesystem-based adapter operations.
package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/intSheep/AgentMoor/internal/adapter"
)

// has tests the entry itself, so a symlink cannot serve as a marker.
func has(root *os.Root, name string, kind fs.FileMode) (bool, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().Type() == kind, nil
}

func (l Layout) DiscoverSkills(ctx context.Context, root *os.Root) ([]adapter.SkillRef, error) {
	found := []adapter.SkillRef{}
	for _, parent := range l.SkillParents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ok, err := has(root, parent, fs.ModeDir)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		dir := path.Join(parent, "skills")
		ok, err = has(root, dir, fs.ModeDir)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if err := l.scan(ctx, root, dir, 0, &found); err != nil {
			return nil, err
		}
	}
	return found, nil
}

func (l Layout) scan(ctx context.Context, root *os.Root, dir string, depth int, found *[]adapter.SkillRef) error {
	entries, err := fs.ReadDir(root.FS(), dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if l.SkipHiddenAndDependencies && (strings.HasPrefix(name, ".") || name == "node_modules") {
			continue
		}
		child := path.Join(dir, name)
		if !entry.IsDir() {
			if depth == 0 && entry.Type().IsRegular() && l.RootFilePattern != "" {
				matched, err := path.Match(l.RootFilePattern, name)
				if err != nil {
					return err
				}
				if matched {
					*found = append(*found, adapter.SkillRef{Manifest: child})
				}
			}
			continue
		}
		manifest := path.Join(child, "SKILL.md")
		matched, err := has(root, manifest, 0)
		if err != nil {
			return err
		}
		if matched {
			*found = append(*found, adapter.SkillRef{Manifest: manifest})
		} else if depth+1 < l.MaxDepth {
			if err := l.scan(ctx, root, child, depth+1, found); err != nil {
				return err
			}
		}
	}
	return nil
}
