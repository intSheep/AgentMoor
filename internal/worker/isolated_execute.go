package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"agentmoor/internal/platform"
	"agentmoor/internal/task"
)

var errIsolatedCleanup = errors.New("isolated sandbox deletion was not confirmed")

func (e *Engine) validateIsolatedTask() error {
	c := e.Config
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || e.Adapter == nil ||
		c.RuntimeCLI != "" || c.Sessions != "" || c.NativeSessions || c.ArtifactSession != "" || (c.Artifacts != "" && c.Artifacts != "out") {
		return fmt.Errorf("isolated tasks require HTTPS and a data-only Worker without a native CLI or session directory")
	}
	d := e.Adapter.Descriptor()
	if d.ID != "codex" || c.Scope.Adapter != d.ID || d.TestedRuntime == "" ||
		(c.AdapterVersion != "" && c.AdapterVersion != d.Version) || (c.RuntimeVersion != "" && c.RuntimeVersion != d.TestedRuntime) {
		return fmt.Errorf("unverified isolated adapter/runtime allocation")
	}
	return nil
}

func sandboxTransient(err error) bool {
	var status *platform.HTTPError
	if errors.As(err, &status) {
		return status.StatusCode >= 500 || status.StatusCode == 429
	}
	var network net.Error
	return errors.As(err, &network) || errors.Is(err, context.DeadlineExceeded)
}

func sandboxPause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (e *Engine) executeIsolated(ctx context.Context, record task.Record, path, attempt string) (session string, artifacts []task.Ref, runErr error) {
	if record.ExecutionPolicy != task.ExecutionUntrustedIsolated {
		return "", nil, fmt.Errorf("control plane did not freeze the isolated execution policy")
	}
	// Even a lost Start response may have created the Pod. Cleanup always uses
	// the same private attempt; neither recovery nor cleanup allocates a new one.
	defer func() {
		if runErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if err := e.cleanupIsolated(cleanupCtx, path, attempt); err != nil {
				runErr = errors.Join(runErr, errIsolatedCleanup, err)
			}
		}
	}()
	status, err := e.Client.SandboxTask(ctx, path, "", attempt)
	for retry := 0; err != nil && retry < 3 && sandboxTransient(err); retry++ {
		if err = sandboxPause(ctx, 250*time.Millisecond); err != nil {
			break
		}
		status, err = e.Client.SandboxTask(ctx, path, "/status", attempt)
		if errors.Is(err, platform.ErrMissing) {
			// No durable input/run exists yet. The same claim can prepare it;
			// the persisted create fence still decides whether POST is allowed.
			status, err = e.Client.SandboxTask(ctx, path, "", attempt)
		}
	}
	if err != nil {
		return "", nil, err
	}
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		r, q := status.Run, status.Request
		d := e.Adapter.Descriptor()
		if r.Scope != e.Config.Scope || r.TurnID != record.TurnID || !sameAllocation(record, r.Source) || !r.Deadline.Equal(record.Deadline) ||
			q.Spec != record.Spec || q.AdapterID != d.ID || q.AdapterVersion != d.Version || q.RuntimeVersion != d.TestedRuntime {
			return "", nil, fmt.Errorf("sandbox status differs from assigned isolated task")
		}
		switch r.State {
		case platform.SandboxReady:
			reader, err := e.Client.SandboxOutput(ctx, path, attempt, r.Output)
			if err != nil {
				return "", nil, err
			}
			session, artifacts, err := e.publishSandboxResult(ctx, record, status, reader)
			return session, artifacts, errors.Join(err, reader.Close())
		case platform.SandboxFailed, platform.SandboxCancelled, platform.SandboxExpired:
			return "", nil, fmt.Errorf("isolated sandbox stopped: %s", r.State)
		case platform.SandboxPreparing, platform.SandboxPrepared, platform.SandboxCreating, platform.SandboxRunning:
		default:
			return "", nil, fmt.Errorf("unknown isolated sandbox state")
		}
		if err = sandboxPause(ctx, time.Second); err != nil {
			return "", nil, err
		}
		next, err := e.Client.SandboxTask(ctx, path, "/status", attempt)
		if err != nil {
			failures++
			if !sandboxTransient(err) || failures >= 3 {
				return "", nil, err
			}
			continue
		}
		status, failures = next, 0
	}
}

func (e *Engine) cleanupIsolated(ctx context.Context, path, attempt string) error {
	for {
		status, err := e.Client.SandboxTask(ctx, path, "/cancel", attempt)
		if errors.Is(err, platform.ErrMissing) {
			return nil
		}
		if err == nil && (status.Run.PodDeleted || !status.Run.CreateStarted) {
			return nil
		}
		if err != nil && !sandboxTransient(err) {
			return err
		}
		if err = sandboxPause(ctx, 250*time.Millisecond); err != nil {
			return fmt.Errorf("isolated sandbox deletion was not confirmed: %w", err)
		}
	}
}
