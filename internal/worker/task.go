package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"agentmoor/internal/adapter"
	"agentmoor/internal/platform"
	"agentmoor/internal/task"
)

// RunTask handles one control-plane allocation. A replacement process never
// re-executes an already-claimed turn or overwrites an existing workspace.
func (e *Engine) RunTask(ctx context.Context, wait time.Duration) error {
	return e.runTask(ctx, wait, "")
}

func (e *Engine) RunTurn(ctx context.Context, turn string) error {
	id, err := uuid.Parse(turn)
	if err != nil || id == uuid.Nil || id.String() != turn {
		return fmt.Errorf("invalid turn ID")
	}
	return e.runTask(ctx, 0, turn)
}

func (e *Engine) runTask(ctx context.Context, wait time.Duration, turn string) error {
	c := e.Config
	if err := c.Validate(); err != nil {
		return err
	}
	isolated := c.ExecutionPolicy == ExecutionUntrustedIsolated && c.IsolatedProvider == KubernetesGVisorProvider
	if c.ExecutionPolicy == ExecutionUntrustedIsolated && !isolated {
		return ErrIsolationUnavailable
	}
	if isolated {
		if err := e.validateIsolatedTask(); err != nil {
			return err
		}
	} else if c.RuntimeCLI == "" || c.Sessions != "" || c.NativeSessions || (c.Artifacts != "" && c.Artifacts != "out") {
		return fmt.Errorf("run-task requires runtime_cli and manages its own sessions and out directory")
	}
	var g platform.Grant
	err := connectForTask(ctx, func() error {
		var authErr error
		g, authErr = e.authorize(ctx)
		return authErr
	})
	if err != nil {
		return err
	}
	if !g.Execute || !g.Read || !g.Write {
		return fmt.Errorf("run-task requires an execution credential")
	}
	route := "/v1/tasks/" + url.PathEscape(c.Scope.Runtime)
	if turn != "" {
		route += "/turns/" + turn
	}
	var record task.Record
	if wait < 0 {
		return fmt.Errorf("negative task wait")
	}
	until := time.Now().Add(wait)
	for {
		err = connectForTask(ctx, func() error { return e.Client.JSON(ctx, "GET", route, nil, &record) })
		if err == nil {
			break
		}
		if !errors.Is(err, platform.ErrMissing) || !time.Now().Before(until) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(time.Second, time.Until(until))):
		}
	}
	if record.State == task.Succeeded || record.State == task.Cancelled {
		return nil
	}
	if record.State != task.Queued {
		return fmt.Errorf("task is %s", record.State)
	}
	attempt := uuid.NewString()
	claim := task.Claim{Attempt: attempt, Skills: c.SkillPins}
	if isolated {
		claim.ExecutionPolicy = task.ExecutionUntrustedIsolated
	}
	if err = e.taskUpdate(ctx, route+"/claim", claim, &record); err != nil {
		return err
	}
	runCtx, cancel := context.WithDeadline(ctx, record.Deadline)
	defer cancel()
	stopped := make(chan struct{})
	go func() { defer close(stopped); e.monitorTask(runCtx, route, cancel) }()
	var session string
	var artifacts []task.Ref
	var runErr error
	if isolated {
		session, artifacts, runErr = e.executeIsolated(runCtx, record, route+"/sandbox", attempt)
	} else {
		session, artifacts, runErr = e.execute(runCtx, record)
	}
	cancel()
	<-stopped
	// A failed acknowledgement can be retried without starting another turn.
	finish := task.Finish{Attempt: attempt, SessionID: session, Artifacts: artifacts}
	if runErr != nil {
		finish.Error = "execution or asset publication failed"
	}
	finalCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	finishErr := e.taskUpdate(finalCtx, route+"/finish", finish, &record)
	if finishErr == nil && record.State == task.Cancelled {
		return nil
	}
	return errors.Join(runErr, finishErr)
}

// Kubernetes may report a ready endpoint just before the node's Service
// routing rules take effect. Retry only transport failures on pre-claim reads.
func connectForTask(ctx context.Context, read func() error) error {
	delay := 250 * time.Millisecond
	for attempt := 0; ; attempt++ {
		err := read()
		var networkErr *net.OpError
		if err == nil || !errors.As(err, &networkErr) || attempt >= 6 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, 4*time.Second)
	}
}

// Retrying a claim uses the same attempt id. Retrying a finish never reruns the agent.
func (e *Engine) taskUpdate(ctx context.Context, route string, body any, record *task.Record) error {
	var err error
	for i := range 3 {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		err = e.Client.JSON(ctx, "POST", route, body, record)
		if err == nil || errors.Is(err, platform.ErrConflict) || errors.Is(err, platform.ErrMissing) {
			break
		}
	}
	return err
}

func (e *Engine) monitorTask(ctx context.Context, route string, stop context.CancelFunc) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var state task.Record
			if err := e.Client.JSON(ctx, "GET", route, nil, &state); err == nil && state.State != task.Running {
				stop()
				return
			}
		}
	}
}

func (e *Engine) execute(ctx context.Context, record task.Record) (string, []task.Ref, error) {
	c := e.Config
	spec := record.Spec
	var err error
	if cp := record.Parent; cp != nil {
		err = e.Restore(ctx, cp.Workspace.Key, cp.Workspace.Version, c.Workspace)
	} else {
		err = os.Mkdir(c.Workspace, 0700)
	}
	if err != nil {
		return "", nil, fmt.Errorf("task needs a new workspace: %w", err)
	}
	root, err := os.OpenRoot(c.Workspace)
	if err != nil {
		return "", nil, err
	}
	defer root.Close()
	info, err := adapter.Inspect(ctx, e.Adapter, root, c.RuntimeCLI)
	if err != nil {
		return "", nil, err
	}
	if !info.Tested || (c.RuntimeVersion != "" && c.RuntimeVersion != info.RuntimeVersion) || (c.AdapterVersion != "" && c.AdapterVersion != info.Descriptor.Version) {
		return "", nil, fmt.Errorf("unverified task adapter/runtime version")
	}
	if record.Parent == nil {
		if err = root.WriteFile("input.txt", []byte(spec.Input), 0600); err != nil {
			return "", nil, err
		}
		if err = root.Mkdir("out", 0700); err != nil {
			return "", nil, err
		}
	}
	if err = os.MkdirAll(c.StateDir, 0700); err != nil {
		return "", nil, err
	}
	home, err := os.MkdirTemp(c.StateDir, "task-home-*")
	if err != nil {
		return "", nil, err
	}
	// Keep the private native history available for inspection after a failed publication.
	env := task.Environment{Executable: c.RuntimeCLI, Workspace: c.Workspace, Home: home}
	env.Skills, err = e.installTaskSkills(ctx, env, record.Skills)
	if err != nil {
		return "", nil, err
	}
	var output task.Execution
	target := filepath.Join("out", spec.Output)
	if spec.Mode == task.FileTurn {
		files, ok := e.Adapter.(task.FileExecutor)
		if !ok {
			return "", nil, fmt.Errorf("adapter does not support file turns")
		}
		staged := ".result-" + uuid.NewString()
		if _, err := root.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
			return "", nil, fmt.Errorf("file turn needs a fresh result path")
		}
		var session string
		var history task.RestoreHistory
		if cp := record.Parent; cp != nil {
			session, history = cp.SessionID, pinnedHistory{ctx: ctx, engine: e, ref: cp.Session}
		}
		output, err = files.ExecuteFile(ctx, env, spec, staged, session, history)
		if err == nil && session != "" && output.SessionID != session {
			return "", nil, fmt.Errorf("native session changed on continuation")
		}
		if err == nil {
			info, statErr := root.Lstat(staged)
			if statErr != nil || !info.Mode().IsRegular() {
				return "", nil, fmt.Errorf("file tool did not produce a new regular file")
			}
			err = root.Rename(staged, target)
		}
	} else {
		executor, ok := e.Adapter.(task.Executor)
		if !ok {
			return "", nil, fmt.Errorf("adapter does not support text turns")
		}
		output, err = e.runText(ctx, executor, env, record)
		if err == nil {
			err = root.WriteFile(target, []byte(output.Text), 0600)
		}
	}
	if err != nil {
		return "", nil, err
	}
	// The journal must remain outside both the workspace and the history root.
	c.StateDir = filepath.Join(c.StateDir, "publish")
	c.Artifacts = "out"
	c.Sessions = output.HistoryDir
	c.ArtifactSession = output.SessionID
	publisher := Engine{Config: c, Client: e.Client, Adapter: e.Adapter}
	var artifacts []task.Ref
	if err = publisher.sync(ctx, record.TurnID, &artifacts); err != nil {
		return "", nil, err
	}
	_ = os.RemoveAll(home)
	return output.SessionID, artifacts, nil
}

func (e *Engine) runText(ctx context.Context, executor task.Executor, env task.Environment, record task.Record) (task.Execution, error) {
	cp := record.Parent
	if cp == nil {
		return executor.ExecuteTask(ctx, env, record.Spec)
	}
	resume, ok := executor.(task.Continuation)
	if !ok {
		return task.Execution{}, fmt.Errorf("adapter does not support continuation")
	}
	history := pinnedHistory{ctx: ctx, engine: e, ref: cp.Session}
	output, err := resume.ContinueTask(ctx, env, cp.SessionID, record.Spec.Instruction, history)
	if err == nil && output.SessionID != cp.SessionID {
		return task.Execution{}, fmt.Errorf("native session changed on continuation")
	}
	return output, err
}

type pinnedHistory struct {
	ctx    context.Context
	engine *Engine
	ref    task.Ref
}

func (h pinnedHistory) Files(dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	return h.engine.Restore(h.ctx, h.ref.Key, h.ref.Version, dest)
}

func (h pinnedHistory) NativeDatabase(dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	return h.engine.RestoreNative(h.ctx, h.ref.ID, h.ref.Version, dest)
}
