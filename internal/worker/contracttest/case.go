// Package contracttest provides repository-internal Adapter acceptance fixtures.
// It is a test helper, not the externally supported Adapter SDK.
package contracttest

import (
	"testing"

	"agentmoor/internal/adapter"
)

type Mechanism string

const (
	Filesystem Mechanism = "filesystem"
	NativeCLI  Mechanism = "native-cli"
)

// Runtime is separate from Workspace and Sessions so call recording never
// creates a new workspace revision or changes the native Session payload.
type Paths struct{ Workspace, Sessions, Runtime string }

type Calls struct{ Version, List, Export, Import int }

type NativeFixture struct {
	Executable     string
	Calls          func(*testing.T) Calls
	SetVersion     func(*testing.T, string)
	AssertImported func(*testing.T, string)
}

type Fixture struct {
	Adapter                  adapter.Adapter
	Mechanism                Mechanism
	SessionID, SessionFormat string
	SkillManifest            string
	SessionFiles             map[string][]byte // Raw Session uses one empty key.
	Native                   *NativeFixture
}

// Prepare owns only Adapter-specific synthetic files and the CLI fixture.
// Run owns authorization, HTTP, Scope, state and the shared assertions.
type Case struct {
	Name    string
	Prepare func(*testing.T, Paths) Fixture
}
