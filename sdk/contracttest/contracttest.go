// Package contracttest exposes the preview synthetic Adapter asset suite.
// This opt-in test helper uses Worker and local platform fixtures; ordinary
// Adapter implementations need only sdk and do not import this package.
// Passing the suite is not vendor CLI, isolation, or independent engineer proof.
package contracttest

import (
	"testing"

	internal "github.com/intSheep/AgentMoor/internal/worker/contracttest"
	"github.com/intSheep/AgentMoor/sdk"
)

type (
	Mechanism     = internal.Mechanism
	Paths         = internal.Paths
	Calls         = internal.Calls
	NativeFixture = internal.NativeFixture
	Fixture       = internal.Fixture
	Case          = internal.Case
)

const (
	Filesystem = internal.Filesystem
	NativeCLI  = internal.NativeCLI
)

// Run delegates the existing bounded synthetic asset contract to the shared
// runner. Cases provide Adapter-specific inputs, not platform pass assertions.
func Run(t *testing.T, c Case) {
	t.Helper()
	internal.Run(t, c)
}

// ValidateDescriptor checks the relationships understood by this test helper;
// unknown TaskModes are unsupported here, not prohibited by Core Registry.
// Other capability modes and reliability labels are unverified descriptions.
func ValidateDescriptor(a sdk.Adapter) error {
	return internal.ValidateDescriptor(a)
}
