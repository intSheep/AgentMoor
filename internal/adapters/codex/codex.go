package codex

import (
	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/adapters/workspace"
	"github.com/intSheep/AgentMoor/internal/task"
)

type Adapter struct{ workspace.Layout }

func New() Adapter {
	return Adapter{workspace.Layout{
		ReadSessions:  sessions,
		TestedRuntime: "codex-cli 0.171.0-alpha.13",
		ID:            "codex",
		Version:       "0.5.0",
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
