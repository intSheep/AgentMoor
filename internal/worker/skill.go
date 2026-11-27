package worker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/snapshot"
)

func (s *scan) skill(ctx context.Context, root *os.Root, manifest string) error {
	key := asset.Key{Kind: "skill", ID: manifest}
	bundled := path.Base(manifest) == "SKILL.md"
	m := asset.Metadata{Key: key, Format: "skill-markdown.v1", MediaType: "text/markdown", Name: strings.TrimSuffix(path.Base(manifest), path.Ext(manifest))}
	var b snapshot.Blob
	var err error
	if bundled {
		m.Format = "skill-tar.v1"
		m.MediaType = "application/x-tar"
		m.Name = path.Base(path.Dir(manifest))
		b, err = snapshot.Tree(ctx, root, []snapshot.Entry{{Source: path.Dir(manifest), Name: "."}}, s.engine.Config.StateDir)
	} else {
		b, err = snapshot.File(ctx, root, manifest, s.engine.Config.StateDir)
	}
	if err != nil {
		return err
	}
	f, err := os.Open(b.Path)
	if err != nil {
		b.Remove()
		return err
	}
	var r io.Reader = f
	if bundled {
		tr := tar.NewReader(f)
		for {
			h, err := tr.Next()
			if err != nil {
				f.Close()
				b.Remove()
				return fmt.Errorf("captured skill has no manifest: %w", err)
			}
			if h.Name == "SKILL.md" && h.Typeflag == tar.TypeReg {
				break
			}
		}
		r = tr
	}
	details := readFrontmatter(r)
	f.Close()
	if details.Name != "" {
		m.Name = details.Name
	}
	m.DeclaredVersion = details.Version
	m.Dependencies = details.Dependencies
	m.Origin = "runtime-native"
	if slices.Contains(s.engine.Config.ManagedSkills, manifest) {
		m.Origin = "platform-managed"
	}
	return s.publish(ctx, m, b)
}

type frontmatter struct {
	Name         string   `yaml:"name"`
	Version      string   `yaml:"version"`
	Dependencies []string `yaml:"dependencies"`
	Metadata     struct {
		Version string `yaml:"version"`
	} `yaml:"metadata"`
}

// Projects may use runtime-specific frontmatter. Keep unrecognized fields in
// the original manifest, and only promote the small set we can interpret.
func readFrontmatter(r io.Reader) frontmatter {
	reader := bufio.NewScanner(io.LimitReader(r, 64<<10))
	if !reader.Scan() || strings.TrimSpace(reader.Text()) != "---" {
		return frontmatter{}
	}
	var b bytes.Buffer
	for reader.Scan() {
		if strings.TrimSpace(reader.Text()) == "---" {
			var v frontmatter
			if yaml.Unmarshal(b.Bytes(), &v) != nil {
				return frontmatter{}
			}
			if v.Version == "" {
				v.Version = v.Metadata.Version
			}
			return v
		}
		b.WriteString(reader.Text())
		b.WriteByte('\n')
	}
	return frontmatter{}
}
