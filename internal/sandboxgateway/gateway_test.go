package sandboxgateway

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/guesttransport"
	"agentmoor/internal/platform"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

const syntheticProviderKey = "synthetic-provider-secret-for-gateway-tests"

var fixtureExecutionSHA = sha([]byte("fixed synthetic sandbox profile"))
var fixtureEnvelope = []byte("frozen synthetic input envelope")

func sha(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func blob(raw []byte) platform.SandboxBlob {
	return platform.SandboxBlob{SHA256: sha(raw), Size: int64(len(raw))}
}

// These object faults remain inside the synthetic fixture; they test that a
// bad backing object is checked through EOF and never starts the guest callback.
type faultObjects struct {
	platform.DiskObjects
	corrupt atomic.Bool
}

func (o *faultObjects) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if o.corrupt.Load() {
		return io.NopCloser(strings.NewReader(strings.Repeat("x", len(fixtureEnvelope)))), nil
	}
	return o.DiskObjects.Get(ctx, key)
}

type fixture struct {
	t                     *testing.T
	catalog               *platform.Catalog
	directory             string
	objects               *faultObjects
	scope                 asset.Scope
	permit                platform.SandboxCreatePermit
	profile               Profile
	gateway               *Gateway
	server, api, upstream *httptest.Server
	client                *platform.Client
	requests              atomic.Int32
	seenMu                sync.Mutex
	seen                  []map[string]any
}

func setup(t *testing.T, upstream http.HandlerFunc, deadline time.Duration) *fixture {
	t.Helper()
	f := &fixture{t: t, directory: t.TempDir(), objects: &faultObjects{DiskObjects: platform.DiskObjects{Dir: t.TempDir()}}, scope: asset.Scope{Tenant: "fixture-tenant", Project: "fixture-project", Runtime: "fixture-" + uuid.NewString(), Adapter: "codex"}}
	f.upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Method != "POST" || r.URL.RequestURI() != "/v1/responses" || r.Header.Get("Authorization") != "Bearer "+syntheticProviderKey {
			t.Error("upstream route or trusted credential changed")
		}
		if strings.Contains(r.Header.Get("Authorization"), "sbx1.") || r.Header.Get("OpenAI-Organization") != "" || r.Header.Get("X-Leak-Capability") != "" {
			t.Error("guest headers escaped into upstream request")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var m map[string]any
		if err = json.Unmarshal(raw, &m); err != nil {
			t.Error(err)
		}
		f.seenMu.Lock()
		f.seen = append(f.seen, m)
		f.seenMu.Unlock()
		if upstream != nil {
			upstream(w, r)
			return
		}
		completed(w)
	}))
	var err error
	f.catalog, err = platform.OpenCatalog(t.Context(), f.directory, "", f.objects)
	if err != nil {
		t.Fatal(err)
	}
	f.startAPI()
	var record task.Record
	if err = f.client.JSON(t.Context(), "PUT", "/v1/tasks/"+f.scope.Runtime, task.Spec{Instruction: "synthetic instruction", Input: "synthetic data", Output: "reply.txt"}, &record); err != nil {
		t.Fatal(err)
	}
	a := platform.SandboxAddress{Scope: f.scope, Attempt: uuid.NewString()}
	if err = f.client.JSON(t.Context(), "POST", "/v1/tasks/"+f.scope.Runtime+"/claim", task.Claim{Attempt: a.Attempt}, &record); err != nil {
		t.Fatal(err)
	}
	if deadline > 0 {
		// A short original task deadline is test data, not a production timeout override.
		record.Deadline = time.Now().UTC().Add(deadline)
		raw, _ := json.Marshal(record)
		db, err := sql.Open("sqlite3", filepath.Join(f.directory, "catalog.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec("UPDATE tasks SET record=$1 WHERE scope=$2", string(raw), f.scope.Prefix())
		_ = db.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	run, err := f.catalog.BeginSandbox(t.Context(), a, fixtureExecutionSHA, blob(fixtureEnvelope))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.catalog.SealSandboxInput(t.Context(), a, bytes.NewReader(fixtureEnvelope)); err != nil {
		t.Fatal(err)
	}
	f.permit, err = f.catalog.FenceSandboxCreate(t.Context(), a, run.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.catalog.BindSandboxPod(t.Context(), a, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	config := t.TempDir()
	key, ca := filepath.Join(config, "key"), filepath.Join(config, "ca")
	if err = os.WriteFile(key, []byte(syntheticProviderKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.upstream.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	f.profile = Profile{Scope: f.scope, ExecutionProfileSHA256: fixtureExecutionSHA, Model: "gpt-5", UpstreamURL: f.upstream.URL + "/v1/responses", APIKeyFile: key, CAFile: ca, MaxRequests: 8, MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, PerRequestOutputBytes: 64 << 10, ModelConcurrency: 2, MaxOutputTokens: 1024}
	f.startGateway(nil)
	t.Cleanup(func() { f.server.Close(); f.gateway.Close(); f.api.Close(); _ = f.catalog.Close(); f.upstream.Close() })
	return f
}
func (f *fixture) startAPI() {
	secret := bytes.Repeat([]byte{9}, 32)
	token, err := platform.Issue(secret, platform.Grant{Scope: f.scope, Read: true, Submit: true, Execute: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	f.api = httptest.NewServer((&platform.API{Catalog: f.catalog, Secret: secret, Temp: f.t.TempDir(), TaskAdapters: map[string]bool{"codex": true}}).Handler())
	f.client, err = platform.NewClient(f.api.URL, token)
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) startGateway(wrap func(http.Handler) http.Handler) {
	var err error
	f.gateway, err = New(f.catalog, &Config{Profiles: []Profile{f.profile}})
	if err != nil {
		f.t.Fatal(err)
	}
	f.server = httptest.NewUnstartedServer(f.gateway)
	f.server.Config = NewTLSServer("", f.gateway)
	if wrap != nil {
		f.server.Config.Handler = wrap(f.gateway)
	}
	f.server.StartTLS()
}
func (f *fixture) restartGateway(wrap func(http.Handler) http.Handler) {
	f.server.Close()
	f.gateway.Close()
	f.startGateway(wrap)
}
func completed(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
}
func (f *fixture) request(method, path, token string, raw []byte) (*http.Response, error) {
	r, err := http.NewRequest(method, f.server.URL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
	}
	if method == "PUT" {
		r.Header.Set("Content-Type", "application/x-tar")
		r.Header.Set("X-Agentmoor-SHA256", sha(raw))
		r.Header.Set("X-Agentmoor-Size", fmt.Sprint(len(raw)))
	}
	return f.server.Client().Do(r)
}
func closeResponse(t *testing.T, r *http.Response, err error, want int) []byte {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, e := io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	if r.StatusCode != want {
		t.Fatalf("HTTP=%d want=%d body=%q", r.StatusCode, want, body)
	}
	return body
}
func modelBody() []byte {
	return []byte(`{"model":"gpt-5","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic prompt with ordinary https://example.invalid text"}]}]}`)
}

func TestDedicatedGatewayWholeBlobAndSuccessful204(t *testing.T) {
	f := setup(t, nil, 0)
	r, err := f.request("GET", "/input", f.permit.Capabilities.Input, nil)
	body := closeResponse(t, r, err, 200)
	if !bytes.Equal(body, fixtureEnvelope) || r.Header.Get("X-Agentmoor-SHA256") != sha(body) {
		t.Fatal("whole sealed input changed")
	}
	output := []byte("synthetic frozen output")
	for range 2 {
		r, err = f.request("PUT", "/output", f.permit.Capabilities.Output, output)
		closeResponse(t, r, err, 204)
	}
	run, err := f.catalog.SandboxRun(t.Context(), f.permit.Run.Address())
	if err != nil || !run.Output.Sealed || run.Output.SHA256 != sha(output) {
		t.Fatal("output not sealed", run, err)
	}
	if _, err = f.catalog.AuthenticateSandboxCapability(t.Context(), f.permit.Capabilities.Model, platform.SandboxModel); !errors.Is(err, platform.ErrSandboxDenied) {
		t.Fatal("output seal retained model authority", err)
	}
}
func TestDedicatedGatewayRefusesOtherAPIsPurposeAndScope(t *testing.T) {
	f := setup(t, nil, 0)
	for _, tc := range []struct {
		method, path, token string
		status              int
	}{
		{"GET", "/v1/assets", f.permit.Capabilities.Input, 404}, {"GET", "/v1/tasks/fixture", f.permit.Capabilities.Input, 404}, {"POST", "/input", f.permit.Capabilities.Input, 404}, {"HEAD", "/input", f.permit.Capabilities.Input, 404},
		{"GET", "/input?", f.permit.Capabilities.Input, 404}, {"GET", "/input?x=1", f.permit.Capabilities.Input, 404}, {"GET", "/%69nput", f.permit.Capabilities.Input, 404}, {"GET", "//input", f.permit.Capabilities.Input, 404}, {"GET", "/input/", f.permit.Capabilities.Input, 404},
		{"GET", "/input", f.permit.Capabilities.Output, 401}, {"POST", "/v1/responses", f.permit.Capabilities.Input, 401}, {"GET", "/input", "sbx1." + strings.Repeat("A", 43), 401},
	} {
		t.Run(tc.method+tc.path+fmt.Sprint(tc.status), func(t *testing.T) {
			r, err := f.request(tc.method, tc.path, tc.token, nil)
			body := closeResponse(t, r, err, tc.status)
			if bytes.Contains(body, []byte(tc.token)) {
				t.Fatal("capability exposed in denial")
			}
		})
	}
	r, _ := http.NewRequest("GET", f.server.URL+"/input", nil)
	r.Header.Add("Authorization", "Bearer "+f.permit.Capabilities.Input)
	r.Header.Add("Authorization", "Bearer "+f.permit.Capabilities.Input)
	resp, err := f.server.Client().Do(r)
	closeResponse(t, resp, err, 401)
	f.profile.Scope.Tenant = "different-tenant"
	f.restartGateway(nil)
	resp, err = f.request("GET", "/input", f.permit.Capabilities.Input, nil)
	closeResponse(t, resp, err, 403)
	f.profile.Scope = f.scope
	f.profile.ExecutionProfileSHA256 = sha([]byte("different execution image"))
	f.restartGateway(nil)
	resp, err = f.request("GET", "/input", f.permit.Capabilities.Input, nil)
	closeResponse(t, resp, err, 403)
	if f.requests.Load() != 0 {
		t.Fatal("denied gateway request reached provider")
	}
}
func TestModelGatewayRewritesTrustedFieldsAndStripsGuestHeaders(t *testing.T) {
	f := setup(t, nil, 0)
	body := []byte(`{"model":"gpt-5","input":"ordinary https://example.invalid and file_id as text","prompt_cache_key":"foreign-tenant-cache","client_metadata":{"session_id":"foreign-trace"},"max_output_tokens":17}`)
	r, _ := http.NewRequest("POST", f.server.URL+"/v1/responses", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.permit.Capabilities.Model)
	r.Header.Set("OpenAI-Organization", "foreign-org")
	r.Header.Set("X-Leak-Capability", f.permit.Capabilities.Model)
	resp, err := f.server.Client().Do(r)
	output := closeResponse(t, resp, err, 200)
	if bytes.Contains(output, []byte(syntheticProviderKey)) || bytes.Contains(output, []byte(f.permit.Capabilities.Model)) {
		t.Fatal("model secret/capability leaked")
	}
	f.seenMu.Lock()
	defer f.seenMu.Unlock()
	m := f.seen[0]
	if m["model"] != "gpt-5" || m["store"] != false || m["background"] != false || m["stream"] != true || m["service_tier"] != "default" || m["max_output_tokens"] != float64(17) || m["client_metadata"] != nil || m["prompt_cache_key"] == "foreign-tenant-cache" {
		t.Fatal("trusted model constraints were not enforced", m)
	}
}
func TestModelGatewayRejectsProviderResourceReferencesAndAmbiguousJSON(t *testing.T) {
	f := setup(t, nil, 0)
	bodies := []string{
		`{"model":"gpt-5","model":"other","input":"x"}`, `{"model":"gpt-5","mo\u0064el":"gpt-5","input":"x"}`, `{"Model":"gpt-5","input":"x"}`,
		`{"model":"gpt-5","input":"x","previous_response_id":"resp_foreign"}`, `{"model":"gpt-5","input":"x","conversation":"conv_foreign"}`, `{"model":"gpt-5","input":"x","prompt":{"id":"pmpt_foreign"}}`,
		`{"model":"gpt-5","input":[{"type":"item_reference","id":"msg_foreign"}]}`, `{"model":"gpt-5","input":[{"type":"reasoning","id":"rs_foreign"}]}`,
		`{"model":"gpt-5","input":[{"role":"user","content":[{"type":"input_file","file_id":"file_foreign"}]}]}`, `{"model":"gpt-5","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://remote.invalid/image"}]}]}`,
		`{"model":"gpt-5","input":"x","tools":[{"type":"file_search","vector_store_ids":["vs_foreign"]}]}`, `{"model":"gpt-5","input":"x","tools":[{"type":"code_interpreter","container":"cntr_foreign"}]}`,
		`{"model":"gpt-5","input":"x","tools":[{"type":"web_search"}]}`, `{"model":"gpt-5","input":"x","tools":[{"type":"computer"}]}`,
		`{"model":"gpt-5","input":"x","tools":[{"type":"namespace","name":"n","tools":[{"type":"web_search","name":"x"}]}]}`,
		`{"model":"gpt-5","input":"x","store":true}`, `{"model":"gpt-5","input":"x","background":true}`, `{"model":"gpt-5","input":"x","service_tier":"priority"}`, `{"model":"gpt-5","input":"x","max_output_tokens":16385}`,
		`{"model":"other","input":"x"}`, `{"model":"gpt-5","input":[{"type":"function_call_output","call_id":"foreign","output":"x"}]}`,
		`{"model":"gpt-5","input":"x","tools":[{"type":"function","name":"f","parameters":{"$ref":"https://remote.invalid/schema"}}]}`,
	}
	for i, body := range bodies {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			r, err := f.request("POST", "/v1/responses", f.permit.Capabilities.Model, []byte(body))
			closeResponse(t, r, err, 400)
		})
	}
	if f.requests.Load() != 0 {
		t.Fatal("rejected body reached provider")
	}
	r, err := f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
	closeResponse(t, r, err, 200)
}
func TestSchemaAcceptsFixedCodexWireAndSelfContainedReasoning(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex-wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Requests []json.RawMessage `json:"requests"`
	}
	if err = json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	p := Profile{Model: "gpt-5", MaxOutputTokens: 1024}
	if len(capture.Requests) != 6 {
		t.Fatal("fixed native request capture incomplete")
	}
	for i, raw := range capture.Requests {
		if _, err = validateRequest(raw, p, "fixture-cache"); err != nil {
			t.Fatalf("captured native request %d rejected: %v", i, err)
		}
	}
	valid := []byte(`{"model":"gpt-5","input":[{"type":"reasoning","id":"rs_inline","summary":[{"type":"summary_text","text":"ordinary URL https://example.invalid"}],"encrypted_content":"opaque-caller-supplied-reasoning"},{"type":"function_call","id":"fc_inline","call_id":"call_local","name":"f","arguments":"{\"file_id\":\"normal function argument\",\"url\":\"https://example.invalid\"}"},{"type":"function_call_output","id":"out_inline","call_id":"call_local","output":"ordinary tool text"}],"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"file_id":{"type":"string"},"url":{"type":"string"}}}}]}`)
	if _, err = validateRequest(valid, p, "fixture-cache"); err != nil {
		t.Fatal("self-contained reasoning/local function data rejected", err)
	}
}
func TestBadBackingObjectNeverStartsGuestTransport(t *testing.T) {
	f := setup(t, nil, 0)
	f.objects.corrupt.Store(true)
	caps := t.TempDir()
	for purpose, value := range map[string]string{"input": f.permit.Capabilities.Input, "output": f.permit.Capabilities.Output} {
		if err := os.WriteFile(filepath.Join(caps, purpose), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ca := filepath.Join(t.TempDir(), "gateway-ca")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	var executed atomic.Int32
	config := guesttransport.Config{Gateway: f.server.URL, CAFile: ca, Capabilities: caps, InputSHA256: sha(fixtureEnvelope), InputSize: int64(len(fixtureEnvelope)), Deadline: f.permit.Run.Deadline, MaxInputBytes: 4096, MaxOutputBytes: 4096}
	err := guesttransport.Run(t.Context(), config, t.TempDir(), func(context.Context, io.Reader, io.Writer) error { executed.Add(1); return nil })
	if err == nil || executed.Load() != 0 {
		t.Fatal("corrupt backing object became an executable input", err, executed.Load())
	}
}

func TestModelBudgetPersistsAcrossGatewayAndCatalogRestart(t *testing.T) {
	f := setup(t, nil, 0)
	f.profile.MaxRequests = 1
	f.restartGateway(nil)
	r, err := f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
	closeResponse(t, r, err, 200)
	f.server.Close()
	f.gateway.Close()
	f.api.Close()
	if err = f.catalog.Close(); err != nil {
		t.Fatal(err)
	}
	f.catalog, err = platform.OpenCatalog(t.Context(), f.directory, "", f.objects)
	if err != nil {
		t.Fatal(err)
	}
	f.startAPI()
	f.startGateway(nil)
	r, err = f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
	closeResponse(t, r, err, 429)
	if f.requests.Load() != 1 {
		t.Fatal("catalog/gateway restart restored model allowance")
	}
}
func TestModelConcurrencyIsBoundedBeforeForward(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	f := setup(t, func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
			completed(w)
		case <-r.Context().Done():
		}
	}, 0)
	f.profile.ModelConcurrency = 2
	f.restartGateway(nil)
	done := make(chan error, 2)
	for range 2 {
		go func() {
			r, err := f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
			if err == nil {
				_, err = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
			}
			done <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("allowed model requests did not reach upstream")
		}
	}
	r, err := f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
	closeResponse(t, r, err, 429)
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if f.requests.Load() != 2 {
		t.Fatal("model concurrency exceeded configured bound")
	}
}
func TestUpstreamFailureConsumesAllowanceWithoutRetryOrReflection(t *testing.T) {
	for _, kind := range []string{"server-error", "redirect", "invalid-content-type", "malformed-sse", "error-sse", "oversize-sse", "lost-upstream-ack"} {
		t.Run(kind, func(t *testing.T) {
			var targetCalls atomic.Int32
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
			defer target.Close()
			f := setup(t, func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "server-error":
					w.WriteHeader(503)
					_, _ = io.WriteString(w, syntheticProviderKey)
				case "redirect":
					w.Header().Set("Location", target.URL+"/stolen")
					w.WriteHeader(307)
				case "invalid-content-type":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"secret":"`+syntheticProviderKey+`"}`)
				case "malformed-sse":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: response.completed\ndata: not-json\n\n")
				case "error-sse":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"error\":\""+syntheticProviderKey+"\"}\n\n")
				case "oversize-sse":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\""+strings.Repeat("x", 1024)+"\"}\n\n")
				case "lost-upstream-ack":
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = connection.Close()
				}
			}, 0)
			f.profile.MaxRequests = 1
			if kind == "oversize-sse" {
				f.profile.PerRequestOutputBytes = 128
			}
			f.restartGateway(nil)
			r, err := f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
			if r != nil {
				body, _ := io.ReadAll(r.Body)
				_ = r.Body.Close()
				if bytes.Contains(body, []byte(syntheticProviderKey)) || bytes.Contains(body, []byte(f.permit.Capabilities.Model)) {
					t.Fatal("provider error exposed secret/capability")
				}
				if r.StatusCode == 200 && err == nil && len(body) > 128 && kind == "oversize-sse" {
					t.Fatal("response exceeded reserved limit")
				}
			}
			r, err = f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
			closeResponse(t, r, err, 429)
			if f.requests.Load() != 1 || targetCalls.Load() != 0 {
				t.Fatalf("upstream failure retried/redirected: source=%d target=%d", f.requests.Load(), targetCalls.Load())
			}
		})
	}
}
func TestCancellationCutsInflightModelUpstreamAndClientStream(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	f := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	}, 0)
	r, err := f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	<-started
	if _, err = f.catalog.CancelSandbox(t.Context(), f.permit.Run.Address()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked capability did not cancel actual upstream socket")
	}
	body, err := io.ReadAll(r.Body)
	if err == nil {
		t.Fatal("cancelled SSE was reported as a complete stream", string(body))
	}
}
func TestOutputSealCutsInflightModelRequest(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	f := setup(t, func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(stopped) }, 0)
	done := make(chan struct{})
	go func() {
		r, _ := f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
		if r != nil {
			_ = r.Body.Close()
		}
		close(done)
	}()
	<-started
	output := []byte("sealed result")
	r, err := f.request("PUT", "/output", f.permit.Capabilities.Output, output)
	closeResponse(t, r, err, 204)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("output seal left model upstream alive")
	}
	<-done
}
func TestSlowOutputBodyIsInterruptedByRealSocketDeadlineOrRevocation(t *testing.T) {
	for _, kind := range []string{"deadline", "revoke"} {
		t.Run(kind, func(t *testing.T) {
			deadline := time.Duration(0)
			if kind == "deadline" {
				deadline = 350 * time.Millisecond
			}
			f := setup(t, nil, deadline)
			host := strings.TrimPrefix(f.server.URL, "https://")
			config := f.server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			connection, err := tls.Dial("tcp", host, config)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			output := []byte("synthetic upload body")
			_, err = fmt.Fprintf(connection, "PUT /output HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nX-Agentmoor-SHA256: %s\r\nX-Agentmoor-Size: %d\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", host, f.permit.Capabilities.Output, sha(output), len(output), len(output), output[:1])
			if err != nil {
				t.Fatal(err)
			}
			if kind == "revoke" {
				for until := time.Now().Add(time.Second); ; {
					run, err := f.catalog.SandboxRun(t.Context(), f.permit.Run.Address())
					if err != nil {
						t.Fatal(err)
					}
					if run.Output.SHA256 != "" {
						break
					}
					if time.Now().After(until) {
						t.Fatal("slow upload never declared candidate")
					}
					time.Sleep(time.Millisecond)
				}
				if _, err := f.catalog.CancelSandbox(t.Context(), f.permit.Run.Address()); err != nil {
					t.Fatal(err)
				}
			}
			if err = connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			reply, err := io.ReadAll(connection)
			if err != nil {
				if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
					t.Fatal("actual TLS socket remained open past task deadline/revocation")
				}
			}
			if bytes.Contains(reply, []byte("204 No Content")) {
				t.Fatal("partial output was acknowledged")
			}
			run, err := f.catalog.SandboxRun(t.Context(), f.permit.Run.Address())
			if err != nil || run.Output.Sealed {
				t.Fatal("partial output committed", run, err)
			}
		})
	}
}

// Intercept only a committed successful PUT acknowledgement, then break its
// actual TLS connection. The next request reaches the same immutable result.
type drop204 struct {
	http.ResponseWriter
	drop bool
}

func (w *drop204) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *drop204) WriteHeader(status int) {
	if status == 204 && w.drop {
		connection, _, err := w.ResponseWriter.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
		return
	}
	w.ResponseWriter.WriteHeader(status)
}
func TestLostGatewayAckReplaysSameOutputWithoutReexecutingGuest(t *testing.T) {
	f := setup(t, nil, 0)
	var uploads atomic.Int32
	f.restartGateway(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "PUT" {
				next.ServeHTTP(&drop204{ResponseWriter: w, drop: uploads.Add(1) == 1}, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	caps := t.TempDir()
	for purpose, value := range map[string]string{"input": f.permit.Capabilities.Input, "output": f.permit.Capabilities.Output} {
		if err := os.WriteFile(filepath.Join(caps, purpose), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ca := filepath.Join(t.TempDir(), "gateway-ca")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	var callbacks atomic.Int32
	output := bytes.Repeat([]byte("r"), 2<<20)
	config := guesttransport.Config{Gateway: f.server.URL, CAFile: ca, Capabilities: caps, InputSHA256: sha(fixtureEnvelope), InputSize: int64(len(fixtureEnvelope)), Deadline: f.permit.Run.Deadline, MaxInputBytes: 4096, MaxOutputBytes: 3 << 20}
	err := guesttransport.Run(t.Context(), config, t.TempDir(), func(_ context.Context, input io.Reader, result io.Writer) error {
		callbacks.Add(1)
		body, err := io.ReadAll(input)
		if err != nil || !bytes.Equal(body, fixtureEnvelope) {
			return fmt.Errorf("input changed")
		}
		_, err = result.Write(output)
		return err
	})
	if err != nil || callbacks.Load() != 1 || uploads.Load() != 2 {
		t.Fatalf("lost gateway acknowledgement reran guest: err=%v callback=%d PUT=%d", err, callbacks.Load(), uploads.Load())
	}
	run, err := f.catalog.SandboxRun(t.Context(), f.permit.Run.Address())
	if err != nil || !run.Output.Sealed || run.Output.SHA256 != sha(output) {
		t.Fatal("lost acknowledgement changed frozen result", run, err)
	}
}
func TestWrongOutputDigestAndSizeNeverSealResult(t *testing.T) {
	for _, kind := range []string{"digest", "short", "long", "duplicate-header", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t, nil, 0)
			raw := []byte("synthetic output")
			r, _ := http.NewRequest("PUT", f.server.URL+"/output", bytes.NewReader(raw))
			r.Header.Set("Authorization", "Bearer "+f.permit.Capabilities.Output)
			r.Header.Set("X-Agentmoor-SHA256", sha(raw))
			r.Header.Set("X-Agentmoor-Size", fmt.Sprint(len(raw)))
			switch kind {
			case "digest":
				r.Header.Set("X-Agentmoor-SHA256", sha([]byte("different")))
			case "short":
				r.Header.Set("X-Agentmoor-Size", fmt.Sprint(len(raw)+1))
			case "long":
				r.Header.Set("X-Agentmoor-Size", fmt.Sprint(len(raw)-1))
			case "duplicate-header":
				r.Header.Add("X-Agentmoor-Size", fmt.Sprint(len(raw)))
			case "oversize":
				r.Header.Set("X-Agentmoor-Size", fmt.Sprint(platform.SandboxMaxBytes+1))
			}
			response, err := f.server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == 204 {
				t.Fatal("bad output accepted")
			}
			run, err := f.catalog.SandboxRun(t.Context(), f.permit.Run.Address())
			if err != nil || run.Output.Sealed {
				t.Fatal("invalid output sealed", run, err)
			}
		})
	}
}
func TestTLSFactoryCannotExposePlaintextOrUnboundedHeaders(t *testing.T) {
	f := setup(t, nil, 0)
	server := NewTLSServer("127.0.0.1:0", f.gateway)
	if server.Handler != f.gateway || server.MaxHeaderBytes != 32<<10 || server.ReadHeaderTimeout == 0 || server.ReadTimeout == 0 || server.WriteTimeout == 0 || len(server.TLSNextProto) != 0 {
		t.Fatal("unbounded or non-dedicated listener configuration")
	}
	plain := httptest.NewServer(f.gateway)
	defer plain.Close()
	response, err := http.Get(plain.URL + "/input")
	closeResponse(t, response, err, 503)
}

func TestInputDownloadRevocationInterruptsActualTLSWrite(t *testing.T) {
	f := setup(t, nil, 0)
	// Replace only this fixture's frozen input before a fresh create fence. The
	// large stream exceeds loopback socket buffering, so a non-reading client
	// forces the server into an actual blocked network write.
	s := f.scope
	s.Runtime += "-large"
	a := platform.SandboxAddress{Scope: s, Attempt: uuid.NewString()}
	secret := bytes.Repeat([]byte{9}, 32)
	token, err := platform.Issue(secret, platform.Grant{Scope: s, Read: true, Submit: true, Execute: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	client, err := platform.NewClient(f.api.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	var record task.Record
	if err = client.JSON(t.Context(), "PUT", "/v1/tasks/"+s.Runtime, task.Spec{Instruction: "large synthetic input", Output: "reply.txt"}, &record); err != nil {
		t.Fatal(err)
	}
	if err = client.JSON(t.Context(), "POST", "/v1/tasks/"+s.Runtime+"/claim", task.Claim{Attempt: a.Attempt}, &record); err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte("z"), 64<<20)
	run, err := f.catalog.BeginSandbox(t.Context(), a, fixtureExecutionSHA, blob(large))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.catalog.SealSandboxInput(t.Context(), a, bytes.NewReader(large)); err != nil {
		t.Fatal(err)
	}
	permit, err := f.catalog.FenceSandboxCreate(t.Context(), a, run.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	f.profile.Scope = s
	f.restartGateway(nil)
	response, err := f.request("GET", "/input", permit.Capabilities.Input, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, 1)
	if _, err = io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	profile := f.gateway.profiles[profileKey(s, fixtureExecutionSHA)]
	if len(profile.handoff) != 1 {
		t.Fatal("fixture did not force an in-flight network write")
	}
	if _, err = f.catalog.CancelSandbox(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	// Keep the client from draining its receive buffer until the handler has
	// actually been interrupted and released the handoff permit.
	for until := time.Now().Add(2 * time.Second); len(profile.handoff) != 0; {
		if time.Now().After(until) {
			t.Fatal("revoked TLS write did not release its handler")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, response.Body); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("revoked input download was reported complete")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revoked input kept TLS write alive")
	}
}
func TestHandoffConcurrencyRejectsThirdUploadBeforeReadingBody(t *testing.T) {
	f := setup(t, nil, 0)
	host := strings.TrimPrefix(f.server.URL, "https://")
	tlsConfig := f.server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	var connections []*tls.Conn
	for range 2 {
		connection, err := tls.Dial("tcp", host, tlsConfig)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
		_, err = fmt.Fprintf(connection, "PUT /output HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nX-Agentmoor-SHA256: %s\r\nX-Agentmoor-Size: 8\r\nContent-Length: 8\r\n\r\nx", host, f.permit.Capabilities.Output, sha([]byte("fixture!")))
		if err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, connection := range connections {
			_ = connection.Close()
		}
	}()
	profile := f.gateway.profiles[profileKey(f.scope, fixtureExecutionSHA)]
	for until := time.Now().Add(time.Second); len(profile.handoff) != 2; {
		if time.Now().After(until) {
			t.Fatal("two slow handoffs never acquired their bounded permits")
		}
		time.Sleep(time.Millisecond)
	}
	response, err := f.request("GET", "/input", f.permit.Capabilities.Input, nil)
	closeResponse(t, response, err, 429)
}
func TestPolicyChangeOrKeyRotationCannotResetModelBudget(t *testing.T) {
	for _, kind := range []string{"key-rotation", "policy-change"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t, nil, 0)
			f.profile.MaxRequests = 1
			f.restartGateway(nil)
			response, err := f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
			closeResponse(t, response, err, 200)
			status := 429
			if kind == "key-rotation" {
				if err = os.WriteFile(f.profile.APIKeyFile, []byte("synthetic-rotated-provider-key"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				f.profile.MaxRequests = 2
				status = 409
			}
			f.restartGateway(nil)
			response, err = f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
			closeResponse(t, response, err, status)
			if f.requests.Load() != 1 {
				t.Fatal("configuration update reopened exhausted ledger")
			}
		})
	}
}
func TestRequestHeadersBodyAndSchemaHaveHardBounds(t *testing.T) {
	f := setup(t, nil, 0)
	for _, kind := range []string{"headers", "body", "depth"} {
		t.Run(kind, func(t *testing.T) {
			raw := modelBody()
			status := 400
			if kind == "body" {
				raw = bytes.Repeat([]byte("x"), (256<<10)+1)
			}
			if kind == "depth" {
				raw = []byte(`{"model":"gpt-5","input":"x","client_metadata":{"nested":` + strings.Repeat("[", 34) + `0` + strings.Repeat("]", 34) + `}}`)
			}
			request, _ := http.NewRequest("POST", f.server.URL+"/v1/responses", bytes.NewReader(raw))
			request.Header.Set("Authorization", "Bearer "+f.permit.Capabilities.Model)
			request.Header.Set("Content-Type", "application/json")
			if kind == "headers" {
				request.Header.Set("X-Large-Fixture", strings.Repeat("x", 33<<10))
				status = 431
			}
			response, err := f.server.Client().Do(request)
			closeResponse(t, response, err, status)
		})
	}
	if f.requests.Load() != 0 {
		t.Fatal("over-bound request reached provider")
	}
}
func TestConfigurationBoundsAndNoGuestSelectedUpstream(t *testing.T) {
	f := setup(t, nil, 0)
	for name, change := range map[string]func(*Profile){"http": func(p *Profile) { p.UpstreamURL = "http://provider.invalid/v1/responses" }, "query": func(p *Profile) { p.UpstreamURL += "?route=foreign" }, "other-path": func(p *Profile) { p.UpstreamURL = f.upstream.URL + "/v1/files" }, "userinfo": func(p *Profile) { p.UpstreamURL = "https://u:p@provider.invalid/v1/responses" }, "relative-key": func(p *Profile) { p.APIKeyFile = "key" }, "token-budget": func(p *Profile) { p.MaxOutputTokens = MaxOutputTokens + 1 }, "concurrency": func(p *Profile) { p.ModelConcurrency = 5 }, "scope": func(p *Profile) { p.Scope.Tenant = "" }, "profile-sha": func(p *Profile) { p.ExecutionProfileSHA256 = "invalid" }} {
		t.Run(name, func(t *testing.T) {
			p := f.profile
			change(&p)
			gateway, err := New(f.catalog, &Config{Profiles: []Profile{p}})
			if err == nil {
				gateway.Close()
				t.Fatal("invalid trusted profile accepted")
			}
		})
	}
	if _, err := New(f.catalog, &Config{Profiles: []Profile{f.profile, f.profile}}); err == nil {
		t.Fatal("ambiguous duplicate profile accepted")
	}
	if _, err := New(f.catalog, &Config{Profiles: make([]Profile, MaxProfiles+1)}); err == nil {
		t.Fatal("unbounded profile map accepted")
	}
}

func TestUpstreamPortConfigurationIsRejectedBeforeAnyDial(t *testing.T) {
	f := setup(t, nil, 0)
	for _, origin := range []string{"https://provider.invalid:/v1/responses", "https://provider.invalid:0/v1/responses", "https://provider.invalid:65536/v1/responses", "https://[::1]:/v1/responses"} {
		p := f.profile
		p.UpstreamURL = origin
		if gateway, err := New(f.catalog, &Config{Profiles: []Profile{p}}); err == nil {
			gateway.Close()
			t.Fatal("accepted invalid upstream port", origin)
		}
	}
}
func TestSSESecretPropertyNameIsNotReflected(t *testing.T) {
	f := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",%q:\"not allowed\"}\n\n", syntheticProviderKey)
	}, 0)
	response, _ := f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
	if response != nil {
		raw, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if bytes.Contains(raw, []byte(syntheticProviderKey)) {
			t.Fatal("upstream secret in JSON key leaked")
		}
	}
}

func TestUpstreamTLSRequiresConfiguredCAAndDoesNotInheritProxy(t *testing.T) {
	t.Run("wrong-real-ca", func(t *testing.T) {
		f := setup(t, nil, 0)
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		certificate := &x509.Certificate{SerialNumber: big.NewInt(7), IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
		der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(f.profile.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
			t.Fatal(err)
		}
		f.profile.MaxRequests = 1
		f.restartGateway(nil)
		response, err := f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
		closeResponse(t, response, err, 502)
		response, err = f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
		closeResponse(t, response, err, 429)
		if f.requests.Load() != 0 {
			t.Fatal("untrusted TLS upstream reached HTTP")
		}
	})
	t.Run("proxy-environment", func(t *testing.T) {
		var calls atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(502) }))
		defer proxy.Close()
		t.Setenv("HTTPS_PROXY", proxy.URL)
		t.Setenv("https_proxy", proxy.URL)
		t.Setenv("NO_PROXY", "")
		t.Setenv("no_proxy", "")
		f := setup(t, nil, 0)
		response, err := f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
		closeResponse(t, response, err, 200)
		if calls.Load() != 0 || f.requests.Load() != 1 {
			t.Fatal("trusted upstream transport inherited ambient proxy")
		}
	})
}
func TestConfigurationFIFOsAndHugeFilesCannotBlockGatewayCreation(t *testing.T) {
	for _, field := range []string{"key", "ca"} {
		for _, kind := range []string{"fifo", "large", "directory"} {
			t.Run(field+"-"+kind, func(t *testing.T) {
				f := setup(t, nil, 0)
				path := filepath.Join(t.TempDir(), "config")
				switch kind {
				case "fifo":
					if err := syscall.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				case "large":
					size := 4097
					if field == "ca" {
						size = (1 << 20) + 1
					}
					if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				p := f.profile
				if field == "key" {
					p.APIKeyFile = path
				} else {
					p.CAFile = path
				}
				done := make(chan error, 1)
				go func() {
					gateway, err := New(f.catalog, &Config{Profiles: []Profile{p}})
					if gateway != nil {
						gateway.Close()
					}
					done <- err
				}()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("invalid trusted file accepted")
					}
					if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), syntheticProviderKey) {
						t.Fatal("configuration error disclosed path/secret")
					}
				case <-time.After(time.Second):
					t.Fatal("configuration file blocked gateway creation")
				}
			})
		}
	}
}
func TestOriginalDeadlineCutsModelSSEWithoutRenewal(t *testing.T) {
	stopped := make(chan struct{})
	f := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stopped)
	}, 350*time.Millisecond)
	response, err := f.request("POST", "/v1/responses", f.permit.Capabilities.Model, modelBody())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("model request extended original task deadline")
	}
	if _, err = io.ReadAll(response.Body); err == nil {
		t.Fatal("deadline-truncated SSE was accepted as complete")
	}
}

type deadlineCapture struct {
	http.ResponseWriter
	mu        *sync.Mutex
	deadlines *[]time.Time
}

func (w *deadlineCapture) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *deadlineCapture) record(deadline time.Time) {
	w.mu.Lock()
	*w.deadlines = append(*w.deadlines, deadline)
	w.mu.Unlock()
}
func (w *deadlineCapture) SetReadDeadline(deadline time.Time) error {
	w.record(deadline)
	return http.NewResponseController(w.ResponseWriter).SetReadDeadline(deadline)
}
func (w *deadlineCapture) SetWriteDeadline(deadline time.Time) error {
	w.record(deadline)
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
}
func TestSuccessfulFinishNearDeadlineNeverExtendsActualSocketDeadline(t *testing.T) {
	f := setup(t, nil, time.Second)
	var mu sync.Mutex
	var deadlines []time.Time
	f.restartGateway(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(&deadlineCapture{ResponseWriter: w, mu: &mu, deadlines: &deadlines}, r)
		})
	})
	time.Sleep(time.Until(f.permit.Run.Deadline.Add(-250 * time.Millisecond)))
	response, err := f.request("PUT", "/output", f.permit.Capabilities.Output, []byte("complete near original deadline"))
	closeResponse(t, response, err, 204)
	mu.Lock()
	defer mu.Unlock()
	if len(deadlines) < 4 {
		t.Fatal("real deadline setters were not exercised")
	}
	for _, deadline := range deadlines {
		if deadline.After(f.permit.Run.Deadline) {
			t.Fatal("normal finish extended original socket deadline", deadline, f.permit.Run.Deadline)
		}
	}
}

func TestSSECommentsCannotSmuggleSecretsAlongsideValidEvents(t *testing.T) {
	var capValue string
	f := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, ": debug Authorization Bearer %s and %s\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n", syntheticProviderKey, capValue)
	}, 0)
	capValue = f.permit.Capabilities.Model
	response, err := f.request("POST", "/v1/responses", capValue, modelBody())
	raw := closeResponse(t, response, err, 200)
	if bytes.Contains(raw, []byte(syntheticProviderKey)) || bytes.Contains(raw, []byte(capValue)) || bytes.Contains(raw, []byte(": debug")) {
		t.Fatal("SSE comment crossed gateway validation")
	}
}
