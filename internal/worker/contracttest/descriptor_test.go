package contracttest_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"agentmoor/internal/adapter"
	"agentmoor/internal/adapters/claudecode"
	"agentmoor/internal/adapters/codex"
	"agentmoor/internal/adapters/gemini"
	"agentmoor/internal/adapters/opencode"
	"agentmoor/internal/task"
	"agentmoor/internal/worker/contracttest"
)

type discoveryFixture struct{ descriptor adapter.Descriptor }

func (a discoveryFixture) Descriptor() adapter.Descriptor { return a.descriptor }
func (discoveryFixture) Detect(context.Context, *os.Root) ([]string, error) {
	return nil, nil
}

type taskFixture struct{ discoveryFixture }

func (taskFixture) ExecuteTask(context.Context, task.Environment, task.Spec) (task.Execution, error) {
	return task.Execution{}, nil
}
func (taskFixture) ContinueTask(context.Context, task.Environment, string, string, task.RestoreHistory) (task.Execution, error) {
	return task.Execution{}, nil
}
func (taskFixture) ExecuteFile(context.Context, task.Environment, task.Spec, string, string, task.RestoreHistory) (task.Execution, error) {
	return task.Execution{}, nil
}

// Distinct single-method fixtures expose an incorrect interface match: a
// text executor is not a file executor merely because both execute tasks.
type textOnlyFixture struct{ discoveryFixture }

func (textOnlyFixture) ExecuteTask(context.Context, task.Environment, task.Spec) (task.Execution, error) {
	return task.Execution{}, nil
}

type fileOnlyFixture struct{ discoveryFixture }

func (fileOnlyFixture) ExecuteFile(context.Context, task.Environment, task.Spec, string, string, task.RestoreHistory) (task.Execution, error) {
	return task.Execution{}, nil
}

type nativeFixture struct{ discoveryFixture }

func (nativeFixture) ExportNative(context.Context, string, string, string) error { return nil }
func (nativeFixture) ImportNative(context.Context, string, string, string, string) error {
	return nil
}

func TestValidateDescriptorExistingAdaptersAndDiscoveryOnly(t *testing.T) {
	for _, a := range []adapter.Adapter{
		codex.New(), claudecode.New(), gemini.New(), opencode.New(),
		discoveryFixture{adapter.Descriptor{ID: "discovery-only", Version: "fixture-build"}},
	} {
		t.Run(a.Descriptor().ID, func(t *testing.T) {
			if err := contracttest.ValidateDescriptor(a); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateDescriptorTaskClaimNeedsItsImplementation(t *testing.T) {
	for _, mode := range []string{task.TextTurn, task.ContinueTurn, task.FileTurn} {
		t.Run(mode, func(t *testing.T) {
			declared := discoveryFixture{adapter.Descriptor{ID: "synthetic-task", Version: "fixture-build", TaskModes: []string{mode}}}
			if err := contracttest.ValidateDescriptor(declared); err == nil {
				t.Fatal("an otherwise valid discovery-only adapter claimed an execution method it lacks")
			}
			// Same descriptor, now with actual methods. This rules out rejecting
			// the synthetic ID or the declaration string regardless of capability.
			if err := contracttest.ValidateDescriptor(taskFixture{declared}); err != nil {
				t.Fatal("matching implementation was rejected", err)
			}
		})
	}
}

func TestValidateDescriptorDoesNotSubstituteAnotherExecutionInterface(t *testing.T) {
	d := discoveryFixture{adapter.Descriptor{ID: "synthetic-file", Version: "fixture-build", TaskModes: []string{task.FileTurn}}}
	if err := contracttest.ValidateDescriptor(textOnlyFixture{d}); err == nil {
		t.Fatal("a text-only implementation falsely claimed file execution")
	}
	if err := contracttest.ValidateDescriptor(fileOnlyFixture{d}); err != nil {
		t.Fatal("the same file declaration with its exact implementation was rejected", err)
	}
}

func TestValidateDescriptorNativeClaimNeedsInterfaceAndTestedRuntime(t *testing.T) {
	d := adapter.Descriptor{ID: "synthetic-native", Version: "fixture-build", TestedRuntime: "fixture runtime 1",
		Capabilities: map[adapter.AssetKind][]adapter.Capability{
			adapter.Session: {{Operation: "export", Mode: "runtime-cli", Reliability: "synthetic-description"}},
		}}
	if err := contracttest.ValidateDescriptor(discoveryFixture{d}); err == nil {
		t.Fatal("Session/runtime-cli passed without NativeSessions")
	}
	if err := contracttest.ValidateDescriptor(nativeFixture{discoveryFixture{d}}); err != nil {
		t.Fatal("native interface and tested runtime were rejected", err)
	}
	d.TestedRuntime = ""
	if err := contracttest.ValidateDescriptor(nativeFixture{discoveryFixture{d}}); err == nil {
		t.Fatal("Session/runtime-cli passed without its tested runtime")
	}
}

func TestValidateDescriptorKeepsOpaquePoliciesAndExtraMethods(t *testing.T) {
	d := adapter.Descriptor{ID: "synthetic-discovery", Version: "operator-build-label",
		Capabilities: map[adapter.AssetKind][]adapter.Capability{
			adapter.Session: {{Operation: "future-description", Mode: "future-capability-mode", Reliability: "not-a-ranked-policy"}},
		}}
	// The helper does not certify these opaque capability semantics, prohibit
	// extra methods, require SkillInstaller/FileExecutor on every adapter, or
	// turn implementation versions into a SemVer compatibility commitment.
	for _, a := range []adapter.Adapter{discoveryFixture{d}, taskFixture{discoveryFixture{d}}, nativeFixture{discoveryFixture{d}}} {
		if err := contracttest.ValidateDescriptor(a); err != nil {
			t.Fatal(err)
		}
	}
	d.TaskModes = []string{"future-task-mode.v9"}
	if err := contracttest.ValidateDescriptor(taskFixture{discoveryFixture{d}}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatal("unknown task mode was silently certified by this helper", err)
	}
}

func TestValidateDescriptorRequiresRegistryIdentifiers(t *testing.T) {
	for _, d := range []adapter.Descriptor{{Version: "fixture"}, {ID: "fixture"}} {
		if err := contracttest.ValidateDescriptor(discoveryFixture{d}); err == nil {
			t.Fatal("descriptor missing a Registry identifier passed")
		}
	}
}
