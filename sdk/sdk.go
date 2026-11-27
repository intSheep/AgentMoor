// Package sdk exposes the preview Go Adapter contracts used by AgentMoor.
// Aliases preserve the existing Core type identities. This package does not
// load plugins, grant platform authority, or establish a v1 compatibility policy.
package sdk

import (
	"context"
	"io"
	"os"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/snapshot"
	"github.com/intSheep/AgentMoor/internal/task"
)

type (
	Adapter         = adapter.Adapter
	Descriptor      = adapter.Descriptor
	Capability      = adapter.Capability
	AssetKind       = adapter.AssetKind
	SkillRef        = adapter.SkillRef
	SkillDiscoverer = adapter.SkillDiscoverer
	SessionSource   = adapter.SessionSource
	NativeSession   = adapter.NativeSession
	NativeSessions  = adapter.NativeSessions
	Registry        = adapter.Registry
	RuntimeInfo     = adapter.RuntimeInfo

	// Entry maps an assigned Session source path into its original archive name.
	Entry = snapshot.Entry

	Executor       = task.Executor
	Continuation   = task.Continuation
	FileExecutor   = task.FileExecutor
	SkillInstaller = task.SkillInstaller
	Environment    = task.Environment
	Spec           = task.Spec
	Execution      = task.Execution
	LoadedSkill    = task.LoadedSkill
	RestoreHistory = task.RestoreHistory
)

const (
	Session   = adapter.Session
	Skill     = adapter.Skill
	Artifact  = adapter.Artifact
	Workspace = adapter.Workspace

	TextTurn     = task.TextTurn
	ContinueTurn = task.ContinueTurn
	FileTurn     = task.FileTurn
)

// NewRegistry uses the existing startup registry and its identifier checks.
// Registration does not load the Adapter into an existing agentmoor binary or
// certify its declared capabilities, runtime compatibility, or isolation.
func NewRegistry(adapters ...Adapter) (*Registry, error) {
	return adapter.NewRegistry(adapters...)
}

// Inspect discovers workspace evidence and, when exe is supplied, observes its
// version using private temporary runtime homes. Tested reports an exact match;
// a mismatch alone is not an error and callers must apply their version policy.
func Inspect(ctx context.Context, a Adapter, root *os.Root, exe string) (RuntimeInfo, error) {
	return adapter.Inspect(ctx, a, root, exe)
}

// RunCLI invokes an operator-trusted executable with bounded stdout and the
// existing minimal inherited environment and cancellation behavior. Explicit env
// entries remain the caller's responsibility. This is not a sandbox boundary.
func RunCLI(ctx context.Context, exe, workspace string, env []string, out io.Writer, limit int64, args ...string) error {
	return adapter.RunCLI(ctx, exe, workspace, env, out, limit, args...)
}

// RunCLIInput is RunCLI with an explicit stdin stream; it adds no shell or
// authority and preserves the existing trusted native invocation behavior.
func RunCLIInput(ctx context.Context, exe, workspace string, env []string, input io.Reader, out io.Writer, limit int64, args ...string) error {
	return adapter.RunCLIInput(ctx, exe, workspace, env, input, out, limit, args...)
}
