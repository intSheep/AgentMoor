package faultproxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
)

var fixtureTarget = asset.Key{Kind: "workspace", ID: "turns/9a8ded69-f941-43a4-95d4-24648187c15c/root"}

type proxyFixture struct {
	t      *testing.T
	p      *Proxy
	up     *httptest.Server
	down   *httptest.Server
	admin  *httptest.Server
	client *http.Client
}

func newProxyFixture(t *testing.T, handler http.Handler, trust bool) *proxyFixture {
	t.Helper()
	up := httptest.NewUnstartedServer(handler)
	up.EnableHTTP2 = true // The upstream offers h2; the proxy must still use h1.
	up.TLS = &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
	up.StartTLS()
	t.Cleanup(up.Close)
	roots := x509.NewCertPool()
	if trust {
		roots.AddCert(up.Certificate())
	}
	p, err := New(up.URL, roots)
	if err != nil {
		t.Fatal(err)
	}
	down := httptest.NewUnstartedServer(p)
	down.TLS = HTTP1TLSConfig()
	down.StartTLS()
	t.Cleanup(down.Close)
	admin := httptest.NewServer(p.Admin())
	t.Cleanup(admin.Close)
	dowRoots := x509.NewCertPool()
	dowRoots.AddCert(down.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: dowRoots, MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"}}, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
	t.Cleanup(transport.CloseIdleConnections)
	// A failed assertion must not leave a deliberately held handler hanging in
	// httptest.Server.Close. Release before closing either local listener.
	t.Cleanup(func() {
		request, _ := http.NewRequest("POST", admin.URL+"/release", strings.NewReader(`{}`))
		response, _ := (&http.Client{Timeout: time.Second}).Do(request)
		closeResponse(response)
	})
	return &proxyFixture{t: t, p: p, up: up, down: down, admin: admin,
		client: &http.Client{Transport: transport, Timeout: 8 * time.Second}}
}

func fixtureChange() asset.Change {
	raw := []byte("synthetic frozen tar bytes")
	return asset.Change{Metadata: asset.Metadata{Key: fixtureTarget, AdapterVersion: "fixture-1", Format: "workspace-tar.v1",
		MediaType: "application/x-tar", Name: "root", Origin: "runtime", Completeness: "persisted-files",
		SHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), Size: int64(len(raw))}}
}

func (h *proxyFixture) adminJSON(method, path string, input any, output any) []byte {
	h.t.Helper()
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			h.t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, h.admin.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 {
		h.t.Fatalf("admin %s %s: status=%d body=%s error=%v", method, path, response.StatusCode, raw, err)
	}
	if output != nil && json.Unmarshal(raw, output) != nil {
		h.t.Fatalf("admin %s returned invalid JSON: %s", path, raw)
	}
	return raw
}

func (h *proxyFixture) arm(stage, timing string, hold bool) {
	h.t.Helper()
	h.adminJSON("POST", "/arm", map[string]any{"case_id": "fixture-" + stage + "-" + timing, "stage": stage,
		"timing": timing, "limit": 1, "target": fixtureTarget, "hold_first": hold}, nil)
}

func (h *proxyFixture) status() map[string]any {
	h.t.Helper()
	var status map[string]any
	h.adminJSON("GET", "/status", nil, &status)
	return status
}

func (h *proxyFixture) blocked() map[string]any {
	h.t.Helper()
	until := time.Now().Add(4 * time.Second)
	for time.Now().Before(until) {
		status := h.status()
		if status["blocked"] == true {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatal("selected request did not enter its visible barrier")
	return nil
}

func (h *proxyFixture) request(ctx context.Context, method, path string, input any) (*http.Response, error) {
	var body io.Reader
	if input != nil {
		if raw, ok := input.([]byte); ok {
			body = bytes.NewReader(raw)
		} else {
			raw, err := json.Marshal(input)
			if err != nil {
				return nil, err
			}
			body = bytes.NewReader(raw)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, h.down.URL+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer never-record-this-token")
	request.Header.Set("Content-Type", "application/json")
	return h.client.Do(request)
}

func closeResponse(response *http.Response) {
	if response != nil {
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
}

func TestBeforeBeginBarrierDoesNotReachUpstream(t *testing.T) {
	var posts, monitors atomic.Int32
	h := newProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
		} else {
			monitors.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{ "state": "running" }`)
	}), true)
	h.arm("begin", "before", true)
	done := make(chan error, 1)
	go func() {
		response, err := h.request(t.Context(), "POST", "/v1/changes?private-query=never-record-this-query", fixtureChange())
		if err == nil && response.StatusCode != 503 {
			err = fmt.Errorf("before fault returned HTTP %d", response.StatusCode)
		}
		closeResponse(response)
		done <- err
	}()
	status := h.blocked()
	if status["hits"] != float64(1) || posts.Load() != 0 {
		t.Fatalf("before barrier already reached upstream: status=%v posts=%d", status, posts.Load())
	}
	response, err := h.request(t.Context(), "GET", "/v1/tasks/report", nil)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("monitor GET blocked behind publication barrier: %v", err)
	}
	closeResponse(response)
	if monitors.Load() != 1 {
		t.Fatal("monitor did not reach upstream")
	}
	ledger := h.adminJSON("GET", "/ledger", nil, nil)
	if bytes.Contains(ledger, []byte("never-record-this-token")) || bytes.Contains(ledger, []byte("never-record-this-query")) {
		t.Fatal("ledger exposed a credential or query")
	}
	h.adminJSON("POST", "/release", map[string]any{}, nil)
	if err := <-done; err != nil || posts.Load() != 0 {
		t.Fatalf("before release did not return 503 without upstream POST: %v posts=%d", err, posts.Load())
	}
}

func TestAfterCommitReadsCompleteSuccessfulResponseThenDropsAcknowledgement(t *testing.T) {
	change := fixtureChange()
	op := asset.Fingerprint(change)
	prefixWritten, finishResponse, handlerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	finish := func() { finishOnce.Do(func() { close(finishResponse) }) }
	var commits atomic.Int32
	h := newProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/changes":
			json.NewEncoder(w).Encode(asset.Ticket{ID: op, Upload: true})
		case r.Method == "POST" && r.URL.Path == "/v1/changes/"+op+"/commit":
			commits.Add(1)
			defer close(handlerDone)
			io.WriteString(w, `{"id":"`+op+`","version":`)
			w.(http.Flusher).Flush()
			close(prefixWritten)
			select {
			case <-finishResponse:
				io.WriteString(w, `1}`)
			case <-r.Context().Done():
				t.Error("proxy cancelled upstream before its successful response completed")
			}
		default:
			io.WriteString(w, `{ "state": "running" }`)
		}
	}), true)
	t.Cleanup(finish)
	h.arm("commit", "after", true)
	response, err := h.request(t.Context(), "POST", "/v1/changes", change)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("normal Begin failed: %v", err)
	}
	closeResponse(response)
	done := make(chan error, 1)
	go func() {
		response, err := h.request(t.Context(), "POST", "/v1/changes/"+op+"/commit", nil)
		closeResponse(response)
		done <- err
	}()
	select {
	case <-prefixWritten:
	case <-time.After(3 * time.Second):
		t.Fatal("commit did not reach upstream")
	}
	if status := h.status(); status["blocked"] == true {
		t.Fatal("after barrier started before the upstream response was complete")
	}
	finish()
	status := h.blocked()
	select {
	case <-handlerDone:
	default:
		t.Fatal("after barrier started before upstream handler completed")
	}
	if commits.Load() != 1 || status["operation"] != op {
		t.Fatalf("after barrier did not identify the one actual commit: %v", status)
	}
	response, err = h.request(t.Context(), "GET", "/v1/tasks/report/checkpoint", nil)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("monitor GET failed during after barrier: %v", err)
	}
	closeResponse(response)
	h.adminJSON("POST", "/release", map[string]any{}, nil)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "EOF") {
			t.Fatalf("successful commit acknowledgement was not dropped as EOF: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("released after request hung")
	}
}

func TestHeldRequestCancellationReleasesBarrierWithoutBlockingAdmin(t *testing.T) {
	var posts atomic.Int32
	h := newProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
		}
		io.WriteString(w, `{}`)
	}), true)
	h.arm("begin", "before", true)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		response, err := h.request(ctx, "POST", "/v1/changes", fixtureChange())
		closeResponse(response)
		done <- err
	}()
	h.blocked()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled held request unexpectedly succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("held request ignored cancellation")
	}
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		if h.status()["blocked"] == false {
			if posts.Load() != 0 {
				t.Fatal("cancelled before request reached upstream")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("cancelled request left admin status blocked")
}

func TestAfterFaultDoesNotDropAnUnsuccessfulUpstreamResponse(t *testing.T) {
	change := fixtureChange()
	op := asset.Fingerprint(change)
	h := newProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/changes" {
			json.NewEncoder(w).Encode(asset.Ticket{ID: op, Upload: true})
			return
		}
		http.Error(w, "synthetic upstream commit failure", http.StatusInternalServerError)
	}), true)
	h.arm("commit", "after", true)
	response, err := h.request(t.Context(), "POST", "/v1/changes", change)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("begin: %v", err)
	}
	closeResponse(response)
	response, err = h.request(t.Context(), "POST", "/v1/changes/"+op+"/commit", nil)
	if err != nil || response.StatusCode != 500 {
		t.Fatalf("after fault hid an unsuccessful commit response: response=%v error=%v", response, err)
	}
	closeResponse(response)
	if h.status()["blocked"] == true {
		t.Fatal("unsuccessful upstream response entered the successful-acknowledgement barrier")
	}
}

func TestOrdinaryGETStreamsAndPropagatesCancellation(t *testing.T) {
	cancelled := make(chan struct{})
	stop := make(chan struct{})
	h := newProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first streamed bytes\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-stop:
		}
	}), true)
	t.Cleanup(func() { close(stop) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	response, err := h.request(ctx, "GET", "/v1/content?kind=workspace&id=root&version=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	prefix := make([]byte, len("first streamed bytes\n"))
	if _, err := io.ReadFull(response.Body, prefix); err != nil || string(prefix) != "first streamed bytes\n" {
		t.Fatalf("ordinary GET did not stream before the upstream completed: %q %v", prefix, err)
	}
	cancel()
	response.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("downstream cancellation did not stop the upstream streaming GET")
	}
}

func TestUpstreamCertificateIsVerifiedAndBothTLSLinksUseHTTP1(t *testing.T) {
	for _, trust := range []bool{false, true} {
		t.Run(fmt.Sprint(trust), func(t *testing.T) {
			var called atomic.Int32
			h := newProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called.Add(1)
				if r.ProtoMajor != 1 || r.TLS == nil || r.TLS.NegotiatedProtocol != "http/1.1" {
					t.Errorf("upstream did not use verified HTTP/1 TLS: proto=%s TLS=%+v", r.Proto, r.TLS)
				}
				io.WriteString(w, `{}`)
			}), trust)
			response, err := h.request(t.Context(), "GET", "/v1/tasks/report", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeResponse(response)
			if response.ProtoMajor != 1 || response.TLS == nil || response.TLS.NegotiatedProtocol != "http/1.1" {
				t.Fatalf("downstream negotiated a protocol other than HTTP/1 TLS: %+v", response)
			}
			if trust && (response.StatusCode != 200 || called.Load() != 1) {
				t.Fatalf("trusted upstream not forwarded: status=%d calls=%d", response.StatusCode, called.Load())
			}
			if !trust && (response.StatusCode != http.StatusBadGateway || called.Load() != 0) {
				t.Fatalf("proxy ignored invalid upstream CA: status=%d calls=%d", response.StatusCode, called.Load())
			}
		})
	}
}

func TestManifestAndPartSelectionKeepFixedOperationAndMetadata(t *testing.T) {
	first, other := []byte("first selected missing body"), []byte("other parallel body")
	firstSHA, otherSHA := fmt.Sprintf("%x", sha256.Sum256(first)), fmt.Sprintf("%x", sha256.Sum256(other))
	manifest := asset.TarParts{Format: asset.TarPartsFormat, Parts: []asset.TarPart{
		{SHA256: firstSHA, Size: int64(len(first))}, {SHA256: otherSHA, Size: int64(len(other))},
	}}
	change := fixtureChange()
	change.Size = int64(len(first) + len(other))
	change.SHA256 = fmt.Sprintf("%x", sha256.Sum256(append(append([]byte{}, first...), other...)))
	op := asset.Fingerprint(change)
	var partCalls sync.Map
	h := newProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/changes":
			json.NewEncoder(w).Encode(asset.Ticket{ID: op, Upload: true})
		case r.URL.Path == "/v1/changes/"+op+"/parts":
			reader, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Error(err)
				http.Error(w, "bad gzip", 400)
				return
			}
			var decoded asset.TarParts
			err = json.NewDecoder(reader).Decode(&decoded)
			reader.Close()
			if err != nil || decoded.Validate(change) != nil {
				t.Error("proxy did not forward the unchanged valid gzip manifest", err)
			}
			json.NewEncoder(w).Encode(map[string]any{"missing": manifest.Parts})
		case strings.HasPrefix(r.URL.Path, "/v1/changes/"+op+"/parts/"):
			digest := strings.TrimPrefix(r.URL.Path, "/v1/changes/"+op+"/parts/")
			raw, _ := io.ReadAll(r.Body)
			if digest != fmt.Sprintf("%x", sha256.Sum256(raw)) {
				t.Error("proxy changed part bytes")
			}
			partCalls.Store(digest, true)
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}), true)
	h.arm("part", "before", false)
	response, err := h.request(t.Context(), "POST", "/v1/changes", change)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("begin: %v", err)
	}
	closeResponse(response)
	var zipped bytes.Buffer
	writer := gzip.NewWriter(&zipped)
	if err := json.NewEncoder(writer).Encode(manifest); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	request, err := http.NewRequestWithContext(t.Context(), "POST", h.down.URL+"/v1/changes/"+op+"/parts", bytes.NewReader(zipped.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Encoding", "gzip")
	response, err = h.client.Do(request)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("manifest: %v", err)
	}
	closeResponse(response)
	// The first missing entry is selected even if another part arrives first.
	response, err = h.request(t.Context(), "PUT", "/v1/changes/"+op+"/parts/"+otherSHA, other)
	if err != nil || response.StatusCode != 204 {
		t.Fatalf("unselected parallel part: %v", err)
	}
	closeResponse(response)
	response, err = h.request(t.Context(), "PUT", "/v1/changes/"+op+"/parts/"+firstSHA, first)
	if err != nil || response.StatusCode != 503 {
		t.Fatalf("selected first missing part: %v", err)
	}
	closeResponse(response)
	if _, exists := partCalls.Load(firstSHA); exists {
		t.Fatal("before part fault forwarded the selected body")
	}
	if _, exists := partCalls.Load(otherSHA); !exists {
		t.Fatal("part fault blocked a different parallel body")
	}
	response, err = h.request(t.Context(), "PUT", "/v1/changes/"+op+"/parts/"+firstSHA, first)
	if err != nil || response.StatusCode != 204 {
		t.Fatalf("part retry: %v", err)
	}
	closeResponse(response)
	status := h.status()
	if status["operation"] != op || status["hits"] != float64(1) {
		t.Fatalf("fixed operation or unique hit changed: %v", status)
	}
	ledger := h.adminJSON("GET", "/ledger", nil, nil)
	if bytes.Contains(ledger, first) || bytes.Contains(ledger, other) {
		t.Fatal("ledger exposed raw part bodies")
	}
}

func (h *proxyFixture) adminAttempt(input any) (int, []byte) {
	h.t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		h.t.Fatal(err)
	}
	request, err := http.NewRequest("POST", h.admin.URL+"/arm", bytes.NewReader(raw))
	if err != nil {
		h.t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return response.StatusCode, body
}

func armInput(id string, target asset.Key, limit int) map[string]any {
	return map[string]any{"case_id": id, "stage": "commit", "timing": "before", "target": target, "limit": limit, "hold_first": false}
}

func TestAdminRejectsInvalidTargetLimitsAndNonLoopback(t *testing.T) {
	h := newProxyFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid admin arm unexpectedly reached upstream")
	}), true)
	for _, test := range []struct {
		name   string
		target asset.Key
		limit  int
	}{
		{"artifact", asset.Key{Kind: "artifact", ID: fixtureTarget.ID}, 1},
		{"unscoped workspace", asset.Key{Kind: "workspace", ID: "root"}, 1},
		{"nil turn", asset.Key{Kind: "workspace", ID: "turns/00000000-0000-0000-0000-000000000000/root"}, 1},
		{"extra suffix", asset.Key{Kind: "workspace", ID: fixtureTarget.ID + "/extra"}, 1},
		{"zero limit", fixtureTarget, 0},
		{"unbounded limit", fixtureTarget, 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := h.adminAttempt(armInput("invalid-case", test.target, test.limit))
			if status != 400 {
				t.Fatalf("invalid arm accepted: status=%d body=%s", status, body)
			}
		})
	}
	for _, field := range []string{"stage", "timing"} {
		input := armInput("invalid-case", fixtureTarget, 1)
		input[field] = "unsupported"
		if status, _ := h.adminAttempt(input); status != 400 {
			t.Fatalf("invalid %s accepted: HTTP %d", field, status)
		}
	}
	request := httptest.NewRequest("GET", "http://admin/status", nil)
	request.RemoteAddr = "192.0.2.1:12345"
	response := httptest.NewRecorder()
	h.p.Admin().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatal("admin accepted a non-loopback caller", response.Code)
	}
	var ledger []map[string]any
	h.adminJSON("GET", "/ledger", nil, &ledger)
	if len(ledger) != 0 {
		t.Fatal("invalid arms consumed or polluted the bounded case ledger", ledger)
	}
}

func TestCaseLedgerStopsAtThirtyTwoWithoutEvictingEvidence(t *testing.T) {
	h := newProxyFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("arming cases unexpectedly reached upstream")
	}), true)
	for index := 1; index <= 32; index++ {
		target := asset.Key{Kind: "workspace", ID: fmt.Sprintf("turns/9a8ded69-f941-43a4-95d4-%012x/root", index)}
		if status, body := h.adminAttempt(armInput(fmt.Sprintf("case-%d", index), target, 3)); status != 200 {
			t.Fatalf("case %d: status=%d body=%s", index, status, body)
		}
	}
	target := asset.Key{Kind: "workspace", ID: "turns/9a8ded69-f941-43a4-95d4-ffffffffffff/root"}
	if status, body := h.adminAttempt(armInput("case-overflow", target, 1)); status != http.StatusTooManyRequests {
		t.Fatalf("overflow was accepted: status=%d body=%s", status, body)
	}
	var ledger []map[string]any
	h.adminJSON("GET", "/ledger", nil, &ledger)
	if len(ledger) != 32 || ledger[0]["case_id"] != "case-1" || ledger[31]["case_id"] != "case-32" {
		t.Fatal("overflow evicted evidence or exceeded the ledger bound", len(ledger))
	}
}

func gzipManifest(t *testing.T, manifest asset.TarParts, name string) []byte {
	t.Helper()
	var zipped bytes.Buffer
	writer := gzip.NewWriter(&zipped)
	writer.Name = name
	if err := json.NewEncoder(writer).Encode(manifest); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return zipped.Bytes()
}

func (h *proxyFixture) sendEncoded(path string, raw []byte, encoding string) *http.Response {
	h.t.Helper()
	request, err := http.NewRequestWithContext(h.t.Context(), "POST", h.down.URL+path, bytes.NewReader(raw))
	if err != nil {
		h.t.Fatal(err)
	}
	if encoding != "" {
		request.Header.Set("Content-Encoding", encoding)
	}
	response, err := h.client.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	return response
}

func TestManifestIdentityDriftIsRejectedBeforeForwarding(t *testing.T) {
	for _, mode := range []string{"canonical", "wire only"} {
		t.Run(mode, func(t *testing.T) {
			first, second := []byte("abc"), []byte("def")
			manifest := asset.TarParts{Format: asset.TarPartsFormat, Parts: []asset.TarPart{
				{SHA256: fmt.Sprintf("%x", sha256.Sum256(first)), Size: 3},
				{SHA256: fmt.Sprintf("%x", sha256.Sum256(second)), Size: 3},
			}}
			change := fixtureChange()
			change.Size, change.SHA256 = 6, fmt.Sprintf("%x", sha256.Sum256([]byte("abcdef")))
			op := asset.Fingerprint(change)
			var manifests atomic.Int32
			h := newProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/changes" {
					json.NewEncoder(w).Encode(asset.Ticket{ID: op, Upload: true})
					return
				}
				manifests.Add(1)
				json.NewEncoder(w).Encode(map[string]any{"missing": manifest.Parts})
			}), true)
			h.arm("commit", "before", false)
			response, err := h.request(t.Context(), "POST", "/v1/changes", change)
			if err != nil || response.StatusCode != 200 {
				t.Fatal("begin failed", err)
			}
			closeResponse(response)
			original := gzipManifest(t, manifest, "")
			response = h.sendEncoded("/v1/changes/"+op+"/parts", original, "gzip")
			if response.StatusCode != 200 {
				t.Fatal("initial manifest rejected", response.StatusCode)
			}
			closeResponse(response)
			changed := manifest
			changed.Parts = append([]asset.TarPart(nil), manifest.Parts...)
			name := "changed synthetic wrapper"
			if mode == "canonical" {
				changed.Parts[0], changed.Parts[1] = changed.Parts[1], changed.Parts[0]
				name = ""
			}
			if err := changed.Validate(change); err != nil {
				t.Fatal("drift fixture is not itself a valid manifest", err)
			}
			response = h.sendEncoded("/v1/changes/"+op+"/parts", gzipManifest(t, changed, name), "gzip")
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 409 || manifests.Load() != 1 {
				t.Fatalf("manifest drift was forwarded or accepted: status=%d calls=%d body=%s", response.StatusCode, manifests.Load(), body)
			}
			status := h.status()
			digest, _ := manifest.Digest()
			if status["consistent"] != false || status["manifest_digest"] != digest || status["wire_sha256"] != fmt.Sprintf("%x", sha256.Sum256(original)) {
				t.Fatalf("drift overwrote original identity or failed to mark inconsistency: %v", status)
			}
			ledger := h.adminJSON("GET", "/ledger", nil, nil)
			if bytes.Contains(ledger, []byte("changed synthetic wrapper")) || bytes.Contains(body, []byte("changed synthetic wrapper")) {
				t.Fatal("drift diagnostics exposed body metadata")
			}
		})
	}
}

func TestPublicationFramingBoundsRejectWithoutUpstreamMutation(t *testing.T) {
	for _, mode := range []string{"begin wire", "manifest wire", "manifest decoded", "part body"} {
		t.Run(mode, func(t *testing.T) {
			change := fixtureChange()
			op := asset.Fingerprint(change)
			var mutations atomic.Int32
			h := newProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mutations.Add(1)
				json.NewEncoder(w).Encode(asset.Ticket{ID: op, Upload: true})
			}), true)
			h.arm("commit", "before", false)
			if mode != "begin wire" {
				response, err := h.request(t.Context(), "POST", "/v1/changes", change)
				if err != nil || response.StatusCode != 200 {
					t.Fatal("begin failed", err)
				}
				closeResponse(response)
			}
			wantedMutations := mutations.Load()
			var response *http.Response
			wantedStatus := 413
			switch mode {
			case "begin wire":
				response = h.sendEncoded("/v1/changes", bytes.Repeat([]byte{'x'}, asset.MaxTarPartsManifestBytes+1), "")
			case "manifest wire":
				response = h.sendEncoded("/v1/changes/"+op+"/parts", bytes.Repeat([]byte{'x'}, asset.MaxTarPartsManifestBytes+1), "")
			case "manifest decoded":
				var encoded bytes.Buffer
				writer := gzip.NewWriter(&encoded)
				writer.Write(bytes.Repeat([]byte{' '}, asset.MaxTarPartsManifestBytes+1))
				writer.Close()
				response = h.sendEncoded("/v1/changes/"+op+"/parts", encoded.Bytes(), "gzip")
				wantedStatus = 409
			case "part body":
				raw := bytes.Repeat([]byte{'x'}, int(asset.MaxTarPartBytes)+1)
				var err error
				response, err = h.request(t.Context(), "PUT", "/v1/changes/"+op+"/parts/"+fmt.Sprintf("%x", sha256.Sum256(raw)), raw)
				if err != nil {
					t.Fatal(err)
				}
			}
			if response.StatusCode != wantedStatus {
				t.Fatalf("oversized %s returned HTTP %d instead of %d", mode, response.StatusCode, wantedStatus)
			}
			closeResponse(response)
			if mutations.Load() != wantedMutations {
				t.Fatal("oversized publication was forwarded upstream")
			}
		})
	}
}

func TestBeforeLimitThreeHoldsOnlyFirstAndThenForwardsFourth(t *testing.T) {
	change := fixtureChange()
	op := asset.Fingerprint(change)
	var posts atomic.Int32
	h := newProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		var received asset.Change
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil || asset.Fingerprint(received) != op {
			t.Error("fourth Begin changed its operation identity", err)
		}
		json.NewEncoder(w).Encode(asset.Ticket{ID: op, Upload: true})
	}), true)
	input := armInput("limit-three", fixtureTarget, 3)
	input["stage"], input["hold_first"] = "begin", true
	h.adminJSON("POST", "/arm", input, nil)
	done := make(chan error, 1)
	go func() {
		response, err := h.request(t.Context(), "POST", "/v1/changes", change)
		if err == nil && response.StatusCode != 503 {
			err = fmt.Errorf("first Begin returned HTTP %d", response.StatusCode)
		}
		closeResponse(response)
		done <- err
	}()
	if status := h.blocked(); status["hits"] != float64(1) || posts.Load() != 0 {
		t.Fatalf("first request did not hold before upstream: %v", status)
	}
	h.adminJSON("POST", "/release", map[string]any{}, nil)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for attempt := 2; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		response, err := h.request(ctx, "POST", "/v1/changes", change)
		if err != nil || response.StatusCode != 503 {
			cancel()
			t.Fatalf("Begin attempt %d held again or did not return 503: response=%v error=%v", attempt, response, err)
		}
		closeResponse(response)
		cancel()
		status := h.status()
		if status["blocked"] != false || status["hits"] != float64(attempt) || posts.Load() != 0 {
			t.Fatalf("attempt %d did not respect first-only hold and before boundary: %v posts=%d", attempt, status, posts.Load())
		}
	}
	response, err := h.request(t.Context(), "POST", "/v1/changes", change)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("fourth Begin did not forward normally: response=%v error=%v", response, err)
	}
	closeResponse(response)
	status := h.status()
	events, ok := status["events"].([]any)
	if !ok || len(events) != 4 || status["operation"] != op || status["hits"] != float64(3) || status["consistent"] != true || posts.Load() != 1 {
		t.Fatalf("limit changed identity, counted another fault, or reached upstream incorrectly: %v posts=%d", status, posts.Load())
	}
	for index, raw := range events {
		event := raw.(map[string]any)
		if event["stage"] != "begin" || event["operation"] != op || event["injected"] != (index < 3) {
			t.Fatalf("unexpected Begin ledger event %d: %v", index, event)
		}
	}
}

func TestAfterPartSuccessIsReusedWhenNextPrepareNoLongerReportsItMissing(t *testing.T) {
	first, other := []byte("selected uploaded body"), []byte("other still missing body")
	firstSHA, otherSHA := fmt.Sprintf("%x", sha256.Sum256(first)), fmt.Sprintf("%x", sha256.Sum256(other))
	manifest := asset.TarParts{Format: asset.TarPartsFormat, Parts: []asset.TarPart{
		{SHA256: firstSHA, Size: int64(len(first))}, {SHA256: otherSHA, Size: int64(len(other))},
	}}
	change := fixtureChange()
	change.Size = int64(len(first) + len(other))
	change.SHA256 = fmt.Sprintf("%x", sha256.Sum256(append(append([]byte{}, first...), other...)))
	op := asset.Fingerprint(change)
	wire := gzipManifest(t, manifest, "")
	var mu sync.Mutex
	stored, calls := map[string][]byte{}, map[string]int{}
	h := newProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/changes":
			json.NewEncoder(w).Encode(asset.Ticket{ID: op, Upload: true})
		case r.URL.Path == "/v1/changes/"+op+"/parts":
			raw, err := io.ReadAll(r.Body)
			if err != nil || !bytes.Equal(raw, wire) {
				t.Error("Prepare retry changed the gzip manifest", err)
			}
			missing := []asset.TarPart{}
			mu.Lock()
			for _, part := range manifest.Parts {
				if _, exists := stored[part.SHA256]; !exists {
					missing = append(missing, part)
				}
			}
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"missing": missing})
		case strings.HasPrefix(r.URL.Path, "/v1/changes/"+op+"/parts/"):
			digest := strings.TrimPrefix(r.URL.Path, "/v1/changes/"+op+"/parts/")
			raw, err := io.ReadAll(r.Body)
			if err != nil || digest != fmt.Sprintf("%x", sha256.Sum256(raw)) {
				t.Error("upstream did not receive the actual selected part bytes", err)
			}
			mu.Lock()
			stored[digest] = append([]byte(nil), raw...)
			calls[digest]++
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}), true)
	h.arm("part", "after", true)
	prepare := func() []asset.TarPart {
		t.Helper()
		response, err := h.request(t.Context(), "POST", "/v1/changes", change)
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("Begin retry failed: response=%v error=%v", response, err)
		}
		closeResponse(response)
		response = h.sendEncoded("/v1/changes/"+op+"/parts", wire, "gzip")
		if response.StatusCode != 200 {
			t.Fatal("Prepare retry was rejected as drift", response.StatusCode)
		}
		var result struct {
			Missing []asset.TarPart `json:"missing"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		closeResponse(response)
		return result.Missing
	}
	if missing := prepare(); len(missing) != 2 || missing[0].SHA256 != firstSHA {
		t.Fatal("fixture did not initially select the first missing digest", missing)
	}
	done := make(chan error, 1)
	go func() {
		response, err := h.request(t.Context(), "PUT", "/v1/changes/"+op+"/parts/"+firstSHA, first)
		closeResponse(response)
		done <- err
	}()
	status := h.blocked()
	mu.Lock()
	upstreamSucceeded := bytes.Equal(stored[firstSHA], first) && calls[firstSHA] == 1
	mu.Unlock()
	if !upstreamSucceeded || status["part_target"] != firstSHA || status["hits"] != float64(1) {
		t.Fatalf("after barrier preceded a real successful body store: %v stored=%t", status, upstreamSucceeded)
	}
	events := status["events"].([]any)
	last := events[len(events)-1].(map[string]any)
	if last["stage"] != "part" || last["upstream_status"] != float64(204) || last["injected"] != true {
		t.Fatal("after-part barrier did not follow a real upstream 204", last)
	}
	h.adminJSON("POST", "/release", map[string]any{}, nil)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "EOF") {
		t.Fatal("successful part acknowledgement was not dropped as EOF", err)
	}
	if missing := prepare(); len(missing) != 1 || missing[0].SHA256 != otherSHA {
		t.Fatal("next Prepare did not reuse the already successful body", missing)
	}
	response, err := h.request(t.Context(), "PUT", "/v1/changes/"+op+"/parts/"+otherSHA, other)
	if err != nil || response.StatusCode != 204 {
		t.Fatalf("remaining part failed: response=%v error=%v", response, err)
	}
	closeResponse(response)
	if missing := prepare(); len(missing) != 0 {
		t.Fatal("final Prepare unnecessarily requested uploaded bodies again", missing)
	}
	status = h.status()
	if status["part_target"] != firstSHA || status["operation"] != op || status["consistent"] != true || status["hits"] != float64(1) {
		t.Fatalf("shrinking missing set was misreported as drift or changed selected digest: %v", status)
	}
	mu.Lock()
	firstCalls, otherCalls := calls[firstSHA], calls[otherSHA]
	mu.Unlock()
	if firstCalls != 1 || otherCalls != 1 {
		t.Fatalf("acknowledgement loss caused duplicate body uploads: first=%d other=%d", firstCalls, otherCalls)
	}
}
