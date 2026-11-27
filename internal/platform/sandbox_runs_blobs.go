package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
)

func (c *Catalog) freezeSandboxBlob(ctx context.Context, expected SandboxBlob, source io.Reader) (string, error) {
	if !expected.valid() || source == nil {
		return "", errBadRequest
	}
	file, err := os.CreateTemp(c.sandboxTemp, ".upload-*")
	if err != nil {
		return "", err
	}
	name, ok := file.Name(), false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(name)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, h), sandboxContextReader{ctx, io.LimitReader(source, expected.Size+1)})
	if err != nil {
		return "", err
	}
	if n != expected.Size || hex.EncodeToString(h.Sum(nil)) != expected.SHA256 {
		return "", errBadRequest
	}
	if err = errors.Join(file.Sync(), file.Close()); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

// The input candidate was already committed by BeginSandbox. Slow client input
// is frozen outside the mutex, so it cannot block task cancellation or reads.
func (c *Catalog) SealSandboxInput(ctx context.Context, a SandboxAddress, source io.Reader) (SandboxRun, error) {
	r, err := c.loadSandbox(ctx, a)
	if err != nil {
		return SandboxRun{}, err
	}
	if err = c.sandboxActive(ctx, r); err != nil {
		return SandboxRun{}, err
	}
	if r.Input.Sealed {
		return r, nil
	}
	file, err := c.freezeSandboxBlob(ctx, r.Input, source)
	if err != nil {
		return SandboxRun{}, err
	}
	defer os.Remove(file)
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err = c.loadSandbox(ctx, a)
	if err != nil {
		return SandboxRun{}, err
	}
	if err = c.sandboxActive(ctx, r); err != nil {
		return SandboxRun{}, err
	}
	if r.Input.Sealed {
		return r, nil
	}
	if err = c.sandboxWriteGuard(ctx, r); err != nil {
		return SandboxRun{}, err
	}
	if err = c.prepareObjectPut(ctx, r.Scope, objectSandboxInput, r.Input.SHA256); err != nil {
		return SandboxRun{}, err
	}
	if err = c.objects.Put(ctx, sandboxObjectKey(r.Scope, SandboxInput, r.Input.SHA256), file, r.Input.Size); err != nil {
		return SandboxRun{}, err
	}
	r.Input.Sealed = true
	r.State = SandboxPrepared
	err = c.saveSandbox(ctx, &r, "", nil)
	return r, err
}

// The first output declaration is registered before any object Put. Only its
// identical frozen digest and size may be retried after a lost acknowledgement.
func (c *Catalog) SealSandboxOutput(ctx context.Context, token string, output SandboxBlob, source io.Reader) (SandboxRun, error) {
	if !output.valid() || output.Sealed {
		return SandboxRun{}, errBadRequest
	}
	r, err := c.bindSandboxOutput(ctx, token, output)
	if err != nil || r.Output.Sealed {
		return r, err
	}
	file, err := c.freezeSandboxBlob(ctx, output, source)
	if err != nil {
		return SandboxRun{}, err
	}
	defer os.Remove(file)
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err = c.AuthenticateSandboxCapability(ctx, token, SandboxOutput)
	if err != nil {
		return SandboxRun{}, err
	}
	ctx = auditCapabilityContext(ctx, SandboxOutput)
	if r.Output.SHA256 != output.SHA256 || r.Output.Size != output.Size {
		return SandboxRun{}, ErrConflict
	}
	if r.Output.Sealed {
		return r, nil
	}
	if err = c.sandboxWriteGuard(ctx, r); err != nil {
		return SandboxRun{}, err
	}
	ctx = objectCollectionRunContext(ctx, r)
	if err = c.prepareObjectPut(ctx, r.Scope, objectSandboxOutput, output.SHA256); err != nil {
		return SandboxRun{}, err
	}
	if err = c.objects.Put(ctx, sandboxObjectKey(r.Scope, SandboxOutput, output.SHA256), file, output.Size); err != nil {
		return SandboxRun{}, err
	}
	r.Output.Sealed = true
	err = c.saveSandbox(ctx, &r, "input-model", nil)
	return r, err
}

func (c *Catalog) bindSandboxOutput(ctx context.Context, token string, output SandboxBlob) (SandboxRun, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.AuthenticateSandboxCapability(ctx, token, SandboxOutput)
	if err != nil {
		return SandboxRun{}, err
	}
	ctx = auditCapabilityContext(ctx, SandboxOutput)
	if r.Output.SHA256 != "" {
		if r.Output.SHA256 != output.SHA256 || r.Output.Size != output.Size {
			return SandboxRun{}, ErrConflict
		}
		return r, nil
	}
	r.Output = output
	err = c.saveSandbox(ctx, &r, "", nil)
	return r, err
}

func (c *Catalog) ReadSandboxInput(ctx context.Context, token string) (SandboxBlob, io.ReadCloser, error) {
	r, err := c.AuthenticateSandboxCapability(ctx, token, SandboxInput)
	if err != nil {
		return SandboxBlob{}, nil, err
	}
	if !r.Input.Sealed {
		return SandboxBlob{}, nil, ErrNotReady
	}
	return c.readSandboxBlob(ctx, r, SandboxInput, r.Input)
}

// A protected Worker may re-read a completed result after Finish succeeded but
// its acknowledgement was lost. Guest capabilities never regain this access.
func (c *Catalog) ReadSandboxOutput(ctx context.Context, a SandboxAddress) (SandboxBlob, io.ReadCloser, error) {
	r, err := c.loadSandbox(ctx, a)
	if err != nil {
		return SandboxBlob{}, nil, err
	}
	if r.Collected {
		return SandboxBlob{}, nil, ErrMissing
	}
	if err = c.sandboxCurrent(ctx, r, true); err != nil {
		return SandboxBlob{}, nil, err
	}
	if r.State != SandboxReady || !r.PodTerminal || r.ExitCode != 0 || !r.PodDeleted || !r.Output.Sealed {
		return SandboxBlob{}, nil, ErrNotReady
	}
	return c.readSandboxBlob(ctx, r, SandboxOutput, r.Output)
}

func (c *Catalog) readSandboxBlob(ctx context.Context, r SandboxRun, purpose SandboxPurpose, blob SandboxBlob) (SandboxBlob, io.ReadCloser, error) {
	stream, err := c.objects.Get(ctx, sandboxObjectKey(r.Scope, purpose, blob.SHA256))
	if err != nil {
		return SandboxBlob{}, nil, err
	}
	return blob, &sandboxVerifiedReader{ctx: ctx, stream: stream, reader: io.LimitReader(stream, blob.Size+1), expected: blob, hash: sha256.New()}, nil
}

type sandboxContextReader struct {
	ctx context.Context
	in  io.Reader
}

func (r sandboxContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.in.Read(p)
}

type sandboxVerifiedReader struct {
	ctx      context.Context
	stream   io.ReadCloser
	reader   io.Reader
	expected SandboxBlob
	hash     hash.Hash
	read     int64
	failure  error
}

func (r *sandboxVerifiedReader) Read(p []byte) (int, error) {
	if r.failure != nil {
		return 0, r.failure
	}
	if err := r.ctx.Err(); err != nil {
		r.failure = err
		return 0, err
	}
	n, err := r.reader.Read(p)
	r.read += int64(n)
	r.hash.Write(p[:n])
	if r.read > r.expected.Size || (err == io.EOF && (r.read != r.expected.Size || hex.EncodeToString(r.hash.Sum(nil)) != r.expected.SHA256)) {
		r.failure = ErrConflict
		return n, r.failure
	}
	if err != nil && err != io.EOF {
		r.failure = err
	}
	return n, err
}

func (r *sandboxVerifiedReader) Close() error { return r.stream.Close() }
