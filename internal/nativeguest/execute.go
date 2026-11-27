package nativeguest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/snapshot"
	"github.com/intSheep/AgentMoor/internal/task"
)

// Execute is a native driver, not an isolation mechanism. Its caller must run
// it inside the assigned sandbox with no platform credentials or shared state.
// The complete request is verified before any native command. The complete
// result is captured and sealed on disk before any result bytes are emitted.
func Execute(ctx context.Context, a adapter.Adapter, executable, root string, src io.Reader, dst io.Writer, limits Limits) error {
	limits, err := limits.normalized()
	if err != nil {
		return err
	}
	if err = credentiallessEnvironment(); err != nil {
		return err
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(executable) {
		return fmt.Errorf("guest root and executable must be absolute paths")
	}
	root = filepath.Clean(root)
	parent, err := filepath.EvalSymlinks(filepath.Dir(root))
	if err != nil {
		return fmt.Errorf("guest root requires an existing trusted parent")
	}
	root = filepath.Join(parent, filepath.Base(root))
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return fmt.Errorf("guest runtime is unavailable")
	}
	if insideDirectory(root, executable) {
		return fmt.Errorf("guest runtime must be outside the writable guest root")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Mkdir(root, 0700); err != nil {
		return fmt.Errorf("guest execution requires a new root: %w", err)
	}
	defer os.RemoveAll(root)
	request, files, err := DecodeRequest(ctx, src, filepath.Join(root, "incoming"), limits)
	if err != nil {
		return err
	}
	descriptor := a.Descriptor()
	if descriptor.ID != request.AdapterID || descriptor.Version != request.AdapterVersion || descriptor.TestedRuntime != request.RuntimeVersion {
		return fmt.Errorf("guest request differs from the assigned adapter or runtime")
	}
	work, capture := filepath.Join(root, "workspace"), filepath.Join(root, "capture")
	if err = os.Mkdir(capture, 0700); err != nil {
		return err
	}
	if request.Workspace == nil {
		if err = os.Mkdir(work, 0700); err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(work, "input.txt"), []byte(request.Spec.Input), 0600); err != nil {
			return err
		}
		if err = os.Mkdir(filepath.Join(work, "out"), 0700); err != nil {
			return err
		}
	} else if err = restoreBlob(ctx, files.Workspace, *request.Workspace, work, true); err != nil {
		return err
	}
	home, err := os.MkdirTemp(root, "home-")
	if err != nil {
		return err
	}
	workspace, err := os.OpenRoot(work)
	if err != nil {
		return err
	}
	defer workspace.Close()
	environment := task.Environment{Executable: executable, Workspace: work, Home: home}
	environment.Skills, err = installSkills(ctx, a, environment, request.Skills, files.Skills)
	if err != nil {
		return err
	}
	info, err := adapter.Inspect(ctx, a, workspace, executable)
	if err != nil {
		return err
	}
	if !info.Tested || info.RuntimeVersion != request.RuntimeVersion {
		return fmt.Errorf("guest runtime does not match its pinned version")
	}
	var history task.RestoreHistory
	if request.Session != nil {
		history = guestHistory{ctx: ctx, a: a, executable: executable, workspace: work, home: home,
			file: files.Session, meta: *request.Session, temporary: capture}
	}
	var execution task.Execution
	if request.Spec.Mode == task.FileTurn {
		fileExecutor, ok := a.(task.FileExecutor)
		if !ok {
			return fmt.Errorf("assigned guest adapter does not support file turns")
		}
		staged := ".result-" + uuid.NewString()
		if _, err = workspace.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("guest file execution requires a fresh result path")
		}
		execution, err = fileExecutor.ExecuteFile(ctx, environment, request.Spec, staged, request.ParentSessionID, history)
		if err == nil {
			var stat os.FileInfo
			stat, err = workspace.Lstat(staged)
			if err == nil && !stat.Mode().IsRegular() {
				err = fmt.Errorf("guest file tool did not produce a new regular result")
			}
			if err == nil {
				err = workspace.Rename(staged, filepath.Join("out", request.Spec.Output))
			}
		}
	} else if request.ParentSessionID != "" {
		continuation, ok := a.(task.Continuation)
		if !ok {
			return fmt.Errorf("assigned guest adapter does not support continuation")
		}
		execution, err = continuation.ContinueTask(ctx, environment, request.ParentSessionID, request.Spec.Instruction, history)
	} else {
		executor, ok := a.(task.Executor)
		if !ok {
			return fmt.Errorf("assigned guest adapter does not support text turns")
		}
		execution, err = executor.ExecuteTask(ctx, environment, request.Spec)
	}
	if err != nil {
		return err
	}
	if !sessionID(execution.SessionID) || len(execution.Text) > MaxTextBytes || (request.ParentSessionID != "" && execution.SessionID != request.ParentSessionID) {
		return fmt.Errorf("guest execution returned an invalid session or result")
	}
	if request.Spec.Mode != task.FileTurn {
		if err = workspace.WriteFile(filepath.Join("out", request.Spec.Output), []byte(execution.Text), 0600); err != nil {
			return err
		}
	}
	// History paths are interpreted only inside this guest. They are never
	// returned as host paths in a result or passed to a platform client.
	if !filepath.IsAbs(execution.HistoryDir) {
		return fmt.Errorf("guest history must belong to its private home")
	}
	realHistory, err := filepath.EvalSymlinks(execution.HistoryDir)
	if err != nil || !insideDirectory(home, realHistory) || realHistory == home {
		return fmt.Errorf("guest history must belong to its private home")
	}
	workBlob, err := snapshot.TreeWithLimit(ctx, workspace, []snapshot.Entry{{Source: ".", Name: "."}}, capture, limits.MaxOutputBytes)
	if err != nil {
		return err
	}
	defer workBlob.Remove()
	remaining := limits.MaxOutputBytes - workBlob.Size
	if remaining < 1 {
		return fmt.Errorf("guest captures exceed their combined output budget")
	}
	session, sessionMetadata, err := captureSession(ctx, a, realHistory, execution.SessionID, capture, remaining)
	if err != nil {
		return err
	}
	defer session.Remove()
	result := Result{Version: Protocol, AdapterID: descriptor.ID, AdapterVersion: descriptor.Version, RuntimeVersion: info.RuntimeVersion,
		SessionID: execution.SessionID, Text: execution.Text,
		Workspace: blobMetadata(workBlob, "workspace-tar.v1", "application/x-tar"), Session: sessionMetadata}
	if err = result.Match(request); err != nil {
		return err
	}
	sealed, err := os.CreateTemp(capture, "result-*.tar")
	if err != nil {
		return err
	}
	defer sealed.Close()
	paths := map[string]string{"workspace.tar": workBlob.Path, "session.blob": session.Path}
	open := func(name string) (io.ReadCloser, error) {
		file, ok := paths[name]
		if !ok {
			return nil, fmt.Errorf("unknown guest result slot")
		}
		return os.Open(file)
	}
	if err = WriteResult(ctx, sealed, result, open, limits); err != nil {
		return err
	}
	if err = sealed.Sync(); err != nil {
		return err
	}
	if _, err = sealed.Seek(0, 0); err != nil {
		return err
	}
	_, err = io.Copy(dst, runContextReader{ctx, sealed})
	return err
}

func credentiallessEnvironment() error {
	for _, name := range []string{"AGENTMOOR_TOKEN", "AGENTMOOR_TOKEN_FILE", "AGENTMOOR_POSTGRES_DSN", "AGENTMOOR_S3_ENDPOINT",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		if value := os.Getenv(name); value != "" {
			return fmt.Errorf("guest process contains forbidden platform credentials or configuration")
		}
	}
	return nil
}

func insideDirectory(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func metadata(b BlobMeta) asset.Metadata {
	return asset.Metadata{Format: b.Format, MediaType: b.MediaType, SHA256: b.SHA256, Size: b.Size}
}

func blobMetadata(blob snapshot.Blob, format, media string) BlobMeta {
	return BlobMeta{Format: format, MediaType: media, SHA256: blob.SHA256, Size: blob.Size}
}

func restoreBlob(ctx context.Context, file string, blob BlobMeta, destination string, tree bool) error {
	if tree {
		if err := ValidateTree(ctx, file, MaxBundleBytes); err != nil {
			return err
		}
	}
	source, err := os.Open(file)
	if err != nil {
		return err
	}
	return errors.Join(snapshot.Restore(ctx, source, metadata(blob), destination, tree), source.Close())
}

func captureSession(ctx context.Context, a adapter.Adapter, dir, id, temporary string, maxBytes int64) (snapshot.Blob, BlobMeta, error) {
	source, ok := a.(adapter.SessionSource)
	if !ok {
		return snapshot.Blob{}, BlobMeta{}, fmt.Errorf("assigned guest adapter does not export native sessions")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return snapshot.Blob{}, BlobMeta{}, err
	}
	defer root.Close()
	sessions, err := source.Sessions(ctx, root)
	if err != nil {
		return snapshot.Blob{}, BlobMeta{}, err
	}
	var selected *adapter.NativeSession
	for _, native := range sessions {
		if native.ID == id {
			if selected != nil {
				return snapshot.Blob{}, BlobMeta{}, fmt.Errorf("guest session identity is ambiguous")
			}
			copy := native
			selected = &copy
		}
	}
	if selected == nil || len(selected.Entries) == 0 {
		return snapshot.Blob{}, BlobMeta{}, fmt.Errorf("guest execution did not persist its native session")
	}
	media := "application/x-tar"
	var blob snapshot.Blob
	if selected.Raw {
		if len(selected.Entries) != 1 {
			return snapshot.Blob{}, BlobMeta{}, fmt.Errorf("raw guest session must have one native source")
		}
		file := selected.Entries[0].Source
		media = mime.TypeByExtension(filepath.Ext(file))
		if media == "" {
			media = "application/octet-stream"
		}
		blob, err = snapshot.FileWithLimit(ctx, root, file, temporary, maxBytes)
	} else {
		blob, err = snapshot.TreeWithLimit(ctx, root, selected.Entries, temporary, maxBytes)
	}
	if err != nil {
		return snapshot.Blob{}, BlobMeta{}, err
	}
	return blob, blobMetadata(blob, selected.Format, media), nil
}

type runContextReader struct {
	ctx context.Context
	src io.Reader
}

func (r runContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.src.Read(p)
}
