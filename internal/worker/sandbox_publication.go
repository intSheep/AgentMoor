package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/nativeguest"
	"agentmoor/internal/platform"
	"agentmoor/internal/snapshot"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

var sealedDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var sandboxNamespace = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func sameAllocation(a, b task.Record) bool {
	return a.ID == b.ID && a.TurnID == b.TurnID && a.Spec == b.Spec && a.Deadline.Equal(b.Deadline) &&
		a.ExecutionPolicy == task.ExecutionUntrustedIsolated && b.ExecutionPolicy == task.ExecutionUntrustedIsolated &&
		reflect.DeepEqual(a.Parent, b.Parent) && slices.Equal(a.Skills, b.Skills)
}

func sealedEnvelope(b platform.SandboxBlob) bool {
	return b.Sealed && b.Size > 0 && b.Size <= nativeguest.MaxBundleBytes && sealedDigest.MatchString(b.SHA256)
}

func publicationUUID(v string) bool {
	u, err := uuid.Parse(v)
	return err == nil && u != uuid.Nil && u.String() == v
}

// publishSandboxResult accepts only a complete, sealed result for the current
// isolated allocation. It restores data to a new workspace; it never invokes
// native inspection, execution, import/export or session-source methods.
// The provider is deliberately not wired into runTask by this component.
func (e *Engine) publishSandboxResult(ctx context.Context, record task.Record, status platform.SandboxTaskStatus, source io.Reader) (string, []task.Ref, error) {
	if e.Client == nil || e.Adapter == nil || source == nil || e.Config.ExecutionPolicy != ExecutionUntrustedIsolated {
		return "", nil, fmt.Errorf("isolated publication requires an assigned engine and sealed source")
	}
	c := e.Config
	// Native settings do not participate in this data-only path. Validate the
	// original state/workspace boundary before choosing its publication journal.
	c.RuntimeCLI, c.RuntimeVersion, c.Sessions = "", "", ""
	c.NativeSessions, c.Artifacts, c.ArtifactSession = false, "out", ""
	if err := c.Validate(); err != nil {
		return "", nil, err
	}
	d := e.Adapter.Descriptor()
	if d.ID != c.Scope.Adapter || d.Version == "" || d.TestedRuntime == "" ||
		(e.Config.AdapterVersion != "" && e.Config.AdapterVersion != d.Version) ||
		(e.Config.RuntimeVersion != "" && e.Config.RuntimeVersion != d.TestedRuntime) {
		return "", nil, fmt.Errorf("isolated publication adapter or runtime differs from assigned versions")
	}
	if record.ID != c.Scope.Runtime || record.State != task.Running || !record.Spec.Valid() ||
		record.ExecutionPolicy != task.ExecutionUntrustedIsolated || !time.Now().Before(record.Deadline) ||
		(record.TurnID != "" && !publicationUUID(record.TurnID)) || task.ValidSkillPins(record.Skills) != nil {
		return "", nil, fmt.Errorf("isolated publication requires the live isolated allocation")
	}
	if err := matchSandboxPublication(c, record, status, d.ID, d.Version, d.TestedRuntime); err != nil {
		return "", nil, err
	}
	g, err := e.authorize(ctx)
	if err != nil {
		return "", nil, err
	}
	if !g.Read || !g.Write || !g.Execute {
		return "", nil, fmt.Errorf("isolated publication requires read, write and execute permission")
	}
	route := "/v1/tasks/" + url.PathEscape(record.ID)
	if record.TurnID != "" {
		route += "/turns/" + record.TurnID
	}
	var current task.Record
	if err = e.Client.JSON(ctx, "GET", route, nil, &current); err != nil {
		return "", nil, err
	}
	if current.State != task.Running || !sameAllocation(record, current) {
		return "", nil, fmt.Errorf("isolated allocation changed or stopped before publication")
	}
	ctx, cancel := context.WithDeadline(ctx, record.Deadline)
	defer cancel()
	stopped := make(chan struct{})
	go func() { defer close(stopped); e.monitorTask(ctx, route, cancel) }()
	defer func() { cancel(); <-stopped }()
	if err = ctx.Err(); err != nil {
		return "", nil, err
	}
	if _, err = os.Lstat(c.Workspace); !errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("isolated result restoration requires a new workspace")
	}
	if err = os.MkdirAll(c.StateDir, 0700); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(c.StateDir, "sandbox-result-*")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(dir)
	sealed := &sealedResultReader{ctx: ctx, reader: io.LimitReader(source, status.Run.Output.Size+1), expected: status.Run.Output, digest: sha256.New()}
	capture := filepath.Join(dir, "capture")
	result, err := nativeguest.ReadResult(ctx, sealed, capture, status.Request, nativeguest.Limits{MaxOutputBytes: status.Run.Output.Size})
	if err != nil {
		return "", nil, err
	}
	if _, err = io.Copy(io.Discard, sealed); err != nil {
		return "", nil, err
	}
	if !sealed.complete {
		return "", nil, fmt.Errorf("isolated result did not finish its sealed envelope")
	}
	workspace, err := os.Open(filepath.Join(capture, "workspace.tar"))
	if err != nil {
		return "", nil, err
	}
	err = snapshot.Restore(ctx, workspace, asset.Metadata{SHA256: result.Workspace.SHA256, Size: result.Workspace.Size}, c.Workspace, true)
	err = errors.Join(err, workspace.Close())
	if err != nil {
		return "", nil, err
	}
	root, err := os.OpenRoot(c.Workspace)
	if err != nil {
		return "", nil, errors.Join(err, snapshot.RemoveDirectory(c.Workspace))
	}
	err = validateSandboxDelivery(ctx, root, record)
	err = errors.Join(err, root.Close())
	if err != nil {
		return "", nil, errors.Join(err, snapshot.RemoveDirectory(c.Workspace))
	}
	c.StateDir = filepath.Join(c.StateDir, "publish")
	c.ArtifactSession = result.SessionID
	publisher := Engine{Config: c, Adapter: e.Adapter, Client: e.Client}
	var artifacts []task.Ref
	err = publisher.syncWithCapture(ctx, record.TurnID, &artifacts, func(s *scan, root *os.Root) error {
		s.runtimeVersion = d.TestedRuntime
		captured, err := os.OpenRoot(capture)
		if err != nil {
			return err
		}
		defer captured.Close()
		workspace, err := sandboxFrozenBlob(ctx, captured, "workspace.tar", result.Workspace, c.StateDir)
		if err != nil {
			return err
		}
		session, err := sandboxFrozenBlob(ctx, captured, "session.blob", result.Session, c.StateDir)
		if err != nil {
			workspace.Remove()
			return err
		}
		// Each publish takes ownership of its frozen bytes. Failed pending
		// operations remain referenced by the journal across caller retries.
		defer func() {
			for _, b := range []snapshot.Blob{workspace, session} {
				if s.journal.Pending == nil || s.journal.Pending.Blob != filepath.Base(b.Path) {
					b.Remove()
				}
			}
		}()
		if err = s.publish(ctx, asset.Metadata{Key: asset.Key{Kind: "workspace", ID: "root"}, Format: result.Workspace.Format, MediaType: result.Workspace.MediaType, Name: "root"}, workspace); err != nil {
			return err
		}
		if err = s.workspaceContents(ctx, root); err != nil {
			return err
		}
		return s.publish(ctx, asset.Metadata{Key: asset.Key{Kind: "session", ID: result.SessionID}, Format: result.Session.Format, MediaType: result.Session.MediaType, Name: result.SessionID}, session)
	})
	if err != nil {
		return "", nil, err
	}
	return result.SessionID, artifacts, nil
}

func matchSandboxPublication(c Config, record task.Record, status platform.SandboxTaskStatus, id, version, runtime string) error {
	r, q := status.Run, status.Request
	if r.Scope != c.Scope || r.TurnID != record.TurnID || !sameAllocation(record, r.Source) || r.Source.State != task.Running ||
		!r.Deadline.Equal(record.Deadline) || r.State != platform.SandboxReady || r.Collected || r.ErrorCode != "" ||
		!r.CreateStarted || !r.PodTerminal || !r.PodDeleted || r.ExitCode != 0 || !publicationUUID(r.PodUID) ||
		!sealedDigest.MatchString(r.ID) || !sealedDigest.MatchString(r.ProfileFingerprint) || !sealedDigest.MatchString(r.RecordFingerprint) ||
		!sealedDigest.MatchString(r.PodTemplateSHA) || !sandboxNamespace.MatchString(r.PodNamespace) || r.PodName != "sandbox-"+r.ID[:32] ||
		!sealedEnvelope(r.Input) || !sealedEnvelope(r.Output) || q.Validate() != nil ||
		q.AdapterID != id || q.AdapterVersion != version || q.RuntimeVersion != runtime || q.Spec != record.Spec || len(q.Skills) != len(record.Skills) {
		return fmt.Errorf("sealed sandbox result differs from assigned allocation")
	}
	if record.Parent == nil {
		if q.ParentSessionID != "" || q.Workspace != nil || q.Session != nil {
			return fmt.Errorf("initial result received parent state")
		}
	} else if q.ParentSessionID != record.Parent.SessionID || q.Session == nil || q.Workspace == nil ||
		q.Session.SHA256 != record.Parent.Session.SHA256 || q.Workspace.SHA256 != record.Parent.Workspace.SHA256 {
		return fmt.Errorf("sandbox parent differs from frozen checkpoint")
	}
	for i, pin := range record.Skills {
		if q.Skills[i].Name != pin.Name || q.Skills[i].Blob.SHA256 != pin.Source.SHA256 {
			return fmt.Errorf("sandbox Skill differs from frozen pin")
		}
	}
	return nil
}

func validateSandboxDelivery(ctx context.Context, root *os.Root, record task.Record) error {
	info, err := root.Lstat("out")
	if err != nil || !info.IsDir() || info.Mode().Type() != fs.ModeDir {
		return fmt.Errorf("isolated delivery requires a real out directory")
	}
	count, primary := 0, false
	err = fs.WalkDir(root.FS(), "out", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() || !task.TurnKey(record.TurnID, asset.Key{Kind: "artifact", ID: name}).Valid() {
			return fmt.Errorf("isolated delivery contains an unsafe artifact")
		}
		count++
		if count > task.MaxArtifacts {
			return fmt.Errorf("isolated delivery exceeds artifact limit")
		}
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
		primary = primary || name == "out/"+record.Spec.Output
		return nil
	})
	if err != nil {
		return err
	}
	if !primary {
		return fmt.Errorf("isolated delivery is missing its primary result")
	}
	return nil
}

func sandboxFrozenBlob(ctx context.Context, root *os.Root, name string, expected nativeguest.BlobMeta, dir string) (snapshot.Blob, error) {
	b, err := snapshot.FileWithLimit(ctx, root, name, dir, expected.Size)
	if err != nil {
		return b, err
	}
	if b.Size != expected.Size || b.SHA256 != expected.SHA256 {
		b.Remove()
		return snapshot.Blob{}, fmt.Errorf("validated sandbox capture changed")
	}
	return b, nil
}

type sealedResultReader struct {
	ctx      context.Context
	reader   io.Reader
	expected platform.SandboxBlob
	digest   hash.Hash
	size     int64
	complete bool
	failure  error
}

func (r *sealedResultReader) Read(p []byte) (int, error) {
	if r.failure != nil {
		return 0, r.failure
	}
	if err := r.ctx.Err(); err != nil {
		r.failure = err
		return 0, err
	}
	n, err := r.reader.Read(p)
	r.size += int64(n)
	r.digest.Write(p[:n])
	if r.size > r.expected.Size || (err == io.EOF && (r.size != r.expected.Size || hex.EncodeToString(r.digest.Sum(nil)) != r.expected.SHA256)) {
		r.failure = fmt.Errorf("sandbox output differs from its independently sealed envelope")
		return n, r.failure
	}
	if err == io.EOF {
		r.complete = true
	}
	if err != nil && err != io.EOF {
		r.failure = err
	}
	return n, err
}
