package guesttransport

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"agentmoor/internal/nativeguest"
)

const (
	fixtureInputCap  = "sbx1.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	fixtureOutputCap = "sbx1.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA"
	fixtureModelCap  = "sbx1.CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCA"
)

var fixtureInput = []byte("synthetic transport fixture input\n")

func fixtureSHA(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func transportFixture(t *testing.T, handler http.HandlerFunc) (Config, string, *httptest.Server) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	configDir, caps, temporary := t.TempDir(), t.TempDir(), t.TempDir()
	for purpose, token := range map[string]string{"input": fixtureInputCap, "output": fixtureOutputCap, "model": fixtureModelCap} {
		writeFixture(t, filepath.Join(caps, purpose), []byte(token+"\n"))
	}
	ca := filepath.Join(configDir, "gateway-ca.pem")
	writeFixture(t, ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	return Config{Gateway: server.URL, CAFile: ca, Capabilities: caps,
		InputSHA256: fixtureSHA(fixtureInput), InputSize: int64(len(fixtureInput)),
		Deadline: time.Now().Add(10 * time.Second), MaxInputBytes: 4096, MaxOutputBytes: 4096}, temporary, server
}

func writeFixture(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func noTransferFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("private transfer directory was not cleaned: entries=%v error=%v", entries, err)
	}
}

func noCapabilityInError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, token := range []string{fixtureInputCap, fixtureOutputCap, fixtureModelCap} {
		if strings.Contains(err.Error(), token) {
			t.Fatal("transport error exposed a raw capability")
		}
	}
}

// This is an explicitly named test provider process, never a vendor CLI. Its
// append-only counter proves the transport did not restart an execution when a
// complete upload was accepted but its TCP acknowledgement was lost.
func TestTransportFixtureProcess(t *testing.T) {
	if os.Getenv("AGENTMOOR_TRANSPORT_FIXTURE") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 3 {
		os.Exit(2)
	}
	counter, err := os.OpenFile(args[2], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(3)
	}
	_, err = counter.WriteString("child execution\n")
	if errors.Join(err, counter.Close()) != nil {
		os.Exit(4)
	}
	if args[1] == "block" {
		_, _ = os.Stdout.WriteString("started\n")
		time.Sleep(time.Hour)
		os.Exit(5)
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(6)
	}
	_, _ = os.Stdout.Write(append([]byte("fixture-result:"), raw...))
	os.Exit(0)
}

func fixtureProcess(t *testing.T, counter string, executions *atomic.Int32) Execute {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context, input io.Reader, output io.Writer) error {
		executions.Add(1)
		raw, err := io.ReadAll(input)
		if err != nil || !bytes.Equal(raw, fixtureInput) {
			return fmt.Errorf("fixture received input before complete validation")
		}
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestTransportFixtureProcess$", "--", "result", counter)
		cmd.Env = []string{"AGENTMOOR_TRANSPORT_FIXTURE=1", "GORACE=atexit_sleep_ms=0"}
		cmd.Stdin, cmd.Stdout = bytes.NewReader(raw), output
		return cmd.Run()
	}
}

func childCount(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "child execution\n")
}

func TestRunLostUploadAcknowledgementReplaysOnlyFrozenResult(t *testing.T) {
	var gets, puts, callbacks atomic.Int32
	type receipt struct {
		body                      []byte
		auth, digest, size, media string
		length                    int64
	}
	var mu sync.Mutex
	var receipts []receipt
	config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.URL.Path != "/input" && r.URL.Path != "/output" {
			t.Error("transport used a non-fixed capability route")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /input":
			if r.Header.Get("Authorization") != "Bearer "+fixtureInputCap {
				t.Error("input route used another purpose's capability")
			}
			if gets.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write(fixtureInput)
		case "PUT /output":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			receipts = append(receipts, receipt{body, r.Header.Get("Authorization"), r.Header.Get("X-Agentmoor-SHA256"),
				r.Header.Get("X-Agentmoor-Size"), r.Header.Get("Content-Type"), r.ContentLength})
			mu.Unlock()
			if puts.Add(1) == 1 {
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close() // Complete server-side receipt, no HTTP response.
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Error("unexpected capability method or path")
			w.WriteHeader(http.StatusNotFound)
		}
	})
	counter := filepath.Join(t.TempDir(), "executions")
	if err := Run(context.Background(), config, temporary, fixtureProcess(t, counter, &callbacks)); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 2 || puts.Load() != 2 || callbacks.Load() != 1 || childCount(t, counter) != 1 {
		t.Fatalf("retry reran native execution or lost transfer: GET=%d PUT=%d callback=%d child=%d", gets.Load(), puts.Load(), callbacks.Load(), childCount(t, counter))
	}
	mu.Lock()
	defer mu.Unlock()
	expected := append([]byte("fixture-result:"), fixtureInput...)
	for _, receipt := range receipts {
		if !bytes.Equal(receipt.body, expected) || receipt.auth != "Bearer "+fixtureOutputCap ||
			receipt.digest != fixtureSHA(expected) || receipt.size != fmt.Sprint(len(expected)) ||
			receipt.length != int64(len(expected)) || receipt.media != "application/x-tar" {
			t.Fatalf("retry changed frozen output or metadata: body=%q length=%d", receipt.body, receipt.length)
		}
	}
	noTransferFiles(t, temporary)
}

func TestRunWaitsForCompleteInputBeforeStartingProvider(t *testing.T) {
	prefixSent, release, callback := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	var puts atomic.Int32
	config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/output" {
			puts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write(fixtureInput[:10])
		w.(http.Flusher).Flush()
		close(prefixSent)
		select {
		case <-release:
			_, _ = w.Write(fixtureInput[10:])
		case <-r.Context().Done():
		}
	})
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), config, temporary, func(_ context.Context, input io.Reader, output io.Writer) error {
			callback <- struct{}{}
			raw, err := io.ReadAll(input)
			if err != nil || !bytes.Equal(raw, fixtureInput) {
				return fmt.Errorf("provider received incomplete input")
			}
			_, err = output.Write([]byte("complete fixture result"))
			return err
		})
	}()
	<-prefixSent
	select {
	case <-callback:
		t.Error("provider started before the complete download")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if puts.Load() != 1 {
		t.Fatalf("output uploads = %d", puts.Load())
	}
	noTransferFiles(t, temporary)
}

func TestRunRejectsUnsealedInputsWithoutExecutionOrUpload(t *testing.T) {
	for _, test := range []struct {
		name         string
		body         []byte
		status       int
		largeHeaders bool
	}{
		{"wrong-sha", bytes.Repeat([]byte("x"), len(fixtureInput)), 200, false},
		{"short-content", fixtureInput[:len(fixtureInput)-1], 200, false},
		{"long-content", append(append([]byte{}, fixtureInput...), 'x'), 200, false},
		{"expired-capability", nil, http.StatusUnauthorized, false},
		{"wrong-purpose-capability", nil, http.StatusForbidden, false},
		{"permanent-missing", nil, http.StatusNotFound, false},
		{"response-headers-over-32k", fixtureInput, 200, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gets, puts, callbacks atomic.Int32
			config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PUT" {
					puts.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				gets.Add(1)
				if test.largeHeaders {
					w.Header().Set("X-Oversized-Fixture", strings.Repeat("h", 33<<10))
				}
				w.WriteHeader(test.status)
				_, _ = w.Write(test.body)
			})
			err := Run(context.Background(), config, temporary, func(context.Context, io.Reader, io.Writer) error { callbacks.Add(1); return nil })
			if err == nil || callbacks.Load() != 0 || puts.Load() != 0 {
				t.Fatalf("unsealed input reached execution: err=%v callback=%d upload=%d", err, callbacks.Load(), puts.Load())
			}
			if !test.largeHeaders && gets.Load() != 1 {
				t.Fatalf("permanent input failure retried %d times", gets.Load())
			}
			if test.largeHeaders && gets.Load() != 3 {
				t.Fatalf("oversized response headers were not bounded across retries: %d", gets.Load())
			}
			noCapabilityInError(t, err)
			noTransferFiles(t, temporary)
		})
	}
}

func TestRunRejectsWrongCAAndRedirectsWithoutLeakingCapabilities(t *testing.T) {
	t.Run("wrong-ca", func(t *testing.T) {
		var requests, callbacks atomic.Int32
		config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
		// httptest servers share their default certificate; generate an actually
		// unrelated trust root rather than accidentally trusting the target.
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true,
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
		certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, config.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}))
		err = Run(context.Background(), config, temporary, func(context.Context, io.Reader, io.Writer) error { callbacks.Add(1); return nil })
		if err == nil || requests.Load() != 0 || callbacks.Load() != 0 {
			t.Fatalf("untrusted TLS server reached HTTP/execution: err=%v HTTP=%d callback=%d", err, requests.Load(), callbacks.Load())
		}
		noCapabilityInError(t, err)
		noTransferFiles(t, temporary)
	})
	t.Run("redirect", func(t *testing.T) {
		var targetRequests, sourceRequests, callbacks atomic.Int32
		target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetRequests.Add(1) }))
		defer target.Close()
		config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
			sourceRequests.Add(1)
			w.Header().Set("Location", target.URL+"/stolen")
			w.WriteHeader(http.StatusTemporaryRedirect)
		})
		err := Run(context.Background(), config, temporary, func(context.Context, io.Reader, io.Writer) error { callbacks.Add(1); return nil })
		if err == nil || sourceRequests.Load() != 1 || targetRequests.Load() != 0 || callbacks.Load() != 0 {
			t.Fatalf("redirect followed or executed: err=%v source=%d target=%d callback=%d", err, sourceRequests.Load(), targetRequests.Load(), callbacks.Load())
		}
		noCapabilityInError(t, err)
		noTransferFiles(t, temporary)
	})
}

func TestRunBoundsOutputBeforeAnyUploadEvenIfProviderIgnoresWriteError(t *testing.T) {
	for _, test := range []struct {
		name           string
		output         int
		executionError bool
	}{
		{"empty", 0, false}, {"one-over-limit", 33, false}, {"partial-then-error", 10, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var puts, callbacks atomic.Int32
			config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PUT" {
					puts.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				_, _ = w.Write(fixtureInput)
			})
			config.MaxOutputBytes = 32
			err := Run(context.Background(), config, temporary, func(_ context.Context, _ io.Reader, output io.Writer) error {
				callbacks.Add(1)
				_, _ = output.Write(bytes.Repeat([]byte("o"), test.output))
				if test.executionError {
					return errors.New("fixture execution interrupted")
				}
				return nil
			})
			if err == nil || callbacks.Load() != 1 || puts.Load() != 0 {
				t.Fatalf("incomplete/oversize output uploaded: err=%v callback=%d PUT=%d", err, callbacks.Load(), puts.Load())
			}
			noTransferFiles(t, temporary)
		})
	}
}

func TestRunPermanentOutputFailureDoesNotRerunProvider(t *testing.T) {
	var puts, callbacks atomic.Int32
	config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = w.Write(fixtureInput)
			return
		}
		puts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusForbidden)
	})
	counter := filepath.Join(t.TempDir(), "executions")
	err := Run(context.Background(), config, temporary, fixtureProcess(t, counter, &callbacks))
	if err == nil || puts.Load() != 1 || callbacks.Load() != 1 || childCount(t, counter) != 1 {
		t.Fatalf("permanent output failure retried execution: err=%v PUT=%d callback=%d child=%d", err, puts.Load(), callbacks.Load(), childCount(t, counter))
	}
	noCapabilityInError(t, err)
	noTransferFiles(t, temporary)
}

func TestRunRejectsExpiredOrCancelledBeforeNetworkAndExecution(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint("expired=", expired), func(t *testing.T) {
			var requests, callbacks atomic.Int32
			config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if expired {
				config.Deadline = time.Now().Add(-time.Second)
			} else {
				cancel()
			}
			err := Run(ctx, config, temporary, func(context.Context, io.Reader, io.Writer) error { callbacks.Add(1); return nil })
			if err == nil || requests.Load() != 0 || callbacks.Load() != 0 {
				t.Fatalf("expired/cancelled request reached network or execution: err=%v HTTP=%d callback=%d", err, requests.Load(), callbacks.Load())
			}
			noTransferFiles(t, temporary)
		})
	}
}

func TestRunCancellationStopsActualProviderAndNeverUploadsPartialResult(t *testing.T) {
	var puts, callbacks atomic.Int32
	config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			puts.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write(fixtureInput)
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	counter, started := filepath.Join(t.TempDir(), "executions"), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, config, temporary, func(runCtx context.Context, _ io.Reader, output io.Writer) error {
			callbacks.Add(1)
			cmd := exec.CommandContext(runCtx, executable, "-test.run=^TestTransportFixtureProcess$", "--", "block", counter)
			cmd.Env = []string{"AGENTMOOR_TRANSPORT_FIXTURE=1", "GORACE=atexit_sleep_ms=0"}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				return err
			}
			if err = cmd.Start(); err != nil {
				return err
			}
			line := make([]byte, len("started\n"))
			_, err = io.ReadFull(stdout, line)
			if err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return err
			}
			_, _ = output.Write(line)
			close(started)
			return errors.Join(cmd.Wait(), runCtx.Err())
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture child did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation result = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop actual provider")
	}
	if callbacks.Load() != 1 || childCount(t, counter) != 1 || puts.Load() != 0 {
		t.Fatalf("cancel reran or uploaded partial provider result: callback=%d child=%d PUT=%d", callbacks.Load(), childCount(t, counter), puts.Load())
	}
	noTransferFiles(t, temporary)
}

func TestCapabilityFileBoundariesAndDownwardAPISymlinks(t *testing.T) {
	for _, purpose := range []string{"../input", "input/../output", "", "INPUT", "/input", "input?model"} {
		if _, err := CapabilityFile(t.TempDir(), purpose); err == nil {
			t.Fatalf("accepted purpose %q", purpose)
		}
	}
	if _, err := CapabilityFile("relative", "input"); err == nil {
		t.Fatal("accepted relative capability directory")
	}
	for _, value := range []string{"", "sbx1.short", fixtureInputCap + "x", "sbx1." + strings.Repeat("a", 42) + "/", fixtureInputCap + "\n" + fixtureOutputCap, strings.Repeat("x", 1025)} {
		directory := t.TempDir()
		writeFixture(t, filepath.Join(directory, "input"), []byte(value))
		if _, err := CapabilityFile(directory, "input"); err == nil {
			t.Fatalf("accepted malformed capability of %d bytes", len(value))
		}
	}
	t.Run("projected-symlink", func(t *testing.T) {
		directory := t.TempDir()
		generation := filepath.Join(directory, "..generation")
		if err := os.Mkdir(generation, 0700); err != nil {
			t.Fatal(err)
		}
		for purpose, token := range map[string]string{"input": fixtureInputCap, "output": fixtureOutputCap, "model": fixtureModelCap} {
			writeFixture(t, filepath.Join(generation, purpose), []byte(token+"\n"))
			if err := os.Symlink("..data/"+purpose, filepath.Join(directory, purpose)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("..generation", filepath.Join(directory, "..data")); err != nil {
			t.Fatal(err)
		}
		for purpose, expected := range map[string]string{"input": fixtureInputCap, "output": fixtureOutputCap, "model": fixtureModelCap} {
			actual, err := CapabilityFile(directory, purpose)
			if err != nil || actual != expected {
				t.Fatalf("projected %s unavailable: %v", purpose, err)
			}
		}
	})
	t.Run("fifo-does-not-block", func(t *testing.T) {
		directory := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(directory, "input"), 0600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := CapabilityFile(directory, "input"); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("accepted FIFO")
			}
		case <-time.After(time.Second):
			t.Fatal("capability FIFO blocked")
		}
	})
	t.Run("directory", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.Mkdir(filepath.Join(directory, "input"), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := CapabilityFile(directory, "input"); err == nil {
			t.Fatal("accepted directory capability")
		}
	})
}

func TestWrongPurposeFileCannotUseOutputCapabilityForInput(t *testing.T) {
	var requests, callbacks atomic.Int32
	config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" || r.URL.Path != "/input" || r.Header.Get("Authorization") != "Bearer "+fixtureOutputCap {
			t.Error("unexpected purpose-confusion request")
		}
		w.WriteHeader(http.StatusForbidden)
	})
	writeFixture(t, filepath.Join(config.Capabilities, "input"), []byte(fixtureOutputCap))
	err := Run(context.Background(), config, temporary, func(context.Context, io.Reader, io.Writer) error { callbacks.Add(1); return nil })
	if err == nil || requests.Load() != 1 || callbacks.Load() != 0 {
		t.Fatalf("wrong purpose authorized execution: err=%v HTTP=%d callback=%d", err, requests.Load(), callbacks.Load())
	}
	noCapabilityInError(t, err)
	noTransferFiles(t, temporary)
}

func TestConfigRejectsMutableDestinationsAndUnboundedInputs(t *testing.T) {
	base := Config{Gateway: "https://gateway.example:8443", CAFile: "/image/ca.pem", Capabilities: "/run/bootstrap", InputSHA256: fixtureSHA(fixtureInput), InputSize: int64(len(fixtureInput)), Deadline: time.Now().Add(time.Minute), MaxInputBytes: 4096, MaxOutputBytes: 4096}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"http://gateway.example", "https://gateway.example/", "https://gateway.example/v1", "https://gateway.example?", "https://gateway.example?key=value", "https://gateway.example#fragment", "https://u:p@gateway.example", "https:///missing", "https://gateway.example/%2finput"} {
		config := base
		config.Gateway = origin
		if err := config.Validate(); err == nil {
			t.Fatalf("accepted non-origin gateway %q", origin)
		}
	}
	for name, mutate := range map[string]func(*Config){
		"relative-ca": func(c *Config) { c.CAFile = "ca.pem" }, "relative-caps": func(c *Config) { c.Capabilities = "bootstrap" },
		"zero-input": func(c *Config) { c.InputSize = 0 }, "input-over-limit": func(c *Config) { c.InputSize = c.MaxInputBytes + 1 },
		"unbounded-input": func(c *Config) { c.MaxInputBytes = nativeguest.MaxBundleBytes + 1 }, "unbounded-output": func(c *Config) { c.MaxOutputBytes = nativeguest.MaxBundleBytes + 1 },
		"zero-output-limit": func(c *Config) { c.MaxOutputBytes = 0 }, "wrong-digest": func(c *Config) { c.InputSHA256 = "sha256:bad" },
		"uppercase-digest": func(c *Config) { c.InputSHA256 = strings.ToUpper(c.InputSHA256) }, "expired": func(c *Config) { c.Deadline = time.Now().Add(-time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("accepted invalid guest configuration")
			}
		})
	}
}

func TestRunRetriesInterruptedInputBeforeOneActualProviderExecution(t *testing.T) {
	var gets, puts, callbacks atomic.Int32
	config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			puts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if gets.Add(1) == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(fixtureInput)))
			_, _ = w.Write(fixtureInput[:10])
			w.(http.Flusher).Flush()
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		_, _ = w.Write(fixtureInput)
	})
	counter := filepath.Join(t.TempDir(), "executions")
	if err := Run(context.Background(), config, temporary, fixtureProcess(t, counter, &callbacks)); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 2 || puts.Load() != 1 || callbacks.Load() != 1 || childCount(t, counter) != 1 {
		t.Fatalf("interrupted input changed execution count: GET=%d PUT=%d callback=%d child=%d", gets.Load(), puts.Load(), callbacks.Load(), childCount(t, counter))
	}
	noTransferFiles(t, temporary)
}

func TestRunCancellationOrDeadlineStopsPartialDownloadWithoutExecution(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint("deadline=", deadline), func(t *testing.T) {
			var callbacks, puts atomic.Int32
			started := make(chan struct{})
			config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PUT" {
					puts.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(fixtureInput)))
				_, _ = w.Write(fixtureInput[:5])
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if deadline {
				config.Deadline = time.Now().Add(150 * time.Millisecond)
			}
			done := make(chan error, 1)
			go func() {
				done <- Run(ctx, config, temporary, func(context.Context, io.Reader, io.Writer) error { callbacks.Add(1); return nil })
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("input server was not reached")
			}
			if !deadline {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("partial input accepted after cancellation/deadline")
				}
				noCapabilityInError(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("partial download ignored cancellation/deadline")
			}
			if callbacks.Load() != 0 || puts.Load() != 0 {
				t.Fatalf("partial input executed/uploaded: callback=%d PUT=%d", callbacks.Load(), puts.Load())
			}
			noTransferFiles(t, temporary)
		})
	}
}

func TestRunRetriesTransientOutputWithoutRerunningProvider(t *testing.T) {
	var puts, callbacks atomic.Int32
	config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = w.Write(fixtureInput)
			return
		}
		puts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	counter := filepath.Join(t.TempDir(), "executions")
	err := Run(context.Background(), config, temporary, fixtureProcess(t, counter, &callbacks))
	if err == nil || puts.Load() != 3 || callbacks.Load() != 1 || childCount(t, counter) != 1 {
		t.Fatalf("transient output changed execution count: err=%v PUT=%d callback=%d child=%d", err, puts.Load(), callbacks.Load(), childCount(t, counter))
	}
	noTransferFiles(t, temporary)
}

func TestRunAcceptsExactOutputLimitAndPrivateSpoolPermissions(t *testing.T) {
	var puts atomic.Int32
	config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = w.Write(fixtureInput)
			return
		}
		puts.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != 32 || r.Header.Get("X-Agentmoor-SHA256") != fixtureSHA(body) {
			t.Errorf("exact-limit output changed: size=%d err=%v", len(body), err)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	config.MaxOutputBytes = 32
	err := Run(context.Background(), config, temporary, func(_ context.Context, input io.Reader, output io.Writer) error {
		file, ok := input.(*os.File)
		if !ok {
			return fmt.Errorf("input was not frozen on disk")
		}
		info, err := file.Stat()
		if err != nil || info.Mode().Perm() != 0600 {
			return fmt.Errorf("input spool is not private")
		}
		info, err = os.Stat(filepath.Dir(file.Name()))
		if err != nil || info.Mode().Perm() != 0700 {
			return fmt.Errorf("transfer directory is not private")
		}
		info, err = os.Stat(filepath.Join(filepath.Dir(file.Name()), "result.tar"))
		if err != nil || info.Mode().Perm() != 0600 {
			return fmt.Errorf("result spool is not private")
		}
		_, err = output.Write(bytes.Repeat([]byte("r"), 32))
		return err
	})
	if err != nil || puts.Load() != 1 {
		t.Fatalf("exact-limit result rejected: err=%v PUT=%d", err, puts.Load())
	}
	noTransferFiles(t, temporary)
}

func TestRunRejectsUnboundedOrNonregularCAWithoutNetwork(t *testing.T) {
	for _, kind := range []string{"malformed", "oversize", "fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			var requests, callbacks atomic.Int32
			config, temporary, _ := transportFixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
			switch kind {
			case "malformed":
				writeFixture(t, config.CAFile, []byte("not a PEM certificate"))
			case "oversize":
				writeFixture(t, config.CAFile, bytes.Repeat([]byte("p"), (1<<20)+1))
			case "fifo":
				if err := os.Remove(config.CAFile); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(config.CAFile, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				config.CAFile = t.TempDir()
			}
			done := make(chan error, 1)
			go func() {
				done <- Run(context.Background(), config, temporary, func(context.Context, io.Reader, io.Writer) error { callbacks.Add(1); return nil })
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("invalid CA configuration accepted")
				}
				noCapabilityInError(t, err)
			case <-time.After(time.Second):
				t.Fatal("invalid CA configuration blocked")
			}
			if requests.Load() != 0 || callbacks.Load() != 0 {
				t.Fatalf("invalid CA reached network/execution: HTTP=%d callback=%d", requests.Load(), callbacks.Load())
			}
			noTransferFiles(t, temporary)
		})
	}
}
