package nativeguest

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agentmoor/internal/adapter"
	"agentmoor/internal/asset"
	"agentmoor/internal/task"
	"go.yaml.in/yaml/v3"
)

func installSkills(ctx context.Context, a adapter.Adapter, environment task.Environment, skills []Skill, files []string) ([]task.LoadedSkill, error) {
	if len(skills) != len(files) {
		return nil, fmt.Errorf("guest skill slots differ from the request")
	}
	if len(skills) == 0 {
		return nil, nil
	}
	installer, ok := a.(task.SkillInstaller)
	if !ok {
		return nil, fmt.Errorf("assigned guest adapter does not install native skills")
	}
	home, err := filepath.EvalSymlinks(environment.Home)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var loaded []task.LoadedSkill
	for i, skill := range skills {
		destination, err := installer.SkillDirectory(environment, skill.Name)
		if err != nil || !filepath.IsAbs(destination) || destination == home {
			return nil, fmt.Errorf("native skill target must stay in the guest home")
		}
		rel, err := filepath.Rel(home, destination)
		if err != nil || !asset.SafePath(filepath.ToSlash(rel)) {
			return nil, fmt.Errorf("native skill target must stay in the guest home")
		}
		if err = root.MkdirAll(filepath.Dir(rel), 0700); err != nil {
			return nil, err
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
		if err != nil || !insideDirectory(home, parent) {
			return nil, fmt.Errorf("native skill parent escapes the guest home")
		}
		if err = restoreBlob(ctx, files[i], skill.Blob, destination, true); err != nil {
			return nil, err
		}
		manifest := filepath.Join(rel, "SKILL.md")
		info, err := root.Lstat(manifest)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("native skill requires a regular manifest")
		}
		file, err := root.Open(manifest)
		if err != nil {
			return nil, err
		}
		name := skillManifestName(file)
		file.Close()
		if name != skill.Name {
			return nil, fmt.Errorf("native skill manifest differs from its pinned name")
		}
		canonical, err := filepath.EvalSymlinks(filepath.Join(home, manifest))
		if err != nil || !insideDirectory(home, canonical) {
			return nil, fmt.Errorf("native skill manifest escapes the guest home")
		}
		loaded = append(loaded, task.LoadedSkill{Name: skill.Name, Manifest: canonical})
	}
	return loaded, nil
}

func skillManifestName(source io.Reader) string {
	scanner := bufio.NewScanner(io.LimitReader(source, 64<<10))
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return ""
	}
	var front bytes.Buffer
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "---" {
			var fields struct {
				Name string `yaml:"name"`
			}
			if yaml.Unmarshal(front.Bytes(), &fields) != nil {
				return ""
			}
			return strings.TrimSpace(fields.Name)
		}
		front.WriteString(scanner.Text())
		front.WriteByte('\n')
	}
	return ""
}
