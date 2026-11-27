package nativeguest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentmoor/internal/adapter"
	"agentmoor/internal/snapshot"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

// This is an explicitly named test runtime with its own session format. Its
// real child process is not a stand-in for any vendor CLI or a real sandbox.
type executionFixture struct{}

func (executionFixture) Descriptor() adapter.Descriptor {
	return adapter.Descriptor{ID: "guest-fixture", Version: "1", TestedRuntime: "native-guest-fixture 1"}
}
func (executionFixture) Detect(context.Context, *os.Root) ([]string, error) { return nil, nil }
func (executionFixture) SkillDirectory(env task.Environment, name string) (string, error) {
	return filepath.Join(env.Home, "skills", name), nil
}

type fixtureSession struct {
	ID    string   `json:"id"`
	Turns []string `json:"turns"`
	Homes []string `json:"homes"`
}

func (executionFixture) Sessions(ctx context.Context, root *os.Root) ([]adapter.NativeSession, error) {
	raw, err := root.ReadFile("session.json")
	if err != nil {
		return nil, err
	}
	var native fixtureSession
	if err = json.Unmarshal(raw, &native); err != nil || !sessionID(native.ID) {
		return nil, fmt.Errorf("invalid fixture native session")
	}
	return []adapter.NativeSession{{ID: native.ID, Format: "guest-fixture-session.v1",
		Entries: []snapshot.Entry{{Source: "session.json", Name: "session.json"}}}}, nil
}

func (a executionFixture) ExecuteTask(ctx context.Context, env task.Environment, spec task.Spec) (task.Execution, error) {
	return a.run(ctx, env, spec.Instruction, "native-report", "", nil)
}
func (a executionFixture) ContinueTask(ctx context.Context, env task.Environment, id, message string, history task.RestoreHistory) (task.Execution, error) {
	return a.run(ctx, env, message, "native-report", id, history)
}
func (a executionFixture) ExecuteFile(ctx context.Context, env task.Environment, spec task.Spec, result, id string, history task.RestoreHistory) (task.Execution, error) {
	return a.run(ctx, env, spec.Instruction, result, id, history)
}
func (executionFixture) run(ctx context.Context, env task.Environment, message, result, id string, history task.RestoreHistory) (task.Execution, error) {
	dir := filepath.Join(env.Home, "history")
	native := fixtureSession{ID: id}
	if history != nil {
		if err := history.Files(dir); err != nil {
			return task.Execution{}, err
		}
		raw, err := os.ReadFile(filepath.Join(dir, "session.json"))
		if err != nil || json.Unmarshal(raw, &native) != nil || native.ID != id {
			return task.Execution{}, fmt.Errorf("fixture session did not restore")
		}
	} else {
		native.ID = uuid.NewString()
		if err := os.Mkdir(dir, 0700); err != nil {
			return task.Execution{}, err
		}
	}
	var skill string
	if len(env.Skills) != 0 {
		skill = filepath.Join(filepath.Dir(env.Skills[0].Manifest), "scripts", "rule.sh")
	}
	var output bytes.Buffer
	args := []string{result, skill}
	if message == "hold" {
		args = []string{"hold"}
	}
	if err := adapter.RunCLIInput(ctx, env.Executable, env.Workspace, []string{"HOME=" + env.Home}, strings.NewReader(message), &output, 64<<10, args...); err != nil {
		return task.Execution{}, err
	}
	native.Turns = append(native.Turns, message)
	native.Homes = append(native.Homes, env.Home)
	raw, _ := json.Marshal(native)
	if err := os.WriteFile(filepath.Join(dir, "session.json"), raw, 0600); err != nil {
		return task.Execution{}, err
	}
	return task.Execution{Text: output.String(), SessionID: native.ID, HistoryDir: dir}, nil
}

const executionFixtureProgram = `#!/bin/sh
if [ "$1" = --version ]; then
  printf 'version\n' >> "$0.calls"
  printf 'native-guest-fixture 1\n'
  exit 0
fi
if [ "$1" = import ]; then
  printf 'import\n' >> "$0.calls"
  /bin/cp "$2" "$3/session.json"
  exit $?
fi
[ -z "${AGENTMOOR_TOKEN:-}${AGENTMOOR_TOKEN_FILE:-}${AWS_SECRET_ACCESS_KEY:-}${OUTER_PRIVATE_SENTINEL:-}" ] || exit 93
printf 'run\n' >> "$0.calls"
if [ "$1" = hold ]; then
  printf 'started' > "$0.started"
  sleep 60 &
  wait "$!"
  exit 0
fi
if [ -n "$2" ]; then
  /bin/sh "$2" > "$1" || exit 94
else
  : > "$1"
fi
/bin/cat >> "$1"
printf 'done'
`

func executionEnvironment(t *testing.T) (executionFixture, string, Request) {
	t.Helper()
	for _, name := range []string{"AGENTMOOR_TOKEN", "AGENTMOOR_TOKEN_FILE", "AGENTMOOR_POSTGRES_DSN", "AGENTMOOR_S3_ENDPOINT", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		t.Setenv(name, "")
	}
	t.Setenv("OUTER_PRIVATE_SENTINEL", "not-for-the-native-child")
	exe := filepath.Join(t.TempDir(), "native-guest-fixture")
	if err := os.WriteFile(exe, []byte(executionFixtureProgram), 0700); err != nil {
		t.Fatal(err)
	}
	request := Request{Version: Protocol, AdapterID: "guest-fixture", AdapterVersion: "1", RuntimeVersion: "native-guest-fixture 1",
		Spec: task.Spec{Mode: task.FileTurn, Instruction: "first native report", Input: "private workspace input", Output: "report.txt"}}
	return executionFixture{}, exe, request
}

func requestBytes(t *testing.T, request Request, files map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := WriteRequest(t.Context(), &out, request, func(name string) (io.ReadCloser, error) {
		file, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("unexpected fixture slot")
		}
		return os.Open(file)
	}, DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func executeFixtureBundle(t *testing.T, a executionFixture, exe string, request Request, input []byte, virtualRoot string) (Result, string) {
	t.Helper()
	var output bytes.Buffer
	if err := Execute(t.Context(), a, exe, virtualRoot, bytes.NewReader(input), &output, DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(virtualRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("guest-only execution directory was not cleaned", err)
	}
	destination := filepath.Join(t.TempDir(), "received")
	result, err := ReadResult(t.Context(), bytes.NewReader(output.Bytes()), destination, request, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return result, destination
}

func restoredSession(t *testing.T, result Result, directory string) fixtureSession {
	t.Helper()
	target := filepath.Join(t.TempDir(), "native-history")
	if err := restoreBlob(t.Context(), filepath.Join(directory, "session.blob"), result.Session, target, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(target, "session.json"))
	var native fixtureSession
	if err != nil || json.Unmarshal(raw, &native) != nil {
		t.Fatal("original fixture session bytes did not restore", err)
	}
	return native
}

func TestGuestExecuteFileAndContinuationUseFreshHomesAndOriginalSession(t *testing.T) {
	a, exe, request := executionEnvironment(t)
	root := filepath.Join(t.TempDir(), "same-virtual-root")
	first, firstDir := executeFixtureBundle(t, a, exe, request, requestBytes(t, request, nil), root)
	initial := restoredSession(t, first, firstDir)
	if initial.ID != first.SessionID || !reflect.DeepEqual(initial.Turns, []string{request.Spec.Instruction}) {
		t.Fatal("first guest execution did not capture its own original session", initial)
	}
	work := filepath.Join(t.TempDir(), "work")
	if err := restoreBlob(t.Context(), filepath.Join(firstDir, "workspace.tar"), first.Workspace, work, true); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(work, "out", "report.txt"))
	if string(raw) != request.Spec.Instruction {
		t.Fatal("native child file was not delivered", string(raw))
	}
	request.ParentSessionID, request.Workspace, request.Session = first.SessionID, &first.Workspace, &first.Session
	request.Spec.Instruction = "revise the restored report"
	input := requestBytes(t, request, map[string]string{"workspace.tar": filepath.Join(firstDir, "workspace.tar"), "parent.session": filepath.Join(firstDir, "session.blob")})
	second, secondDir := executeFixtureBundle(t, a, exe, request, input, root)
	continued := restoredSession(t, second, secondDir)
	if second.SessionID != first.SessionID || !reflect.DeepEqual(continued.Turns, []string{"first native report", request.Spec.Instruction}) || len(continued.Homes) != 2 || continued.Homes[0] == continued.Homes[1] {
		t.Fatal("continuation did not retain native ID/history with a new private home", continued)
	}
	calls, _ := os.ReadFile(exe + ".calls")
	if string(calls) != "version\nrun\nversion\nrun\n" {
		t.Fatal("unexpected number of probes or intended native turns", string(calls))
	}
	if after := restoredSession(t, first, firstDir); !reflect.DeepEqual(after, initial) {
		t.Fatal("continuation rewrote the old immutable session", after)
	}
}

func TestGuestExecutePinnedSkillRunsItsOriginalScriptAndReference(t *testing.T) {
	a, exe, request := executionEnvironment(t)
	skill := t.TempDir()
	for name, value := range map[string]string{"SKILL.md": "---\nname: rule\n---\nUse the pinned rule.",
		"scripts/rule.sh": "#!/bin/sh\n/bin/cat \"$(dirname \"$0\")/../references/rule.txt\"\n", "references/rule.txt": "original-rule-v1\n"} {
		file := filepath.Join(skill, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(value), 0700); err != nil {
			t.Fatal(err)
		}
	}
	root, _ := os.OpenRoot(skill)
	defer root.Close()
	blob, err := snapshot.Tree(t.Context(), root, []snapshot.Entry{{Source: ".", Name: "."}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Remove()
	request.Skills = []Skill{{Name: "rule", Blob: blobMetadata(blob, "skill-tar.v1", "application/x-tar")}}
	packet := requestBytes(t, request, map[string]string{"skills/0.tar": blob.Path})
	if err := os.WriteFile(filepath.Join(skill, "references", "rule.txt"), []byte("unselected-source-v2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, directory := executeFixtureBundle(t, a, exe, request, packet, filepath.Join(t.TempDir(), "guest"))
	work := filepath.Join(t.TempDir(), "work")
	if err := restoreBlob(t.Context(), filepath.Join(directory, "workspace.tar"), result.Workspace, work, true); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(work, "out", request.Spec.Output))
	if string(raw) != "original-rule-v1\n"+request.Spec.Instruction {
		t.Fatal("native child did not execute the original restored script/reference", string(raw))
	}
}

func TestGuestExecuteRejectsBadInputAndAmbientCredentialsBeforeAnyNativeCommand(t *testing.T) {
	for _, condition := range []string{"truncated", "metadata-tamper", "ambient-platform-token", "existing-root"} {
		t.Run(condition, func(t *testing.T) {
			a, exe, request := executionEnvironment(t)
			packet := requestBytes(t, request, nil)
			root := filepath.Join(t.TempDir(), "guest")
			switch condition {
			case "truncated":
				packet = packet[:len(packet)-1]
			case "metadata-tamper":
				packet = bytes.Replace(packet, []byte(`"adapter_id"`), []byte(`"Adapter_ID"`), 1)
			case "ambient-platform-token":
				t.Setenv("AGENTMOOR_TOKEN", "synthetic-platform-secret")
			case "existing-root":
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "sentinel"), []byte("existing content"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			if err := Execute(t.Context(), a, exe, root, bytes.NewReader(packet), &output, DefaultLimits()); err == nil || output.Len() != 0 {
				t.Fatal("invalid request emitted successful result bytes", err, output.Len())
			}
			if _, err := os.Stat(exe + ".calls"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid handoff started a native command, including a version probe", err)
			}
			if condition == "existing-root" {
				raw, err := os.ReadFile(filepath.Join(root, "sentinel"))
				if err != nil || string(raw) != "existing content" {
					t.Fatal("existing guest target was modified", string(raw), err)
				}
			}
		})
	}
}

func TestGuestExecuteVersionMismatchNeverRunsTurnOrEmitsResult(t *testing.T) {
	a, exe, request := executionEnvironment(t)
	if err := os.WriteFile(exe, []byte(strings.Replace(executionFixtureProgram, "native-guest-fixture 1", "native-guest-fixture 2", 1)), 0700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Execute(t.Context(), a, exe, filepath.Join(t.TempDir(), "guest"), bytes.NewReader(requestBytes(t, request, nil)), &output, DefaultLimits()); err == nil || output.Len() != 0 {
		t.Fatal("wrong native version executed or emitted a result", err)
	}
	calls, _ := os.ReadFile(exe + ".calls")
	if string(calls) != "version\n" {
		t.Fatal("version mismatch must only run the bounded probe", string(calls))
	}
}

func TestGuestExecuteCancellationStopsNativeProcessAndEmitsNoResult(t *testing.T) {
	a, exe, request := executionEnvironment(t)
	request.Spec.Instruction = "hold"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var output bytes.Buffer
	root := filepath.Join(t.TempDir(), "guest")
	input := requestBytes(t, request, nil)
	done := make(chan error, 1)
	go func() {
		done <- Execute(ctx, a, exe, root, bytes.NewReader(input), &output, DefaultLimits())
	}()
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(exe + ".started"); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("real native fixture never reached its blocking step")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || output.Len() != 0 {
			t.Fatal("cancelled native turn emitted a result", err, output.Len())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled native process did not stop")
	}
}

type rawExecutionFixture struct{ executionFixture }

func (rawExecutionFixture) Sessions(ctx context.Context, root *os.Root) ([]adapter.NativeSession, error) {
	items, err := (executionFixture{}).Sessions(ctx, root)
	if err == nil {
		items[0].Raw = true
		items[0].Format = "guest-fixture-native-export.v1"
	}
	return items, err
}

func (rawExecutionFixture) ImportNative(ctx context.Context, exe, workspace, file, freshState string) error {
	return adapter.RunCLIInput(ctx, exe, workspace, []string{"HOME=" + freshState}, nil, io.Discard, 1<<20, "import", file, freshState)
}
func (rawExecutionFixture) ExportNative(context.Context, string, string, string) error {
	return fmt.Errorf("fixture execution already writes its raw original export")
}

type nativeDatabaseRestore struct{ task.RestoreHistory }

func (h nativeDatabaseRestore) Files(destination string) error {
	return h.NativeDatabase(destination)
}

func (a rawExecutionFixture) ExecuteFile(ctx context.Context, env task.Environment, spec task.Spec, result, id string, history task.RestoreHistory) (task.Execution, error) {
	if history != nil {
		history = nativeDatabaseRestore{history}
	}
	return a.executionFixture.ExecuteFile(ctx, env, spec, result, id, history)
}

func TestGuestExecuteRawNativeImportStaysInGuestAndPreservesOriginalBytes(t *testing.T) {
	_, exe, request := executionEnvironment(t)
	a := rawExecutionFixture{}
	var packet bytes.Buffer
	if err := Execute(t.Context(), a, exe, filepath.Join(t.TempDir(), "guest"), bytes.NewReader(requestBytes(t, request, nil)), &packet, DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	firstDir := filepath.Join(t.TempDir(), "received")
	first, err := ReadResult(t.Context(), bytes.NewReader(packet.Bytes()), firstDir, request, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if first.Session.MediaType != "application/json" || first.Session.Format != "guest-fixture-native-export.v1" {
		t.Fatal("raw native export was converted to a different format", first.Session)
	}
	original, err := os.ReadFile(filepath.Join(firstDir, "session.blob"))
	if err != nil {
		t.Fatal(err)
	}
	var native fixtureSession
	if err = json.Unmarshal(original, &native); err != nil || native.ID != first.SessionID {
		t.Fatal("original raw native bytes are missing", err)
	}
	request.ParentSessionID, request.Workspace, request.Session = first.SessionID, &first.Workspace, &first.Session
	request.Spec.Instruction = "continue from original native export"
	input := requestBytes(t, request, map[string]string{"workspace.tar": filepath.Join(firstDir, "workspace.tar"), "parent.session": filepath.Join(firstDir, "session.blob")})
	packet.Reset()
	if err = Execute(t.Context(), a, exe, filepath.Join(t.TempDir(), "guest"), bytes.NewReader(input), &packet, DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	secondDir := filepath.Join(t.TempDir(), "received")
	second, err := ReadResult(t.Context(), bytes.NewReader(packet.Bytes()), secondDir, request, DefaultLimits())
	if err != nil || second.SessionID != first.SessionID {
		t.Fatal("raw native import did not retain its ID", err)
	}
	raw, _ := os.ReadFile(filepath.Join(secondDir, "session.blob"))
	if json.Unmarshal(raw, &native) != nil || !reflect.DeepEqual(native.Turns, []string{"first native report", request.Spec.Instruction}) {
		t.Fatal("native import did not restore its original records", string(raw))
	}
	calls, _ := os.ReadFile(exe + ".calls")
	if string(calls) != "version\nrun\nversion\nimport\nrun\n" {
		t.Fatal("native import escaped the guest-owned lifecycle", string(calls))
	}
	if after, _ := os.ReadFile(filepath.Join(firstDir, "session.blob")); !bytes.Equal(after, original) {
		t.Fatal("native continuation rewrote old raw export bytes")
	}
}

func TestGuestExecuteTextTurnUsesExistingExecutor(t *testing.T) {
	a, exe, request := executionEnvironment(t)
	request.Spec.Mode = task.TextTurn
	result, directory := executeFixtureBundle(t, a, exe, request, requestBytes(t, request, nil), filepath.Join(t.TempDir(), "guest"))
	work := filepath.Join(t.TempDir(), "work")
	if err := restoreBlob(t.Context(), filepath.Join(directory, "workspace.tar"), result.Workspace, work, true); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(work, "out", request.Spec.Output))
	if string(raw) != "done" || result.Text != "done" {
		t.Fatal("existing text executor result did not reach the output file", string(raw), result.Text)
	}
}

type oversizedWorkspaceFixture struct {
	executionFixture
	sessionReads *atomic.Int64
}

func (a oversizedWorkspaceFixture) ExecuteFile(ctx context.Context, env task.Environment, spec task.Spec, result, id string, history task.RestoreHistory) (task.Execution, error) {
	execution, err := a.executionFixture.ExecuteFile(ctx, env, spec, result, id, history)
	if err != nil {
		return execution, err
	}
	err = os.WriteFile(filepath.Join(env.Workspace, "oversized"), bytes.Repeat([]byte{'x'}, 32<<10), 0600)
	return execution, err
}

func (a oversizedWorkspaceFixture) Sessions(ctx context.Context, root *os.Root) ([]adapter.NativeSession, error) {
	a.sessionReads.Add(1)
	return a.executionFixture.Sessions(ctx, root)
}

func TestGuestCaptureBudgetRejectsWorkspaceBeforeCapturingSession(t *testing.T) {
	_, exe, request := executionEnvironment(t)
	var sessionReads atomic.Int64
	a := oversizedWorkspaceFixture{sessionReads: &sessionReads}
	var output bytes.Buffer
	root := filepath.Join(t.TempDir(), "guest")
	if err := Execute(t.Context(), a, exe, root, bytes.NewReader(requestBytes(t, request, nil)), &output,
		Limits{MaxInputBytes: MaxBundleBytes, MaxOutputBytes: 8 << 10}); err == nil || output.Len() != 0 {
		t.Fatal("oversized captured workspace emitted a result", err, output.Len())
	}
	if sessionReads.Load() != 0 {
		t.Fatal("oversized workspace was captured before the output budget was enforced")
	}
	calls, _ := os.ReadFile(exe + ".calls")
	if string(calls) != "version\nrun\n" {
		t.Fatal("oversized output reran the intended native turn", string(calls))
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed bounded capture retained guest temporary files", err)
	}
}
