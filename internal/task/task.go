// Package task contains the SaaS-facing, runtime-independent task contract.
package task

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"time"

	"agentmoor/internal/asset"
)

const (
	Queued      = "queued"
	Running     = "running"
	Cancelling  = "cancelling"
	Cancelled   = "cancelled"
	Interrupted = "interrupted"
	Succeeded   = "succeeded"
	Failed      = "failed"
)

const ReplyFile = "reply.txt"

const TextTurn = "text-turn.v1"
const ContinueTurn = "continue-turn.v1"
const FileTurn = "file-turn.v1"

// MaxArtifacts bounds the delivery receipt carried by the Worker finish call.
const MaxArtifacts = 128

// Prompt contains only the two SaaS fields passed to the native CLI on stdin.
func (s Spec) Prompt() []byte {
	b, _ := json.Marshal(struct {
		Instruction string `json:"instruction"`
		Input       string `json:"input"`
	}{s.Instruction, s.Input})
	return b
}

const Timeout = 15 * time.Minute

// Spec is immutable within a task. A new logical runtime id allocates a new task.
type Spec struct {
	Mode        string `json:"mode,omitempty"`
	Instruction string `json:"instruction"`
	Input       string `json:"input"`
	Output      string `json:"output"`
}

func (s Spec) Valid() bool {
	return (s.Mode == "" || s.Mode == TextTurn || s.Mode == FileTurn) && strings.TrimSpace(s.Instruction) != "" && len(s.Instruction) <= 8192 && len(s.Input) <= 65536 && asset.SafePath(s.Output) && path.Base(s.Output) == s.Output && len(s.Output) <= 128
}

// Executor implements one non-interactive text turn and reports its native history.
type Executor interface {
	ExecuteTask(context.Context, Environment, Spec) (Execution, error)
}
type Environment struct{ Executable, Workspace, Home string }

// FileExecutor writes the delivery to a fresh workspace-relative result path.
// A continuation receives the immutable parent session and restored workspace.
type FileExecutor interface {
	ExecuteFile(ctx context.Context, env Environment, spec Spec, result, session string, restore RestoreHistory) (Execution, error)
}
type Execution struct{ Text, SessionID, HistoryDir string }

// Continuation restores the pinned native transcript into the adapter's private layout.
type Continuation interface {
	ContinueTask(ctx context.Context, env Environment, sessionID, message string, restore RestoreHistory) (Execution, error)
}

// RestoreHistory verifies and installs the parent revision at a fresh path.
type RestoreHistory interface {
	Files(destination string) error
	NativeDatabase(destination string) error
}

type Question struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

type Checkpoint struct {
	SessionID string `json:"session_id"`
	Session   Ref    `json:"session"`
	Workspace Ref    `json:"workspace"`
}

func TurnKey(turn string, key asset.Key) asset.Key {
	if turn != "" {
		key.ID = "turns/" + turn + "/" + key.ID
	}
	return key
}

type Ref struct {
	asset.Key
	Version     int64  `json:"version"`
	SHA256      string `json:"sha256"`
	ContentPath string `json:"content_path"`
}
type Record struct {
	TurnID   string      `json:"turn_id,omitempty"`
	Parent   *Checkpoint `json:"parent,omitempty"`
	ID       string      `json:"id"`
	State    string      `json:"state"`
	Spec     Spec        `json:"spec"`
	Deadline time.Time   `json:"deadline,omitempty,omitzero"`
	Assets   []Ref       `json:"assets,omitempty"`
	Error    string      `json:"error,omitempty"`
}
type Claim struct {
	Attempt string `json:"attempt"`
}
type Finish struct {
	Attempt   string `json:"attempt"`
	SessionID string `json:"session_id,omitempty"`
	Error     string `json:"error,omitempty"`
	// Artifacts pins the complete delivery directory for this successful turn.
	// An omitted list retains the original single-output worker protocol.
	Artifacts []Ref `json:"artifacts,omitempty"`
}
