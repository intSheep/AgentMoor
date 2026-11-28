// Package notebook demonstrates a filesystem-only Adapter for a synthetic
// notebook format. It does not execute a CLI or implement task modes.
package notebook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/intSheep/AgentMoor/sdk"
)

const (
	marker        = ".notebook/marker"
	skillManifest = ".notebook/skills/greet/SKILL.md"
	sessionFormat = "notebook-json.v1"
	maxEntries    = 32
	maxSession    = 64 << 10
)

type Adapter struct{}

var (
	_ sdk.Adapter         = Adapter{}
	_ sdk.SkillDiscoverer = Adapter{}
	_ sdk.SessionSource   = Adapter{}
)

func (Adapter) Descriptor() sdk.Descriptor {
	return sdk.Descriptor{ID: "notebook-example", Version: "0.1.0",
		Capabilities: map[sdk.AssetKind][]sdk.Capability{
			sdk.Session:   {{Operation: "export", Mode: "filesystem", Reliability: "synthetic-example-only"}, {Operation: "restore", Mode: "filesystem", Reliability: "synthetic-example-only"}},
			sdk.Skill:     {{Operation: "discover", Mode: "filesystem", Reliability: "synthetic-example-only"}},
			sdk.Workspace: {{Operation: "snapshot", Mode: "filesystem", Reliability: "synthetic-example-only"}},
			sdk.Artifact:  {{Operation: "publish", Mode: "immutable-object", Reliability: "synthetic-example-only"}},
		}}
}

// regularFile checks only paths inside the root assigned by the trusted caller.
func regularFile(ctx context.Context, root *os.Root, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s must be a regular file", name)
	}
	return true, nil
}

func (Adapter) Detect(ctx context.Context, root *os.Root) ([]string, error) {
	found, err := regularFile(ctx, root, marker)
	if err != nil || !found {
		return nil, err
	}
	return []string{marker}, nil
}

func (Adapter) DiscoverSkills(ctx context.Context, root *os.Root) ([]sdk.SkillRef, error) {
	found, err := regularFile(ctx, root, skillManifest)
	if err != nil || !found {
		return nil, err
	}
	return []sdk.SkillRef{{Manifest: skillManifest}}, nil
}

// Sessions reads an assigned Session directory, not a user's global home.
// The format's ID comes from the original JSON; filenames do not replace it.
func (Adapter) Sessions(ctx context.Context, root *os.Root) ([]sdk.NativeSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(maxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maxEntries {
		return nil, fmt.Errorf("Session directory exceeds %d entries", maxEntries)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var sessions []sdk.NativeSession
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("Session %s must be a regular file", entry.Name())
		}
		id, err := readSessionID(root, entry.Name())
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, sdk.NativeSession{ID: id, Format: sessionFormat, Raw: true,
			Entries: []sdk.Entry{{Source: entry.Name(), Name: entry.Name()}}})
	}
	return sessions, nil
}

func readSessionID(root *os.Root, name string) (string, error) {
	file, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("Session %s is not a readable regular file", name)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxSession+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxSession {
		return "", fmt.Errorf("Session %s exceeds %d bytes", name, maxSession)
	}
	var document struct {
		ID string `json:"session_id"`
	}
	if json.Unmarshal(raw, &document) != nil || document.ID == "" || len(document.ID) > 128 {
		return "", fmt.Errorf("Session %s has invalid JSON or identity", name)
	}
	for _, char := range document.ID {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return "", fmt.Errorf("Session %s has an unsupported identity", name)
		}
	}
	// Do not marshal the document back: unknown fields and original bytes belong
	// to the native format. The shared pipeline archives the original file.
	return document.ID, nil
}
