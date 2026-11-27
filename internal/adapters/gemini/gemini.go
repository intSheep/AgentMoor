package gemini

import (
	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/adapters/workspace"
	"github.com/intSheep/AgentMoor/internal/task"
)

type Adapter struct{ workspace.Layout }

func New() Adapter {
	return Adapter{workspace.Layout{
		ReadSessions:              sessions,
		TestedRuntime:             "0.70.0",
		ID:                        "gemini-cli",
		Version:                   "0.3.0",
		Markers:                   []string{".gemini/", "GEMINI.md"},
		SkillParents:              []string{".agents", ".gemini"},
		MaxDepth:                  1,
		RootFilePattern:           "SKILL.md",
		SkipHiddenAndDependencies: true,
	}}
}

func (a Adapter) Descriptor() adapter.Descriptor {
	d := a.Layout.Descriptor()
	d.TaskModes = []string{task.TextTurn, task.ContinueTurn}
	return d
}
