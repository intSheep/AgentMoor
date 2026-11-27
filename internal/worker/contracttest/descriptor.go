package contracttest

import (
	"fmt"

	"agentmoor/internal/adapter"
	"agentmoor/internal/task"
)

// ValidateDescriptor checks minimum identifiers and the declaration/interface
// relationships this internal helper knows. Unknown task modes are outside this
// helper's scope; their rejection does not change production Registry policy.
// Other capability modes and reliability labels remain unverified descriptions,
// except Session/runtime-cli, which requires NativeSessions and TestedRuntime.
// This does not certify runtime compatibility or impose a versioning policy.
func ValidateDescriptor(a adapter.Adapter) error {
	if a == nil {
		return fmt.Errorf("adapter is required")
	}
	d := a.Descriptor()
	if d.ID == "" || d.Version == "" {
		return fmt.Errorf("adapter ID and version are required")
	}
	for _, mode := range d.TaskModes {
		switch mode {
		case task.TextTurn:
			if _, ok := a.(task.Executor); !ok {
				return fmt.Errorf("adapter %q declares %q without task.Executor", d.ID, mode)
			}
		case task.ContinueTurn:
			if _, ok := a.(task.Continuation); !ok {
				return fmt.Errorf("adapter %q declares %q without task.Continuation", d.ID, mode)
			}
		case task.FileTurn:
			if _, ok := a.(task.FileExecutor); !ok {
				return fmt.Errorf("adapter %q declares %q without task.FileExecutor", d.ID, mode)
			}
		default:
			return fmt.Errorf("adapter %q task mode %q is unsupported by this conformance helper", d.ID, mode)
		}
	}
	for _, capability := range d.Capabilities[adapter.Session] {
		if capability.Mode != "runtime-cli" {
			continue
		}
		if _, ok := a.(adapter.NativeSessions); !ok {
			return fmt.Errorf("adapter %q declares Session/runtime-cli without adapter.NativeSessions", d.ID)
		}
		if d.TestedRuntime == "" {
			return fmt.Errorf("adapter %q declares Session/runtime-cli without a tested runtime", d.ID)
		}
	}
	return nil
}
