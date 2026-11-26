package codex

import (
	"agentmoor/internal/adapter"
	"agentmoor/internal/adapters/workspace"
	"agentmoor/internal/task"
)

type Adapter struct{ workspace.Layout }

func New() Adapter {
	return Adapter{workspace.Layout{
		ReadSessions:  sessions,
		TestedRuntime: "codex-cli 0.171.0-alpha.13",
		ID:            "codex",
		Version:       "0.4.0",
		Markers:       []string{".codex/"},
		SkillParents:  []string{".agents"},
		MaxDepth:      1,
	}}
}

func (a Adapter) Descriptor() adapter.Descriptor {
	d := a.Layout.Descriptor()
	d.TaskModes = []string{task.TextTurn, task.ContinueTurn, task.FileTurn}
	return d
}
