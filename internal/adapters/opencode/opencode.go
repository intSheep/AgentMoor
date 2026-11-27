package opencode

import (
	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/adapters/workspace"
	"github.com/intSheep/AgentMoor/internal/task"
)

type Adapter struct{ workspace.Layout }

func New() Adapter {
	return Adapter{workspace.Layout{
		ReadSessions:              sessions,
		TestedRuntime:             "1.19.6",
		ID:                        "opencode",
		Version:                   "0.3.0",
		Markers:                   []string{".opencode/", "opencode.json", "opencode.jsonc"},
		SkillParents:              []string{".opencode", ".claude", ".agents"},
		MaxDepth:                  6,
		RootFilePattern:           "*.md",
		SkipHiddenAndDependencies: true,
	}}
}

func (a Adapter) Descriptor() adapter.Descriptor {
	d := a.Layout.Descriptor()
	d.TaskModes = []string{task.TextTurn, task.ContinueTurn}
	d.Capabilities[adapter.Session] = append(d.Capabilities[adapter.Session],
		adapter.Capability{Operation: "export", Mode: "runtime-cli", Reliability: "tested-main-session"},
		adapter.Capability{Operation: "restore", Mode: "runtime-cli", Reliability: "tested-main-session"},
	)
	return d
}
