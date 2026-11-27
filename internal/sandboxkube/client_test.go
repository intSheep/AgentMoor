package sandboxkube

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
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
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

type clientFixture struct {
	client *Client
	server *httptest.Server
	config ClientConfig
	token  string
}

func newClientFixture(t *testing.T, handler http.HandlerFunc) clientFixture {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	base := t.TempDir()
	ca := filepath.Join(base, "ca.pem")
	token := filepath.Join(base, "token")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("synthetic-original-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := ClientConfig{URL: server.URL, CAFile: ca, TokenFile: token}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.http.CloseIdleConnections)
	return clientFixture{client, server, config, token}
}
func TestClientTLSRotationAndPodOperations(t *testing.T) {
	var lock sync.Mutex
	var methods, auths []string
	var deleted map[string]any
	fixture := newClientFixture(t, func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		defer lock.Unlock()
		methods = append(methods, r.Method)
		auths = append(auths, r.Header.Get("Authorization"))
		switch r.Method {
		case "GET":
			if r.URL.Path == "/api/v1/namespaces/workloads/pods/missing" {
				http.Error(w, "not found", 404)
				return
			}
			if r.URL.Path != "/api/v1/namespaces/workloads/pods/sandbox-pod" {
				t.Error("wrong GET route", r.URL.Path)
			}
			io.WriteString(w, `{"kind":"Pod","metadata":{"name":"sandbox-pod"}}`)
		case "POST":
			if r.URL.Path != "/api/v1/namespaces/workloads/pods" {
				t.Error("wrong POST route", r.URL.Path)
			}
			body, _ := io.ReadAll(r.Body)
			if !bytes.Equal(body, []byte(`{"kind":"Pod"}`)) {
				t.Error("create body changed")
			}
			if r.Header.Get("Content-Type") != "application/json" {
				t.Error("missing JSON content type")
			}
			w.WriteHeader(201)
			io.WriteString(w, `{"kind":"Pod"}`)
		case "DELETE":
			if r.URL.Path != "/api/v1/namespaces/workloads/pods/sandbox-pod" {
				t.Error("wrong DELETE route", r.URL.Path)
			}
			if err := json.NewDecoder(r.Body).Decode(&deleted); err != nil {
				t.Error(err)
			}
			w.WriteHeader(202)
		}
	})
	raw, found, err := fixture.client.Get(t.Context(), "workloads", "sandbox-pod")
	if err != nil || !found || !json.Valid(raw) {
		t.Fatal(raw, found, err)
	}
	replacement := fixture.token + ".new"
	os.WriteFile(replacement, []byte("synthetic-renewed-token\n"), 0600)
	if err := os.Rename(replacement, fixture.token); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.client.Create(t.Context(), "workloads", json.RawMessage(`{"kind":"Pod"}`)); err != nil {
		t.Fatal(err)
	}
	uid := uuid.NewString()
	if err := fixture.client.Delete(t.Context(), "workloads", "sandbox-pod", uid); err != nil {
		t.Fatal(err)
	}
	if raw, found, err := fixture.client.Get(t.Context(), "workloads", "missing"); err != nil || found || len(raw) != 0 {
		t.Fatal("404 must mean absent", raw, found, err)
	}
	lock.Lock()
	defer lock.Unlock()
	if strings.Join(methods, ",") != "GET,POST,DELETE,GET" || auths[0] != "Bearer synthetic-original-token" || auths[1] != "Bearer synthetic-renewed-token" || auths[2] != auths[1] {
		t.Fatal("token was cached or extra request occurred", methods)
	}
	preconditions, ok := deleted["preconditions"].(map[string]any)
	if !ok || preconditions["uid"] != uid || deleted["apiVersion"] != "v1" || deleted["kind"] != "DeleteOptions" || deleted["gracePeriodSeconds"] != float64(5) || len(deleted) != 4 {
		t.Fatal("delete omitted UID fence or grace period", deleted)
	}
}
func TestClientNeverFollowsRedirectsOrLeaksResponseBody(t *testing.T) {
	const sensitive = "synthetic-sensitive-response-value"
	var redirected atomic.Int64
	fixture := newClientFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/private-target" {
			redirected.Add(1)
			io.WriteString(w, `{"kind":"Pod"}`)
			return
		}
		w.Header().Set("Location", "/private-target?token="+sensitive)
		w.WriteHeader(307)
		io.WriteString(w, sensitive)
	})
	_, _, err := fixture.client.Get(t.Context(), "workloads", "pod")
	if err == nil || strings.Contains(err.Error(), sensitive) || redirected.Load() != 0 {
		t.Fatal("redirect/body escaped", err, redirected.Load())
	}
	for _, method := range []string{"create", "delete"} {
		var err error
		if method == "create" {
			_, err = fixture.client.Create(t.Context(), "workloads", json.RawMessage(`{}`))
		} else {
			err = fixture.client.Delete(t.Context(), "workloads", "pod", uuid.NewString())
		}
		if err == nil || strings.Contains(err.Error(), sensitive) || redirected.Load() != 0 {
			t.Fatal("mutation followed redirect", err)
		}
	}
}
func TestClientMutationLostAcknowledgementIsNotRetried(t *testing.T) {
	for _, method := range []string{"POST", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			var writes atomic.Int64
			fixture := newClientFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					io.WriteString(w, `{}`)
					return
				}
				writes.Add(1)
				io.Copy(io.Discard, r.Body)
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				connection.Close()
			})
			if _, _, err := fixture.client.Get(t.Context(), "workloads", "pod"); err != nil {
				t.Fatal("idle connection positive control", err)
			}
			var err error
			if method == "POST" {
				_, err = fixture.client.Create(t.Context(), "workloads", json.RawMessage(`{"kind":"Pod"}`))
			} else {
				err = fixture.client.Delete(t.Context(), "workloads", "pod", uuid.NewString())
			}
			if err == nil || writes.Load() != 1 {
				t.Fatal("ambiguous mutation was retried or accepted", err, writes.Load())
			}
		})
	}
}
func TestClientResponseBoundsAndMalformedJSON(t *testing.T) {
	for _, kind := range []string{"declared-overflow", "chunked-overflow", "malformed", "array", "null", "trailing-json", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			var requests atomic.Int64
			fixture := newClientFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch kind {
				case "declared-overflow":
					w.Header().Set("Content-Length", fmt.Sprint(maxResponseBytes+1))
					w.WriteHeader(200)
				case "chunked-overflow":
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					io.WriteString(w, strings.Repeat("x", maxResponseBytes+1))
				case "malformed":
					io.WriteString(w, "synthetic-private-bad-json")
				case "array":
					io.WriteString(w, `[]`)
				case "null":
					io.WriteString(w, `null`)
				case "trailing-json":
					io.WriteString(w, `{} {}`)
				case "conflict":
					w.WriteHeader(409)
					io.WriteString(w, "synthetic-private-conflict")
				}
			})
			_, _, err := fixture.client.Get(t.Context(), "workloads", "pod")
			if err == nil || strings.Contains(err.Error(), "synthetic-private") || requests.Load() != 1 {
				t.Fatal("bad response accepted or leaked", err, requests.Load())
			}
		})
	}
	fixture := newClientFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		io.WriteString(w, "sensitive-conflict-body")
	})
	if _, err := fixture.client.Create(t.Context(), "workloads", json.RawMessage(`{}`)); err == nil || strings.Contains(err.Error(), "sensitive-conflict-body") {
		t.Fatal("create409 was accepted or body leaked", err)
	}
}
func TestClientRejectsInvalidConfigurationAndAddresses(t *testing.T) {
	var requests atomic.Int64
	fixture := newClientFixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); io.WriteString(w, `{}`) })
	for _, origin := range []string{"http://example.test", "https://example.test/", "https://example.test/path", "https://example.test?token=sensitive", "https://example.test?", "https://user:sensitive@example.test", "https://example.test#sensitive", "https://example.test#", "https://example.test:", "https://example.test:0", "https://example.test:65536", "https://"} {
		config := fixture.config
		config.URL = origin
		if _, err := NewClient(config); err == nil || strings.Contains(err.Error(), "sensitive") {
			t.Fatal("invalid origin accepted or leaked", origin, err)
		}
	}
	for _, timeout := range []time.Duration{-1, 30*time.Second + 1} {
		config := fixture.config
		config.Timeout = timeout
		if _, err := NewClient(config); err == nil {
			t.Fatal("invalid timeout accepted")
		}
	}
	config := fixture.config
	config.TokenFile = ""
	if _, err := NewClient(config); err == nil {
		t.Fatal("missing token accepted")
	}
	badCA := filepath.Join(t.TempDir(), "bad-ca")
	os.WriteFile(badCA, []byte("sensitive-invalid-ca"), 0600)
	config = fixture.config
	config.CAFile = badCA
	if _, err := NewClient(config); err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("bad CA accepted or leaked", err)
	}
	for _, address := range [][2]string{{"Upper", "pod"}, {"workloads/escape", "pod"}, {strings.Repeat("a", 64), "pod"}, {"bad.namespace", "pod"}, {"workloads", "../pod"}, {"workloads", "UPPER"}, {"workloads", "a..b"}, {"workloads", strings.Repeat("a", 254)}, {"workloads", ""}, {"workloads", "pod?token=sensitive"}} {
		if _, _, err := fixture.client.Get(t.Context(), address[0], address[1]); err == nil {
			t.Fatal("invalid GET address accepted", address)
		}
		if err := fixture.client.Delete(t.Context(), address[0], address[1], uuid.NewString()); err == nil {
			t.Fatal("invalid DELETE address accepted", address)
		}
	}
	for _, uid := range []string{"", uuid.Nil.String(), "A1234567-1234-4234-8234-123456789ABC", "../uid"} {
		if err := fixture.client.Delete(t.Context(), "workloads", "pod", uid); err == nil {
			t.Fatal("invalid UID accepted", uid)
		}
	}
	for _, body := range []json.RawMessage{nil, json.RawMessage(`[]`), json.RawMessage(`{} {}`), json.RawMessage(strings.Repeat("x", maxResponseBytes+1))} {
		if _, err := fixture.client.Create(t.Context(), "workloads", body); err == nil {
			t.Fatal("bad create body accepted")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("rejected configuration contacted API", requests.Load())
	}
}
func TestClientPrivateFileBoundsAndRotationFailure(t *testing.T) {
	var requests atomic.Int64
	fixture := newClientFixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); io.WriteString(w, `{}`) })
	for name, data := range map[string][]byte{"oversized": bytes.Repeat([]byte("s"), maxTokenBytes+1), "empty": nil, "newline-injection": []byte("token\r\nX-Secret: secret"), "unicode": []byte("token-密码")} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "token")
			os.WriteFile(file, data, 0600)
			config := fixture.config
			config.TokenFile = file
			if _, err := NewClient(config); err == nil {
				t.Fatal("invalid token file accepted")
			}
		})
	}
	fifo := filepath.Join(t.TempDir(), "token-fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	config := fixture.config
	config.TokenFile = fifo
	finished := make(chan error, 1)
	go func() { _, err := NewClient(config); finished <- err }()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("FIFO token accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("token FIFO blocked before validation")
	}
	if err := os.WriteFile(fixture.token, []byte("changed\r\nheader-injection"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.client.Get(t.Context(), "workloads", "pod"); err == nil {
		t.Fatal("request reused cached token after invalid rotation")
	}
	if requests.Load() != 0 {
		t.Fatal("invalid tokens reached API")
	}
}
func TestClientTLSVerificationAndCancellation(t *testing.T) {
	fixture := newClientFixture(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) })
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(12345), Subject: pkix.Name{CommonName: "independent-wrong-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	wrongCA := filepath.Join(t.TempDir(), "wrong-ca")
	os.WriteFile(wrongCA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	config := fixture.config
	config.CAFile = wrongCA
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.http.CloseIdleConnections)
	if _, _, err := client.Get(t.Context(), "workloads", "pod"); err == nil || strings.Contains(err.Error(), fixture.server.URL) {
		t.Fatal("wrong CA accepted or raw URL leaked", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := fixture.client.Get(ctx, "workloads", "pod"); !errors.Is(err, context.Canceled) {
		t.Fatal("request cancellation identity lost", err)
	}
}
func TestClientTimeoutIsBoundedAndSanitized(t *testing.T) {
	release := make(chan struct{})
	fixture := newClientFixture(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) })
	if fixture.client.http.Timeout != 5*time.Second {
		t.Fatal("default timeout is not five seconds")
	}
	config := fixture.config
	config.Timeout = 25 * time.Millisecond
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.http.CloseIdleConnections)
	if _, _, err := client.Get(t.Context(), "workloads", "pod"); !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), fixture.server.URL) {
		t.Fatal("timeout identity lost or raw URL leaked", err)
	}
}
func TestClientDeleteStatusContract(t *testing.T) {
	for _, status := range []int{200, 202, 404, 204, 409, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int64
			fixture := newClientFixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(status) })
			err := fixture.client.Delete(t.Context(), "workloads", "pod", uuid.NewString())
			want := status == 200 || status == 202 || status == 404
			if (err == nil) != want || requests.Load() != 1 {
				t.Fatal("unexpected DELETE outcome or retry", status, err, requests.Load())
			}
		})
	}
}
