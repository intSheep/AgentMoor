// Package worker captures assets in control-plane-assigned directories and
// publishes revisions through a scoped platform client.
package worker

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
)

type Config struct {
	Scope           asset.Scope     `json:"scope"`
	Endpoint        string          `json:"endpoint"`
	Workspace       string          `json:"workspace"`
	Sessions        string          `json:"sessions,omitempty"`
	Artifacts       string          `json:"artifacts,omitempty"` // Relative to Workspace.
	ArtifactSession string          `json:"artifact_session,omitempty"`
	ManagedSkills   []string        `json:"managed_skills,omitempty"`
	SkillPins       []task.SkillPin `json:"skill_pins,omitempty"`
	StateDir        string          `json:"state_dir"`
	AdapterVersion  string          `json:"adapter_version,omitempty"`
	RuntimeCLI      string          `json:"runtime_cli,omitempty"`
	RuntimeVersion  string          `json:"runtime_version,omitempty"`
	NativeSessions  bool            `json:"native_sessions,omitempty"`
}

func LoadConfig(name string) (Config, error) {
	var c Config
	f, err := os.Open(name)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return c, fmt.Errorf("expected exactly one worker configuration")
	}
	return c, c.Validate()
}

func inside(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolve also resolves symlinks in the existing prefix of a new state path.
func resolve(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("expected absolute path: %q", p)
	}
	p = filepath.Clean(p)
	q, err := filepath.EvalSymlinks(p)
	if err == nil {
		return q, nil
	}
	if !os.IsNotExist(err) || p == filepath.Dir(p) {
		return "", err
	}
	q, err = resolve(filepath.Dir(p))
	return filepath.Join(q, filepath.Base(p)), err
}

func (c Config) Validate() error {
	if err := task.ValidSkillPins(c.SkillPins); err != nil {
		return err
	}
	if !c.Scope.Valid() || c.Endpoint == "" {
		return fmt.Errorf("missing endpoint or invalid runtime scope")
	}
	if c.Artifacts != "" && !asset.SafePath(c.Artifacts) {
		return fmt.Errorf("artifact path must be workspace-relative")
	}
	state, err := resolve(c.StateDir)
	if err != nil {
		return err
	}
	for _, p := range []string{c.Workspace, c.Sessions} {
		if p == "" {
			continue
		}
		dir, err := resolve(p)
		if err != nil {
			return err
		}
		if inside(state, dir) || inside(dir, state) {
			return fmt.Errorf("state_dir must be separate from asset roots")
		}
	}
	if c.Workspace == "" {
		return fmt.Errorf("workspace required")
	}
	if c.RuntimeCLI != "" {
		exe, err := resolve(c.RuntimeCLI)
		if err != nil {
			return err
		}
		root, err := resolve(c.Workspace)
		if err != nil {
			return err
		}
		if inside(root, exe) {
			return fmt.Errorf("runtime_cli must be outside the agent workspace")
		}
	}
	if c.NativeSessions && (c.RuntimeCLI == "" || c.Sessions != "") {
		return fmt.Errorf("native_sessions requires runtime_cli and a separate native home instead of a sessions directory")
	}
	if c.RuntimeVersion != "" && c.RuntimeCLI == "" {
		return fmt.Errorf("version check needs runtime_cli")
	}
	return nil
}
