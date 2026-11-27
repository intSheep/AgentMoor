package sdk_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const sdkModule = "github.com/intSheep/AgentMoor"
const consumerModule = "example.invalid/agentmoor-sdk-consumer"

func checkoutRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if raw, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && strings.HasPrefix(string(raw), "module "+sdkModule+"\n") {
			root, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			return root
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("canonical checkout was not found")
		}
		dir = parent
	}
}

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// Keep the consumer physically outside the checkout even when TMPDIR is set to
// a directory inside it. The current native SDK supports Linux and macOS.
func outsideModule(t *testing.T, checkout string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "agentmoor-sdk-consumer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error("external consumer cleanup failed", err)
		}
	})
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || pathWithin(checkout, resolved) {
		t.Fatal("consumer directory is not outside the checkout", err)
	}
	return resolved
}

func externalEnvironment() []string {
	var env []string
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "GOCACHE", "GOMODCACHE", "GOPATH", "CGO_ENABLED", "CC", "CXX", "SDKROOT", "DEVELOPER_DIR", "MACOSX_DEPLOYMENT_TARGET"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return append(env, "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GO111MODULE=on", "GOFLAGS=-mod=mod -buildvcs=false")
}

type boundedGoOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	cancel context.CancelFunc
}

func (b *boundedGoOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buffer.Len()+len(p) > 2<<20 {
		b.cancel()
		return 0, errors.New("external Go output exceeded its bound")
	}
	return b.buffer.Write(p)
}

func runExternalGo(ctx context.Context, t *testing.T, dir string, args ...string) ([]byte, error) {
	t.Helper()
	goexe, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	output := &boundedGoOutput{cancel: cancel}
	cmd := exec.CommandContext(commandCtx, goexe, args...)
	cmd.Dir, cmd.Env = dir, externalEnvironment()
	cmd.Stdout, cmd.Stderr = output, output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
	cmd.WaitDelay = 3 * time.Second
	err = cmd.Run()
	return output.buffer.Bytes(), err
}

func tailOutput(raw []byte) string {
	if len(raw) > 8192 {
		raw = raw[len(raw)-8192:]
	}
	return string(raw)
}

func writeConsumerFile(t *testing.T, name string, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// Only standard library imports and the two documented public SDK packages
// are permitted in the consumer source, including its optional task signatures.
func checkConsumerImports(t *testing.T, name string, raw []byte) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, raw, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		if path == sdkModule+"/sdk" || path == sdkModule+"/sdk/contracttest" {
			continue
		}
		first, _, _ := strings.Cut(path, "/")
		if strings.Contains(first, ".") || strings.Contains("/"+path+"/", "/internal/") || strings.HasPrefix(path, "agentmoor/") {
			t.Fatal("external consumer bypassed its public SDK imports", path)
		}
	}
}

func copyConsumer(t *testing.T, checkout, target string) {
	t.Helper()
	fixture := filepath.Join(checkout, "testdata", "sdkconsumer")
	files := 0
	if err := filepath.WalkDir(fixture, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		files++
		if files > 16 || !entry.Type().IsRegular() {
			return errors.New("unexpected external consumer fixture entry")
		}
		info, err := entry.Info()
		if err != nil || info.Size() > 1<<20 {
			return errors.New("external consumer fixture exceeds its bound")
		}
		relative, err := filepath.Rel(fixture, name)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if filepath.Ext(name) == ".go" {
			checkConsumerImports(t, relative, raw)
		} else if relative != "go.mod" {
			return errors.New("undocumented external consumer fixture file")
		}
		writeConsumerFile(t, filepath.Join(target, relative), raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mod, err := os.ReadFile(filepath.Join(target, "go.mod"))
	if err != nil || !strings.HasPrefix(string(mod), "module "+consumerModule+"\n") || strings.HasPrefix(consumerModule, sdkModule) {
		t.Fatal("consumer module must have an unrelated module path", err)
	}
	mod = append(mod, []byte("\nreplace "+sdkModule+" => "+strconv.Quote(checkout)+"\n")...)
	writeConsumerFile(t, filepath.Join(target, "go.mod"), mod)
	// Reuse the checkout's verified public dependency checksums. GOPROXY=off
	// requires cached modules; this test never downloads a toolchain or SDK tag.
	sums, err := os.ReadFile(filepath.Join(checkout, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	writeConsumerFile(t, filepath.Join(target, "go.sum"), sums)
}

func assertConsumerTestsRan(t *testing.T, raw []byte) {
	t.Helper()
	wanted := map[string]bool{"TestExternalRegistryAndInspect": false, "TestExternalNativeConformance": false,
		"TestExternalNativeConformance/external-native-custom-commands": false}
	packagePassed, sawCalls := false, false
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var event struct{ Action, Package, Test, Output string }
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal("external go test did not return structured test evidence")
		}
		if event.Package != consumerModule {
			t.Fatal("external test ran a different module", event.Package)
		}
		if event.Action == "fail" {
			t.Fatal("an external consumer test failed", event.Test)
		}
		if event.Action == "pass" {
			if _, ok := wanted[event.Test]; ok {
				wanted[event.Test] = true
			}
			packagePassed = packagePassed || event.Test == ""
		}
		if strings.Contains(event.Output, "ADAPTER_CONTRACT mechanism=native-cli") {
			sawCalls = strings.Contains(event.Output, "List:3 Export:3 Import:1")
			t.Log(strings.TrimSpace(event.Output))
		}
		if strings.Contains(event.Output, "EXTERNAL_SDK ") {
			t.Log(strings.TrimSpace(event.Output))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for name, passed := range wanted {
		if !passed {
			t.Fatal("external consumer test did not actually pass", name)
		}
	}
	if !packagePassed || !sawCalls {
		t.Fatal("external consumer lacked a package pass or observed native calls")
	}
}

func TestExternalSDKConsumer(t *testing.T) {
	checkout := checkoutRoot(t)
	consumer := outsideModule(t, checkout)
	copyConsumer(t, checkout, consumer)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	moduleJSON, err := runExternalGo(ctx, t, consumer, "list", "-m", "-json", sdkModule)
	if err != nil {
		t.Fatalf("external module could not resolve the local SDK: %v\n%s", err, tailOutput(moduleJSON))
	}
	var module struct {
		Path, Version string
		Replace       *struct{ Path, Dir string }
	}
	if json.Unmarshal(moduleJSON, &module) != nil || module.Path != sdkModule || module.Version != "v0.0.0" || module.Replace == nil {
		t.Fatal("external SDK did not use its explicit local replacement")
	}
	resolved, err := filepath.EvalSymlinks(module.Replace.Dir)
	if err != nil || resolved != checkout {
		t.Fatal("external module resolved a different checkout", err)
	}
	deps, err := runExternalGo(ctx, t, consumer, "list", "-deps", "-f", "{{.ImportPath}}", sdkModule+"/sdk")
	if err != nil {
		t.Fatalf("could not inspect public SDK dependencies: %v\n%s", err, tailOutput(deps))
	}
	foundSDK := false
	for _, path := range strings.Fields(string(deps)) {
		foundSDK = foundSDK || path == sdkModule+"/sdk"
		for _, forbidden := range []string{sdkModule + "/internal/worker", sdkModule + "/internal/platform", sdkModule + "/sdk/contracttest", "database/sql", "github.com/lib/pq", "github.com/mattn/go-sqlite3", "github.com/minio/"} {
			if path == forbidden || strings.HasPrefix(path, forbidden+"/") || strings.HasSuffix(forbidden, "/") && strings.HasPrefix(path, forbidden) {
				t.Fatal("public core SDK acquired a Worker, SQL or S3 dependency", path)
			}
		}
	}
	if !foundSDK {
		t.Fatal("core dependency inspection did not include the SDK")
	}
	output, err := runExternalGo(ctx, t, consumer, "test", "-json", "-race", "-count=1", "-timeout=75s", "./...")
	if err != nil {
		t.Fatalf("independent SDK consumer failed: %v\n%s", err, tailOutput(output))
	}
	assertConsumerTestsRan(t, output)
	forbidden := filepath.Join(consumer, "forbidden-internal")
	mod := fmt.Sprintf("module example.invalid/sdk-internal-denied\n\ngo 1.27.4\n\nrequire %s v0.0.0\n\nreplace %s => %s\n", sdkModule, sdkModule, strconv.Quote(checkout))
	writeConsumerFile(t, filepath.Join(forbidden, "go.mod"), []byte(mod))
	writeConsumerFile(t, filepath.Join(forbidden, "denied_test.go"), []byte("package denied\nimport _ \""+sdkModule+"/internal/adapter\"\n"))
	sums, err := os.ReadFile(filepath.Join(consumer, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	writeConsumerFile(t, filepath.Join(forbidden, "go.sum"), sums)
	denial, err := runExternalGo(ctx, t, forbidden, "test", "-run=^$", ".")
	if err == nil || !bytes.Contains(denial, []byte("use of internal package "+sdkModule+"/internal/adapter not allowed")) {
		t.Fatalf("unrelated consumer was not rejected specifically by the internal boundary: %v\n%s", err, tailOutput(denial))
	}
	t.Logf("EXTERNAL_SDK module=%s gowork=off proxy=off toolchain=local checkout_replace=true public_imports_only=true core_deps_clean=true internal_import_denied=true synthetic_consumer_only=true", consumerModule)
}
