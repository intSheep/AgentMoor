// Package opencodecase supplies a bounded synthetic CLI for Adapter conformance.
// It exercises the existing NativeSessions methods, not a vendor binary or its
// database implementation. All fixture inputs and call records stay in Runtime.
package opencodecase

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentmoor/internal/adapters/opencode"
	"agentmoor/internal/worker/contracttest"
)

const sessionID = "ses_contract123"

// Deliberate whitespace and an opaque fixture field make byte preservation
// observable independently of the SessionSource's ID/format parsing.
const sessionJSON = `{
  "info": {"id":"ses_contract123", "title":"synthetic conformance history"},
  "messages": [{"info":{"id":"msg_contract1","role":"user"},"parts":[{"type":"text","text":"original synthetic native history"}]}],
  "fixture_opaque": {"preserve":"original bytes", "revision":1}
}
`

// No arbitrary environment switch selects the fixture's behavior: RunCLI
// intentionally drops such variables. The executable reads only its own
// adjacent input files and the verified import path supplied by the adapter.
const executable = `#!/bin/sh
set -eu
case "$0" in /*) ;; *) exit 80 ;; esac
for value in "${AGENTMOOR_TOKEN:-}" "${AGENTMOOR_TOKEN_FILE:-}" "${AGENTMOOR_POSTGRES_DSN:-}" "${AWS_ACCESS_KEY_ID:-}" "${AWS_SECRET_ACCESS_KEY:-}" "${OPENAI_API_KEY:-}" "${KUBECONFIG:-}"; do
  [ -z "$value" ] || exit 81
done
if [ "$#" -eq 1 ] && [ "$1" = "--version" ]; then
  printf 'version\n' >> "$0.calls"
  /bin/cat "$0.version"
  exit 0
fi
[ "$(pwd -P)" = "$(/bin/cat "$0.workspace")" ] || exit 82
if [ "$#" -eq 5 ] && [ "$1" = "session" ] && [ "$2" = "list" ] && [ "$3" = "--format" ] && [ "$4" = "json" ] && [ "$5" = "--pure" ]; then
  printf 'list\n' >> "$0.calls"
  /bin/cat "$0.list"
elif [ "$#" -eq 3 ] && [ "$1" = "export" ] && [ "$2" = "ses_contract123" ] && [ "$3" = "--pure" ]; then
  printf 'export\n' >> "$0.calls"
  /bin/cat "$0.payload"
elif [ "$#" -eq 3 ] && [ "$1" = "import" ] && [ "$3" = "--pure" ]; then
  case "$2" in /*) ;; *) exit 83 ;; esac
  case "${XDG_DATA_HOME:-}" in /*) ;; *) exit 84 ;; esac
  [ -f "$2" ] && [ -d "$XDG_DATA_HOME" ] || exit 85
  [ ! -e "$XDG_DATA_HOME/opencode-fixture" ] || exit 86
  printf 'import\n' >> "$0.calls"
  /bin/mkdir "$XDG_DATA_HOME/opencode-fixture"
  /bin/cat "$2" > "$XDG_DATA_HOME/opencode-fixture/session.json"
else
  # In particular, this fixture does not implement task execution with run.
  exit 87
fi
`

func writeNew(t *testing.T, name string, raw []byte, mode os.FileMode) {
	t.Helper()
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		t.Fatal("could not create private OpenCode fixture file", err)
	}
	_, writeErr := f.Write(raw)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("could not write private OpenCode fixture file", writeErr, closeErr)
	}
}

func readRegular(t *testing.T, name string, maximum int64) []byte {
	t.Helper()
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximum {
		t.Fatal("invalid bounded OpenCode fixture file", err)
	}
	raw, err := os.ReadFile(name)
	if err != nil || int64(len(raw)) > maximum {
		t.Fatal("could not read bounded OpenCode fixture file", err)
	}
	return raw
}

// New declares only the data and observation callbacks specific to OpenCode.
// The shared runner owns the Worker, scoped HTTP service, operations and claims.
func New() contracttest.Case {
	return contracttest.Case{Name: "opencode-native-cli", Prepare: func(t *testing.T, paths contracttest.Paths) contracttest.Fixture {
		t.Helper()
		for _, dir := range []string{paths.Workspace, paths.Sessions, paths.Runtime} {
			info, err := os.Stat(dir)
			if !filepath.IsAbs(dir) || err != nil || !info.IsDir() {
				t.Fatal("OpenCode case requires assigned absolute temporary directories", err)
			}
		}
		if err := os.Mkdir(filepath.Join(paths.Workspace, ".opencode"), 0700); err != nil {
			t.Fatal(err)
		}
		foreign := filepath.Join(paths.Runtime, "foreign-workspace")
		if err := os.Mkdir(foreign, 0700); err != nil {
			t.Fatal(err)
		}
		workspace, err := filepath.EvalSymlinks(paths.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		list, err := json.Marshal([]map[string]string{{"id": "ses_foreign123", "directory": foreign}, {"id": sessionID, "directory": paths.Workspace}})
		if err != nil {
			t.Fatal(err)
		}
		a := opencode.New()
		exe := filepath.Join(paths.Runtime, "synthetic-opencode")
		writeNew(t, exe, []byte(executable), 0700)
		writeNew(t, exe+".version", []byte(a.Descriptor().TestedRuntime+"\n"), 0600)
		writeNew(t, exe+".workspace", []byte(workspace+"\n"), 0600)
		writeNew(t, exe+".payload", []byte(sessionJSON), 0600)
		writeNew(t, exe+".list", append(list, '\n'), 0600)
		writeNew(t, exe+".calls", nil, 0600)
		return contracttest.Fixture{Adapter: a, Mechanism: contracttest.NativeCLI,
			SessionID: sessionID, SessionFormat: "opencode-export-json.v1",
			SkillManifest: ".opencode/skills/greet/SKILL.md", SessionFiles: map[string][]byte{"": []byte(sessionJSON)},
			Native: &contracttest.NativeFixture{Executable: exe,
				Calls: func(t *testing.T) contracttest.Calls {
					t.Helper()
					var calls contracttest.Calls
					for _, line := range strings.Split(string(readRegular(t, exe+".calls", 64<<10)), "\n") {
						switch line {
						case "":
						case "version":
							calls.Version++
						case "list":
							calls.List++
						case "export":
							calls.Export++
						case "import":
							calls.Import++
						default:
							t.Fatal("unrecognized synthetic OpenCode call record")
						}
					}
					return calls
				},
				SetVersion: func(t *testing.T, version string) {
					t.Helper()
					if version == "" || len(version) > 128 || strings.ContainsAny(version, "\x00\r\n") {
						t.Fatal("invalid bounded fixture version output")
					}
					_ = readRegular(t, exe+".version", 256)
					if err := os.WriteFile(exe+".version", []byte(version+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				},
				AssertImported: func(t *testing.T, dest string) {
					t.Helper()
					// This is the fixture's synthetic state, not OpenCode's vendor DB.
					raw := readRegular(t, filepath.Join(dest, "opencode-fixture", "session.json"), 4<<10)
					var imported struct{ Info struct{ ID string } }
					if json.Unmarshal(raw, &imported) != nil || imported.Info.ID != sessionID || !bytes.Equal(raw, []byte(sessionJSON)) {
						t.Fatal("synthetic native import lost the original Session identity or content")
					}
				},
			},
		}
	}}
}
