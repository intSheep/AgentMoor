package workspace

import (
	"context"
	"io/fs"
	"os"
	"strings"

	"agentmoor/internal/adapter"
)

// Layout describes one runtime's supported project-local conventions.
type Layout struct {
	ID, Version               string
	TestedRuntime             string
	Markers                   []string // A trailing slash denotes a directory.
	SkillParents              []string // Project directories containing a skills/ directory.
	MaxDepth                  int      // Direct skill directories are at depth one.
	RootFilePattern           string   // Optional filename pattern at the top of skills/.
	SkipHiddenAndDependencies bool
	ReadSessions              func(context.Context, *os.Root) ([]adapter.NativeSession, error)
}

func (l Layout) Sessions(ctx context.Context, root *os.Root) ([]adapter.NativeSession, error) {
	if l.ReadSessions == nil {
		return nil, fs.ErrInvalid
	}
	return l.ReadSessions(ctx, root)
}

func (l Layout) Descriptor() adapter.Descriptor {
	d := adapter.Descriptor{
		ID:            l.ID,
		Version:       l.Version,
		TestedRuntime: l.TestedRuntime,
		Capabilities: map[adapter.AssetKind][]adapter.Capability{
			adapter.Session: {},
			adapter.Skill: {
				{Operation: "discover", Mode: "filesystem", Reliability: "best-effort"},
				{Operation: "export", Mode: "snapshot", Reliability: "verified-checksum"},
				{Operation: "restore", Mode: "new-path", Reliability: "verified-checksum"},
			},
			adapter.Artifact: {
				{Operation: "discover", Mode: "declared-directory", Reliability: "persisted-only"},
				{Operation: "publish", Mode: "immutable-object", Reliability: "verified-checksum"},
				{Operation: "restore", Mode: "new-file", Reliability: "verified-checksum"},
			},
			adapter.Workspace: {
				{Operation: "snapshot", Mode: "filesystem", Reliability: "verified-checksum"},
				{Operation: "restore", Mode: "new-directory", Reliability: "verified-checksum"},
			},
		},
	}
	if l.ReadSessions != nil {
		d.Capabilities[adapter.Session] = []adapter.Capability{{Operation: "export", Mode: "native-format", Reliability: "persisted-only"}, {Operation: "restore", Mode: "native-format", Reliability: "tested-main-session"}}
	}
	for kind, operations := range d.Capabilities {
		if len(operations) > 0 {
			d.Capabilities[kind] = append(operations, adapter.Capability{Operation: "sync", Mode: "checksum", Reliability: "persisted-only"})
		}
	}
	return d
}

func (l Layout) Detect(ctx context.Context, root *os.Root) ([]string, error) {
	var evidence []string
	for _, marker := range l.Markers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name, isDir := strings.CutSuffix(marker, "/")
		var kind fs.FileMode
		if isDir {
			kind = fs.ModeDir
		}
		matched, err := has(root, name, kind)
		if err != nil {
			return nil, err
		}
		if matched {
			evidence = append(evidence, marker)
		}
	}
	return evidence, nil
}
