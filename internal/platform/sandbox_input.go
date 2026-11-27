package platform

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"

	"agentmoor/internal/asset"
	"agentmoor/internal/nativeguest"
	"agentmoor/internal/task"
)

// SandboxExecutionProfile is selected by trusted deployment code, never by a
// task, model or Worker request. Native paths and credentials are not wire data.
type SandboxExecutionProfile struct {
	Fingerprint    string
	AdapterID      string
	AdapterVersion string
	RuntimeVersion string
}

// PrepareSandboxInput uses only the real claim and its immutable asset refs.
// No native CLI or asset extraction runs on the trusted platform. The same
// address/profile/input identity may be retried, but cannot allocate a new run.
func (c *Catalog) PrepareSandboxInput(ctx context.Context, address SandboxAddress, profile SandboxExecutionProfile) (SandboxRun, nativeguest.Request, error) {
	if !sandboxDigest(profile.Fingerprint) || profile.AdapterID != address.Scope.Adapter {
		return SandboxRun{}, nativeguest.Request{}, errBadRequest
	}
	slot, err := c.sandboxTask(ctx, address, false)
	if err != nil {
		return SandboxRun{}, nativeguest.Request{}, err
	}
	if slot.ExecutionPolicy != task.ExecutionUntrustedIsolated {
		return SandboxRun{}, nativeguest.Request{}, ErrConflict
	}
	ctx, cancel := context.WithDeadline(ctx, slot.Deadline)
	defer cancel()
	request, refs, err := c.sandboxRequest(ctx, address.Scope, slot.Record, profile)
	if err != nil {
		return SandboxRun{}, request, err
	}
	// Reuse the prefix cleared under the exclusive catalog lease on restart.
	f, err := os.CreateTemp(c.sandboxTemp, ".upload-prepare-*")
	if err != nil {
		return SandboxRun{}, request, err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	h := sha256.New()
	open := func(name string) (io.ReadCloser, error) {
		ref, ok := refs[name]
		if !ok {
			return nil, errBadRequest
		}
		revision, stream, err := c.Read(ctx, address.Scope, ref.Key, ref.Version)
		if err != nil {
			return nil, err
		}
		if revision.Scope != address.Scope || revision.Key != ref.Key || revision.Version != ref.Version || revision.SHA256 != ref.SHA256 {
			stream.Close()
			return nil, ErrConflict
		}
		return stream, nil
	}
	if err = nativeguest.WriteRequest(ctx, io.MultiWriter(f, h), request, open, nativeguest.DefaultLimits()); err != nil {
		return SandboxRun{}, request, err
	}
	if err = f.Sync(); err != nil {
		return SandboxRun{}, request, err
	}
	stat, err := f.Stat()
	if err != nil {
		return SandboxRun{}, request, err
	}
	input := SandboxBlob{SHA256: hex.EncodeToString(h.Sum(nil)), Size: stat.Size()}
	run, err := c.BeginSandbox(ctx, address, profile.Fingerprint, input)
	if err != nil {
		return SandboxRun{}, request, err
	}
	if _, err = f.Seek(0, 0); err != nil {
		return SandboxRun{}, request, err
	}
	run, err = c.SealSandboxInput(ctx, address, f)
	return run, request, err
}

func (c *Catalog) sandboxRequest(ctx context.Context, scope asset.Scope, record task.Record, profile SandboxExecutionProfile) (nativeguest.Request, map[string]task.Ref, error) {
	request := nativeguest.Request{Version: nativeguest.Protocol, AdapterID: profile.AdapterID,
		AdapterVersion: profile.AdapterVersion, RuntimeVersion: profile.RuntimeVersion, Spec: record.Spec}
	refs := make(map[string]task.Ref)
	add := func(name string, ref task.Ref) (nativeguest.BlobMeta, error) {
		if !ref.Key.Valid() || ref.Version < 1 || !sandboxDigest(ref.SHA256) || ref.ContentPath != ref.Key.ContentPath(ref.Version) {
			return nativeguest.BlobMeta{}, errBadRequest
		}
		var raw string
		err := c.db.QueryRowContext(ctx, "SELECT metadata FROM revisions WHERE scope=$1 AND kind=$2 AND asset_id=$3 AND version=$4", scope.Prefix(), ref.Kind, ref.ID, ref.Version).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nativeguest.BlobMeta{}, ErrMissing
		}
		if err != nil {
			return nativeguest.BlobMeta{}, err
		}
		var metadata asset.Metadata
		if err = json.Unmarshal([]byte(raw), &metadata); err != nil {
			return nativeguest.BlobMeta{}, err
		}
		if metadata.Key != ref.Key || metadata.SHA256 != ref.SHA256 {
			return nativeguest.BlobMeta{}, ErrConflict
		}
		refs[name] = ref
		return nativeguest.BlobMeta{Format: metadata.Format, MediaType: metadata.MediaType, SHA256: metadata.SHA256, Size: metadata.Size}, nil
	}
	if record.Parent != nil {
		workspace, err := add("workspace.tar", record.Parent.Workspace)
		if err != nil {
			return request, nil, err
		}
		session, err := add("parent.session", record.Parent.Session)
		if err != nil {
			return request, nil, err
		}
		request.ParentSessionID, request.Workspace, request.Session = record.Parent.SessionID, &workspace, &session
	}
	if err := task.ValidSkillPins(record.Skills); err != nil {
		return request, nil, errBadRequest
	}
	for i, pin := range record.Skills {
		blob, err := add("skills/"+strconv.Itoa(i)+".tar", pin.Source)
		if err != nil {
			return request, nil, err
		}
		request.Skills = append(request.Skills, nativeguest.Skill{Name: pin.Name, Blob: blob})
	}
	return request, refs, request.Validate()
}
