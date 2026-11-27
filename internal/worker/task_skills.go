package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/intSheep/AgentMoor/internal/snapshot"
	"github.com/intSheep/AgentMoor/internal/task"
)

// Pinned packages are restored to a fresh, private runtime home on every turn.
// Native workspace Skills and the original source revisions remain untouched.
func (e *Engine) installTaskSkills(ctx context.Context, env task.Environment, pins []task.SkillPin) ([]task.LoadedSkill, error) {
	if err := task.ValidSkillPins(pins); err != nil {
		return nil, err
	}
	if len(pins) == 0 {
		return nil, nil
	}
	installer, ok := e.Adapter.(task.SkillInstaller)
	if !ok {
		return nil, fmt.Errorf("adapter cannot install pinned Skills")
	}
	home, err := filepath.EvalSymlinks(env.Home)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var loaded []task.LoadedSkill
	for _, pin := range pins {
		dest, err := installer.SkillDirectory(env, pin.Name)
		if err != nil {
			return nil, err
		}
		dest, err = resolve(dest)
		if err != nil || dest == home || !inside(home, dest) {
			return nil, fmt.Errorf("pinned Skill target must be inside the private runtime home")
		}
		rel, err := filepath.Rel(home, dest)
		if err != nil {
			return nil, err
		}
		if err = root.MkdirAll(filepath.Dir(rel), 0700); err != nil {
			return nil, err
		}
		rev, stream, err := e.Client.Content(ctx, pin.Source.Key, pin.Source.Version)
		if err != nil {
			return nil, err
		}
		if rev.Scope != e.Config.Scope || rev.SHA256 != pin.Source.SHA256 || rev.Format != "skill-tar.v1" || rev.MediaType != "application/x-tar" {
			stream.Close()
			return nil, fmt.Errorf("pinned Skill revision differs from the selected package")
		}
		err = snapshot.Restore(ctx, stream, rev.Metadata, dest, true)
		closeErr := stream.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		manifest := filepath.Join(rel, "SKILL.md")
		info, err := root.Lstat(manifest)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("pinned Skill must contain a regular SKILL.md")
		}
		f, err := root.Open(manifest)
		if err != nil {
			return nil, err
		}
		name := readFrontmatter(f).Name
		f.Close()
		if strings.TrimSpace(name) != pin.Name {
			return nil, fmt.Errorf("pinned Skill manifest name differs from selected name")
		}
		canonical, err := filepath.EvalSymlinks(filepath.Join(home, manifest))
		if err != nil {
			return nil, err
		}
		loaded = append(loaded, task.LoadedSkill{Name: pin.Name, Manifest: canonical})
	}
	return loaded, nil
}
