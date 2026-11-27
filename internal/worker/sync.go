package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/platform"
	"github.com/intSheep/AgentMoor/internal/snapshot"
	"github.com/intSheep/AgentMoor/internal/task"
)

type Engine struct {
	Config  Config
	Adapter adapter.Adapter
	Client  *platform.Client
}

func (e *Engine) authorize(ctx context.Context) (platform.Grant, error) {
	g, err := e.Client.Context(ctx)
	if err != nil {
		return g, err
	}
	if g.Scope != e.Config.Scope {
		return g, fmt.Errorf("credential scope differs from assigned runtime")
	}
	return g, nil
}

type scan struct {
	turnID         string      // follow-up checkpoints have independent keys within the allocation
	artifacts      *[]task.Ref // optional receipt of the complete delivery directory
	engine         *Engine
	journal        *Journal
	heads          map[asset.Key]asset.Revision
	seen           map[asset.Key]bool
	runtimeVersion string
}

func (e *Engine) Sync(ctx context.Context) error { return e.sync(ctx, "", nil) }

func (e *Engine) sync(ctx context.Context, turn string, artifacts *[]task.Ref) error {
	return e.syncWithCapture(ctx, turn, artifacts, func(s *scan, root *os.Root) error { return s.captureLocal(ctx, root) })
}

// syncWithCapture keeps authorization, journal ownership and frozen retries in
// one place. The capture function determines the trusted source of each blob.
func (e *Engine) syncWithCapture(ctx context.Context, turn string, artifacts *[]task.Ref, capture func(*scan, *os.Root) error) (err error) {
	c := e.Config
	if err = c.Validate(); err != nil {
		return err
	}
	lock, err := acquire(c.StateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	j, err := load(c)
	if err != nil {
		return err
	}
	if j.State == "conflict" && j.Pending != nil {
		return fmt.Errorf("%w for %s %q; review the platform revision before resolving", platform.ErrConflict, j.Pending.Change.Kind, j.Pending.Change.ID)
	}
	j.State = "running"
	j.Error = ""
	if err = j.save(c.StateDir); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			j.State = "failed"
			if errors.Is(err, platform.ErrConflict) {
				j.State = "conflict"
			}
			j.Error = err.Error()
		} else {
			j.State = "complete"
			j.LastSuccess = time.Now().UTC()
		}
		err = errors.Join(err, j.save(c.StateDir))
	}()
	g, err := e.authorize(ctx)
	if err != nil {
		return err
	}
	if !g.Read || !g.Write {
		return fmt.Errorf("sync requires read and write permission")
	}
	d := e.Adapter.Descriptor()
	if d.ID != c.Scope.Adapter || (c.AdapterVersion != "" && d.Version != c.AdapterVersion) {
		return fmt.Errorf("adapter ID or version mismatch")
	}
	s := scan{engine: e, turnID: turn, artifacts: artifacts, journal: &j, heads: map[asset.Key]asset.Revision{}, seen: map[asset.Key]bool{}}
	// Blobs are disposable until referenced by a durable pending operation.
	leftovers, err := os.ReadDir(c.StateDir)
	if err != nil {
		return err
	}
	for _, entry := range leftovers {
		name := entry.Name()
		path := filepath.Join(c.StateDir, name)
		switch {
		case entry.IsDir() && strings.HasPrefix(name, "native-"):
			err = os.RemoveAll(path)
		case !entry.IsDir() && strings.HasPrefix(name, "asset-") && (j.Pending == nil || name != j.Pending.Blob):
			err = os.Remove(path)
		}
		if err != nil {
			return err
		}
	}
	if j.Pending != nil {
		if err = s.flush(ctx); err != nil {
			return err
		}
	}
	var revisions []asset.Revision
	if err = e.Client.JSON(ctx, "GET", "/v1/heads", nil, &revisions); err != nil {
		return err
	}
	for _, r := range revisions {
		if r.Version > s.heads[r.Key].Version {
			s.heads[r.Key] = r
		}
	}
	root, err := os.OpenRoot(c.Workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	return capture(&s, root)
}

func (s *scan) captureLocal(ctx context.Context, root *os.Root) error {
	e := s.engine
	c := e.Config
	if c.RuntimeCLI != "" {
		info, err := adapter.Inspect(ctx, e.Adapter, root, c.RuntimeCLI)
		if err != nil {
			return err
		}
		s.runtimeVersion = info.RuntimeVersion
		if c.RuntimeVersion != "" && info.RuntimeVersion != c.RuntimeVersion {
			return fmt.Errorf("expected runtime %q, found %q", c.RuntimeVersion, info.RuntimeVersion)
		}
		if c.NativeSessions && !info.Tested {
			return fmt.Errorf("native session commands are unverified for %q", info.RuntimeVersion)
		}
	}
	if err := s.workspace(ctx, root); err != nil {
		return err
	}
	if c.NativeSessions {
		native, ok := e.Adapter.(adapter.NativeSessions)
		if !ok {
			return fmt.Errorf("adapter has no native session CLI integration")
		}
		dir, err := os.MkdirTemp(c.StateDir, "native-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		if err := native.ExportNative(ctx, c.RuntimeCLI, c.Workspace, dir); err != nil {
			return err
		}
		return s.sessions(ctx, dir)
	}
	if c.Sessions != "" {
		return s.sessions(ctx, c.Sessions)
	}
	return nil
}

func (s *scan) flush(ctx context.Context) error {
	p := s.journal.Pending
	dir := s.engine.Config.StateDir
	file := filepath.Join(dir, p.Blob)
	var v int64
	var err error
	for attempt := 0; ; attempt++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		v, err = s.publishFrozen(ctx, p, file)
		// Task execution must finish the frozen operation without rerunning the
		// Agent. Ordinary Sync keeps its one-attempt, journal-on-next-scan behavior.
		if err == nil || s.artifacts == nil || attempt >= 2 || !transientPublication(ctx, err) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
	}
	if err != nil {
		return err
	}
	s.journal.Cursors[asset.Fingerprint(p.Change.Key)] = cursor{v, asset.Fingerprint(p.Change.Metadata)}
	s.journal.Pending = nil
	if err = s.journal.save(dir); err != nil {
		s.journal.Pending = p
		return err
	}
	return os.Remove(file)
}

func transientPublication(ctx context.Context, err error) bool {
	// net/http's response-header timeout also matches DeadlineExceeded. Only
	// the owning task context distinguishes that transport timeout from the
	// caller's actual deadline; a live task may retry its shorter HTTP timeout.
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || err == context.DeadlineExceeded {
		return false
	}
	var status *platform.HTTPError
	if errors.As(err, &status) {
		switch status.StatusCode {
		case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	// A dropped acknowledgement may fail before headers or during JSON framing.
	// Syntax/type errors, local filesystem errors, and TLS validation errors are
	// not transport retries.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return true
	}
	for _, interrupted := range []error{syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ECONNABORTED,
		syscall.EPIPE, syscall.ENETDOWN, syscall.ENETRESET, syscall.ENETUNREACH, syscall.EHOSTDOWN, syscall.EHOSTUNREACH} {
		if errors.Is(err, interrupted) {
			return true
		}
	}
	return false
}

func (s *scan) publish(ctx context.Context, m asset.Metadata, b snapshot.Blob) error {
	m.Key = task.TurnKey(s.turnID, m.Key)
	if s.seen[m.Key] {
		b.Remove()
		return fmt.Errorf("duplicate native identity %v", m.Key)
	}
	s.seen[m.Key] = true
	m.SHA256 = b.SHA256
	m.Size = b.Size
	m.AdapterVersion = s.engine.Adapter.Descriptor().Version
	m.RuntimeVersion = s.runtimeVersion
	if m.Origin == "" {
		m.Origin = "runtime"
	}
	m.Completeness = "persisted-files"
	fp := asset.Fingerprint(m)
	previous := s.journal.Cursors[asset.Fingerprint(m.Key)]
	head := s.heads[m.Key]
	if head.Version > 0 && asset.Fingerprint(head.Metadata) == fp {
		b.Remove()
		if previous.Version == head.Version && previous.Fingerprint == fp {
			return nil
		}
		s.journal.Cursors[asset.Fingerprint(m.Key)] = cursor{head.Version, fp}
		return s.journal.save(s.engine.Config.StateDir)
	}
	p := &pending{Change: asset.Change{Metadata: m, Base: previous.Version}, Blob: filepath.Base(b.Path)}
	if m.Kind == "workspace" && m.Format == "workspace-tar.v1" {
		p.Transfer = s.engine.Config.WorkspaceTransfer
	}
	s.journal.Pending = p
	if err := s.journal.save(s.engine.Config.StateDir); err != nil {
		return err
	}
	if head.Version != previous.Version {
		return fmt.Errorf("%w for %s %q: local base %d, platform head %d", platform.ErrConflict, m.Kind, m.ID, previous.Version, head.Version)
	}
	return s.flush(ctx)
}

func (s *scan) tree(ctx context.Context, root *os.Root, key asset.Key, format string, entries []snapshot.Entry) error {
	b, err := snapshot.Tree(ctx, root, entries, s.engine.Config.StateDir)
	if err != nil {
		return err
	}
	return s.publish(ctx, asset.Metadata{Key: key, Format: format, MediaType: "application/x-tar", Name: path.Base(key.ID)}, b)
}

func (s *scan) file(ctx context.Context, root *os.Root, key asset.Key, format, src, session string) error {
	if s.artifacts != nil && key.Kind == "artifact" && len(*s.artifacts) >= task.MaxArtifacts {
		return fmt.Errorf("task delivery exceeds %d artifacts", task.MaxArtifacts)
	}
	b, err := snapshot.File(ctx, root, src, s.engine.Config.StateDir)
	if err != nil {
		return err
	}
	media := mime.TypeByExtension(path.Ext(src))
	if media == "" {
		media = "application/octet-stream"
	}
	if err = s.publish(ctx, asset.Metadata{Key: key, Format: format, MediaType: media, Name: path.Base(src), SessionID: session}, b); err != nil {
		return err
	}
	if s.artifacts != nil && key.Kind == "artifact" {
		key = task.TurnKey(s.turnID, key)
		version := s.journal.Cursors[asset.Fingerprint(key)].Version
		*s.artifacts = append(*s.artifacts, task.Ref{Key: key, Version: version, SHA256: b.SHA256, ContentPath: key.ContentPath(version)})
	}
	return nil
}

func (s *scan) workspace(ctx context.Context, root *os.Root) error {
	if err := s.tree(ctx, root, asset.Key{Kind: "workspace", ID: "root"}, "workspace-tar.v1", []snapshot.Entry{{Source: ".", Name: "."}}); err != nil {
		return err
	}
	return s.workspaceContents(ctx, root)
}

// workspaceContents only discovers project-local data. Native session import,
// export, execution and runtime inspection are separate capture operations.
func (s *scan) workspaceContents(ctx context.Context, root *os.Root) error {
	c := s.engine.Config
	if finder, ok := s.engine.Adapter.(adapter.SkillDiscoverer); ok {
		skills, err := finder.DiscoverSkills(ctx, root)
		if err != nil {
			return err
		}
		for _, skill := range skills {
			if err = s.skill(ctx, root, skill.Manifest); err != nil {
				return err
			}
		}
	}
	if c.Artifacts == "" {
		return nil
	}
	return fs.WalkDir(root.FS(), c.Artifacts, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("unsupported artifact %q", name)
		}
		return s.file(ctx, root, asset.Key{Kind: "artifact", ID: name}, "file.v1", name, c.ArtifactSession)
	})
}

func (s *scan) sessions(ctx context.Context, dir string) error {
	source, ok := s.engine.Adapter.(adapter.SessionSource)
	if !ok {
		return fmt.Errorf("adapter does not support session export")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	items, err := source.Sessions(ctx, root)
	if err != nil {
		return err
	}
	for _, x := range items {
		k := asset.Key{Kind: "session", ID: x.ID}
		if len(x.Entries) == 0 {
			return fmt.Errorf("empty session %q", x.ID)
		}
		if x.Raw {
			err = s.file(ctx, root, k, x.Format, x.Entries[0].Source, "")
		} else {
			err = s.tree(ctx, root, k, x.Format, x.Entries)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
