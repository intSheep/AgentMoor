package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/adapters/workspace"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/platform"
	"github.com/intSheep/AgentMoor/internal/snapshot"
	"github.com/intSheep/AgentMoor/internal/task"
	"github.com/intSheep/AgentMoor/internal/worker"
)

// The fixture adapter runs the real CSV tool and returns a deliberately
// different final message. It has its own runtime identity and session format.
type reportFixture struct {
	workspace.Layout
	tool string
}

func (f reportFixture) ExecuteFile(ctx context.Context, env task.Environment, spec task.Spec, result, session string, restore task.RestoreHistory) (task.Execution, error) {
	history := filepath.Join(env.Home, "fixture-history")
	idFile := filepath.Join(history, "session")
	west := 31
	if restore == nil {
		session = uuid.NewString()
		if err := os.Mkdir(history, 0700); err != nil {
			return task.Execution{}, err
		}
		if err := os.WriteFile(idFile, []byte(session), 0600); err != nil {
			return task.Execution{}, err
		}
	} else {
		if err := restore.Files(history); err != nil {
			return task.Execution{}, err
		}
		id, err := os.ReadFile(idFile)
		if err != nil || string(id) != session {
			return task.Execution{}, fmt.Errorf("fixture session did not restore")
		}
		// The second worker must read the report and CSV in its restored workspace.
		root, err := os.OpenRoot(env.Workspace)
		if err != nil {
			return task.Execution{}, err
		}
		defer root.Close()
		old, err := root.ReadFile(filepath.Join("out", spec.Output))
		if err != nil || string(old) != "{\"totals_cents\":{\"east\":98,\"west\":31}}\n" {
			return task.Execution{}, fmt.Errorf("prior report missing or changed")
		}
		csv, err := root.OpenFile("input.txt", os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return task.Execution{}, err
		}
		_, err = csv.WriteString("west,-9\n")
		closeErr := csv.Close()
		if err != nil {
			return task.Execution{}, err
		}
		if closeErr != nil {
			return task.Execution{}, closeErr
		}
		west = 22
	}
	cmd := exec.CommandContext(ctx, env.Executable, "-I", f.tool, "input.txt", result)
	cmd.Dir, cmd.Env = env.Workspace, []string{"LANG=C"}
	if data, err := cmd.CombinedOutput(); err != nil {
		return task.Execution{}, fmt.Errorf("CSV tool: %w: %s", err, data)
	}
	return task.Execution{Text: fmt.Sprintf(`{"totals_cents":{"east":99,"west":%d}}`, west), HistoryDir: history, SessionID: session}, nil
}

func fixtureSessions(_ context.Context, root *os.Root) ([]adapter.NativeSession, error) {
	id, err := root.ReadFile("session")
	if err != nil {
		return nil, err
	}
	return []adapter.NativeSession{{ID: string(id), Format: "fixture-session.v1", Entries: []snapshot.Entry{{Source: "session", Name: "session"}}}}, nil
}

// Called as a separate process with the normal run-task flags and a registry
// containing the test-only adapter. A production binary never registers it.
func TestReportFixtureWorker(t *testing.T) {
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		t.Skip("child process helper")
	}
	version, err := exec.CommandContext(t.Context(), "python3", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	examples, err := filepath.Abs("../../examples")
	if err != nil {
		t.Fatal(err)
	}
	f := reportFixture{Layout: workspace.Layout{ID: "local-report-fixture", Version: "1", TestedRuntime: strings.TrimSpace(string(version)), ReadSessions: fixtureSessions}, tool: filepath.Join(examples, "file-report", "summarize.py")}
	registry, err := adapter.NewRegistry(f)
	if err != nil {
		t.Fatal(err)
	}
	if err = workerCommand(t.Context(), os.Args[separator+1:], io.Discard, registry); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerFileDelivery(t *testing.T) {
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	python, err := exec.LookPath("python3")
	check(err)
	examples, err := filepath.Abs("../../examples")
	check(err)
	scope := asset.Scope{Tenant: "local", Project: "reports", Runtime: "file-report", Adapter: "local-report-fixture"}
	key := make([]byte, 32)
	_, err = rand.Read(key)
	check(err)
	catalog, err := platform.OpenCatalog(ctx, t.TempDir(), "", platform.DiskObjects{Dir: t.TempDir()})
	check(err)
	defer catalog.Close()
	api := platform.API{Catalog: catalog, Secret: key, Temp: t.TempDir(), FileTaskAdapters: map[string]bool{scope.Adapter: true}, TurnEnabled: func(s asset.Scope) bool { return s == scope }}
	service := httptest.NewServer(api.Handler())
	defer service.Close()
	token := func(submit, execute bool) string {
		tok, err := platform.Issue(key, platform.Grant{Scope: scope, Read: true, Submit: submit, Execute: execute, Write: execute, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
		check(err)
		return tok
	}
	execute := token(false, true)
	read := token(false, false)
	saas, err := platform.NewClient(service.URL, token(true, false))
	check(err)
	route := "/v1/tasks/" + scope.Runtime
	var row task.Record
	spec := task.Spec{Mode: task.FileTurn, Instruction: "Produce the report", Input: "region,amount_cents\neast,105\nwest,31\neast,-7\n", Output: "report.json"}
	check(saas.JSON(ctx, "PUT", route, spec, &row))
	state := t.TempDir()
	dbPath := filepath.Join(state, "business.db")
	var session string
	var refs []task.Ref
	var reports []string
	for i, west := range []int{31, 22} {
		turn := ""
		recordRoute := route
		allocation := t.TempDir()
		cfg := filepath.Join(allocation, "worker.json")
		config := worker.Config{Scope: scope, Endpoint: service.URL, Workspace: filepath.Join(allocation, "work"), StateDir: filepath.Join(allocation, "state"), RuntimeCLI: python}
		data, err := json.Marshal(config)
		check(err)
		check(os.WriteFile(cfg, data, 0600))
		args := []string{"run-task", "--config", cfg}
		if i > 0 {
			turn = uuid.NewString()
			recordRoute += "/turns/" + turn
			check(saas.JSON(ctx, "PUT", recordRoute, task.Question{SessionID: session, Message: "Apply west refund"}, &row))
			args = append(args, "--turn", turn)
		}
		child := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestReportFixtureWorker$", "--"}, args...)...)
		child.Env = []string{"PATH=" + os.Getenv("PATH"), "AGENTMOOR_TOKEN=" + execute}
		if out, err := child.CombinedOutput(); err != nil {
			t.Fatalf("worker: %v: %s", err, out)
		}
		// The SaaS consumer gets only the public API after the allocation is gone.
		check(os.RemoveAll(allocation))
		row = task.Record{}
		check(saas.JSON(ctx, "GET", recordRoute, nil, &row))
		if row.State != task.Succeeded {
			t.Fatalf("state %s", row.State)
		}
		var cp task.Checkpoint
		check(saas.JSON(ctx, "GET", route+"/checkpoint", nil, &cp))
		if i > 0 && (session != cp.SessionID || row.Parent == nil || row.Parent.SessionID != session) {
			t.Fatal("session changed")
		}
		session = cp.SessionID
		var ref task.Ref
		for _, item := range row.Assets {
			if item.Kind == "artifact" && item.ID == task.TurnKey(turn, asset.Key{Kind: "artifact", ID: "out/report.json"}).ID {
				ref = item
			}
		}
		want := fmt.Sprintf(`{"totals_cents":{"east":98,"west":%d}}`, west)
		wrong := fmt.Sprintf(`{"totals_cents":{"east":99,"west":%d}}`, west)
		if ref.Version != 1 || ref.ContentPath != ref.Key.ContentPath(1) || ref.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(want+"\n"))) {
			t.Fatalf("unexpected artifact %+v", ref)
		}
		refs = append(refs, ref)
		reports = append(reports, want)
		expected := filepath.Join(state, "expected.json")
		importArgs := []string{filepath.Join(examples, "report-consumer", "ingest.py"), "--endpoint", service.URL, "--scope", "local/reports", "--task", scope.Runtime, "--expected", expected, "--db", dbPath}
		if turn != "" {
			importArgs = append(importArgs, "--turn", turn)
		}
		runImport := func() ([]byte, error) {
			cmd := exec.CommandContext(ctx, python, importArgs...)
			cmd.Env = []string{"AGENTMOOR_READ_TOKEN=" + read}
			return cmd.CombinedOutput()
		}
		check(os.WriteFile(expected, []byte(wrong), 0600))
		if out, err := runImport(); err == nil || !strings.Contains(string(out), "independent expected totals") {
			t.Fatalf("final-text control: %q %v", out, err)
		}
		check(os.WriteFile(expected, []byte(want), 0600))
		if i > 0 {
			// A real business-database failure happens after verified HTTP download.
			// The Worker allocation is already gone; retrying this consumer can only
			// read the pinned receipt and never execute the Agent again.
			business, err := sql.Open("sqlite3", dbPath)
			check(err)
			_, err = business.Exec("CREATE TRIGGER reject_report BEFORE INSERT ON imported_reports BEGIN SELECT RAISE(ABORT, 'fixture database write failed'); END")
			check(err)
			out, err := runImport()
			if err == nil || !strings.Contains(string(out), "fixture database write failed") {
				t.Fatalf("database failure control: %q %v", out, err)
			}
			var count int
			check(business.QueryRow("SELECT count(*) FROM imported_reports").Scan(&count))
			if count != 1 {
				t.Fatalf("failed database write changed previous imports: %d", count)
			}
			_, err = business.Exec("DROP TRIGGER reject_report")
			check(err)
			check(business.Close())
		}
		for _, status := range []string{"imported", "already-imported"} {
			out, err := runImport()
			if err != nil {
				t.Fatalf("import: %v %s", err, out)
			}
			var got struct{ Status, SHA256 string }
			check(json.Unmarshal(out, &got))
			if got.Status != status || got.SHA256 != ref.SHA256 {
				t.Fatalf("unexpected import: %s", out)
			}
		}
		t.Logf("fixture_session=%s turn=%q path=%s sha256=%s file=%s final=%s", session, turn, ref.ContentPath, ref.SHA256, want, wrong)
	}
	db, err := sql.Open("sqlite3", dbPath)
	check(err)
	defer db.Close()
	var rows int
	check(db.QueryRow("SELECT count(*) FROM imported_reports").Scan(&rows))
	if rows != 2 {
		t.Fatalf("business rows=%d", rows)
	}
	for i, ref := range refs {
		_, body, err := saas.Content(ctx, ref.Key, ref.Version)
		check(err)
		raw, err := io.ReadAll(body)
		check(err)
		check(body.Close())
		var stored string
		check(db.QueryRow("SELECT report_json FROM imported_reports WHERE artifact_id=? AND version=? AND sha256=?", ref.ID, ref.Version, ref.SHA256).Scan(&stored))
		if stored != reports[i] || string(raw) != stored+"\n" {
			t.Fatal("stored report or versioned download changed")
		}
	}
}
