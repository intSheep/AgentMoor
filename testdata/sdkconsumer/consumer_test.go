package sdkconsumer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intSheep/AgentMoor/sdk"
	"github.com/intSheep/AgentMoor/sdk/contracttest"
)

const originalSession = "{\n  \"id\":\"external_session_42\", \"messages\":[\"original synthetic history\"],\n  \"opaque\":{\"preserve\":true}\n}\n"

const nativeProgram = `#!/bin/sh
set -eu
for value in "${AGENTMOOR_TOKEN:-}" "${AGENTMOOR_TOKEN_FILE:-}" "${AGENTMOOR_POSTGRES_DSN:-}" "${AWS_ACCESS_KEY_ID:-}" "${AWS_SECRET_ACCESS_KEY:-}" "${OPENAI_API_KEY:-}" "${KUBECONFIG:-}"; do
  [ -z "$value" ] || exit 80
done
if [ "$#" -eq 1 ] && [ "$1" = "--version" ]; then
  printf 'version\n' >> "$0.calls"
  /bin/cat "$0.version"
  exit 0
fi
[ "$(pwd -P)" = "$(/bin/cat "$0.workspace")" ] || exit 81
if [ "$#" -eq 1 ] && [ "$1" = "list" ]; then
  printf 'list\n' >> "$0.calls"
  /bin/cat "$0.list"
elif [ "$#" -eq 2 ] && [ "$1" = "dump" ] && [ "$2" = "external_session_42" ]; then
  printf 'dump\n' >> "$0.calls"
  /bin/cat "$0.payload"
elif [ "$#" -eq 2 ] && [ "$1" = "load" ]; then
  case "$2" in /*) ;; *) exit 82 ;; esac
  case "${XDG_DATA_HOME:-}" in /*) ;; *) exit 83 ;; esac
  [ -f "$2" ] && [ -d "$XDG_DATA_HOME" ] || exit 84
  [ ! -e "$XDG_DATA_HOME/consumer-state" ] || exit 85
  printf 'load\n' >> "$0.calls"
  /bin/mkdir "$XDG_DATA_HOME/consumer-state"
  /bin/cat "$2" > "$XDG_DATA_HOME/consumer-state/session.json"
else
  exit 86
fi
`

func put(t *testing.T, name string, raw []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, raw, mode); err != nil {
		t.Fatal(err)
	}
}

func bounded(t *testing.T, name string) []byte {
	t.Helper()
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		t.Fatal("invalid bounded external fixture file", err)
	}
	raw, err := os.ReadFile(name)
	if err != nil || len(raw) > 64<<10 {
		t.Fatal("could not read external fixture file", err)
	}
	return raw
}

func prepare(t *testing.T, paths contracttest.Paths) contracttest.Fixture {
	t.Helper()
	put(t, filepath.Join(paths.Workspace, ".consumer/marker"), []byte("synthetic external SDK case\n"), 0600)
	foreign := filepath.Join(paths.Runtime, "other-workspace")
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(paths.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	list, err := json.Marshal([]map[string]string{{"id": "foreign_session", "directory": foreign}, {"id": sessionID, "directory": paths.Workspace}})
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(paths.Runtime, "consumer-cli")
	put(t, exe, []byte(nativeProgram), 0700)
	put(t, exe+".calls", nil, 0600)
	put(t, exe+".version", []byte(testedVersion+"\n"), 0600)
	put(t, exe+".workspace", []byte(workspace+"\n"), 0600)
	put(t, exe+".list", list, 0600)
	put(t, exe+".payload", []byte(originalSession), 0600)
	return contracttest.Fixture{Adapter: consumerAdapter{}, Mechanism: contracttest.NativeCLI,
		SessionID: sessionID, SessionFormat: sessionFormat, SkillManifest: skillManifest,
		SessionFiles: map[string][]byte{"": []byte(originalSession)}, Native: &contracttest.NativeFixture{
			Executable: exe,
			Calls: func(t *testing.T) contracttest.Calls {
				t.Helper()
				var counts contracttest.Calls
				for _, line := range strings.Split(string(bounded(t, exe+".calls")), "\n") {
					switch line {
					case "":
					case "version":
						counts.Version++
					case "list":
						counts.List++
					case "dump":
						counts.Export++
					case "load":
						counts.Import++
					default:
						t.Fatal("unexpected external fixture call record")
					}
				}
				return counts
			},
			SetVersion: func(t *testing.T, version string) {
				t.Helper()
				if version == "" || len(version) > 128 || strings.ContainsAny(version, "\x00\r\n") {
					t.Fatal("invalid bounded fixture version")
				}
				put(t, exe+".version", []byte(version+"\n"), 0600)
			},
			AssertImported: func(t *testing.T, dest string) {
				t.Helper()
				raw := bounded(t, filepath.Join(dest, "consumer-state/session.json"))
				var imported struct{ ID string }
				if json.Unmarshal(raw, &imported) != nil || imported.ID != sessionID || !bytes.Equal(raw, []byte(originalSession)) {
					t.Fatal("synthetic native state lost its original identity or content")
				}
			},
		}}
}

func TestExternalRegistryAndInspect(t *testing.T) {
	paths := contracttest.Paths{Workspace: t.TempDir(), Sessions: t.TempDir(), Runtime: t.TempDir()}
	fixture := prepare(t, paths)
	registry, err := sdk.NewRegistry(fixture.Adapter)
	if err != nil {
		t.Fatal(err)
	}
	a, err := registry.Lookup(adapterID)
	if err != nil || a.Descriptor().ID != adapterID || len(registry.All()) != 1 {
		t.Fatal("external Adapter did not register or resolve", err)
	}
	if _, err := sdk.NewRegistry(a, a); err == nil {
		t.Fatal("duplicate external Adapter registration was accepted")
	}
	if _, err := registry.Lookup("not-registered"); err == nil {
		t.Fatal("unknown external Adapter was accepted")
	}
	// These public names must be sufficient to construct every task parameter.
	_ = sdk.Environment{Skills: []sdk.LoadedSkill{{Name: "compile-only", Manifest: "owned/path"}}}
	_ = sdk.Spec{Mode: sdk.TextTurn, Instruction: "compile-only", Output: "reply.txt"}
	root, err := os.OpenRoot(paths.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := sdk.Inspect(t.Context(), a, root, fixture.Native.Executable)
	if err != nil || !info.Tested || info.RuntimeVersion != testedVersion || len(info.Evidence) != 1 {
		t.Fatal("public SDK did not inspect the supported fixture", err)
	}
	before := fixture.Native.Calls(t)
	fixture.Native.SetVersion(t, "unsupported-external-fixture")
	info, err = sdk.Inspect(t.Context(), a, root, fixture.Native.Executable)
	if err != nil || info.Tested || info.RuntimeVersion != "unsupported-external-fixture" {
		t.Fatal("public SDK concealed the incompatible fixture version", err)
	}
	after := fixture.Native.Calls(t)
	if after.Version != before.Version+1 || after.List != before.List || after.Export != before.Export || after.Import != before.Import {
		t.Fatal("version observation invoked native history commands")
	}
	t.Log("EXTERNAL_SDK registry=true inspect_supported=true inspect_unsupported=true task_types_compile_only=true")
}

func TestExternalNativeConformance(t *testing.T) {
	contracttest.Run(t, contracttest.Case{Name: "external-native-custom-commands", Prepare: prepare})
	t.Log("EXTERNAL_SDK native_commands=list,dump,load synthetic_only=true")
}
