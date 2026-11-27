package claudecode

import (
	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/adapters/workspace"
	"github.com/intSheep/AgentMoor/internal/task"
)

type Adapter struct{ workspace.Layout }

func New() Adapter {
	return Adapter{workspace.Layout{
		ReadSessions:  sessions,
		TestedRuntime: "2.1.330 (Claude Code)",
		ID:            "claude-code",
		Version:       "0.3.0",
		Markers:       []string{".claude/", "CLAUDE.md"},
		SkillParents:  []string{".claude"},
		MaxDepth:      1,
	}}
}

func (a Adapter) Descriptor() adapter.Descriptor {
	d := a.Layout.Descriptor()
	d.TaskModes = []string{task.TextTurn, task.ContinueTurn}
	return d
}
