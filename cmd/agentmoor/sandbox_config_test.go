package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentmoor/internal/adapter"
	"agentmoor/internal/adapters/codex"
	"agentmoor/internal/asset"
	"agentmoor/internal/platform"
	"agentmoor/internal/sandboxgateway"
	"agentmoor/internal/sandboxkube"
)

func sandboxDeploymentFixture(t *testing.T) (sandboxDeployment, *adapter.Registry, *platform.Catalog, string, string, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(upstream.Close)
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "public-ca.pem"), filepath.Join(dir, "private-key.pem")
	cert := upstream.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		certPath:                        pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		keyPath:                         pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
		filepath.Join(dir, "token"):     []byte("fixture-control-plane-token"),
		filepath.Join(dir, "model-key"): []byte("fixture-shared-upstream-key"),
	} {
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := adapter.NewRegistry(codex.New())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := platform.OpenCatalog(t.Context(), t.TempDir(), "", platform.DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	config := sandboxDeployment{Version: "sandbox-deployment.v1", NetworkPolicy: "gateway-only.v1", GatewayListen: "127.0.0.1:7443"}
	config.Kubernetes.URL, config.Kubernetes.CAFile, config.Kubernetes.TokenFile, config.Kubernetes.ProbeAddress = upstream.URL, certPath, filepath.Join(dir, "token"), "10.96.0.1:443"
	config.Profiles = []sandboxAllocation{{Execution: sandboxkube.Profile{
		Scope:     asset.Scope{Tenant: "fixture", Project: "report", Runtime: "run-1", Adapter: "codex"},
		Namespace: "sandbox", Image: "registry.example/guest@sha256:" + strings.Repeat("a", 64), RuntimeClass: "runsc",
		GatewayURL: "https://gateway.example:7443", GatewayIP: "10.96.0.8", GatewayCAPath: "/etc/gateway/ca.crt",
		RuntimeExecutable: "/usr/local/bin/codex", Model: "fixture-model", FilesystemClass: "gvisor-overlay2-memory.v1",
		CPUMillis: 1000, MemoryMiB: 1024, WorkspaceMiB: 512, TempMiB: 128,
	}, Model: sandboxModelConfig{UpstreamURL: upstream.URL + "/v1/responses", APIKeyFile: filepath.Join(dir, "model-key"), CAFile: certPath,
		MaxRequests: 8, MaxInputBytes: 1 << 20, MaxOutputBytes: 2 << 20, PerRequestOutputBytes: 256 << 10, Concurrency: 1, MaxOutputTokens: 1024}}}
	return config, registry, catalog, certPath, keyPath, &requests
}

func writeSandboxDeployment(t *testing.T, config sandboxDeployment) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sandbox.json")
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSandboxDeploymentBindsTrustedProfiles(t *testing.T) {
	config, registry, catalog, _, _, requests := sandboxDeploymentFixture(t)
	runtime, err := newSandboxRuntime(t.Context(), writeSandboxDeployment(t, config), "0.0.0.0:7401", registry, catalog)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.gateway.Close()
	p := config.Profiles[0].Execution
	p.APIProbePort, p.KubeProbeAddress = 7401, config.Kubernetes.ProbeAddress
	fingerprint, err := p.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	selection, ok := runtime.selection(p.Scope)
	if !ok || selection.Fingerprint != fingerprint || selection.AdapterID != "codex" || selection.RuntimeVersion != codex.New().Descriptor().TestedRuntime {
		t.Fatalf("selection = %+v, present=%v", selection, ok)
	}
	p.Scope.Runtime = "another-runtime"
	if _, ok = runtime.selection(p.Scope); ok || requests.Load() != 0 {
		t.Fatal("missing allocation selected or startup contacted a remote service")
	}
}

func TestSandboxDeploymentRejectsPartialOrDriftingConfiguration(t *testing.T) {
	config, registry, catalog, _, _, requests := sandboxDeploymentFixture(t)
	for _, tc := range []struct {
		name   string
		change func(*sandboxDeployment)
		listen string
	}{
		{"missing-network-contract", func(c *sandboxDeployment) { c.NetworkPolicy = "" }, "127.0.0.1:7401"},
		{"missing-allocation", func(c *sandboxDeployment) { c.Profiles = nil }, "127.0.0.1:7401"},
		{"same-listener", func(c *sandboxDeployment) {}, "127.0.0.1:7443"},
		{"ephemeral-api", func(c *sandboxDeployment) {}, "127.0.0.1:0"},
		{"api-canary-drift", func(c *sandboxDeployment) { c.Profiles[0].Execution.APIProbePort = 7402 }, "127.0.0.1:7401"},
		{"kube-canary-drift", func(c *sandboxDeployment) { c.Profiles[0].Execution.KubeProbeAddress = "10.96.0.2:443" }, "127.0.0.1:7401"},
		{"missing-kube-canary", func(c *sandboxDeployment) { c.Kubernetes.ProbeAddress = "" }, "127.0.0.1:7401"},
		{"gateway-port-drift", func(c *sandboxDeployment) { c.Profiles[0].Execution.GatewayURL = "https://gateway.example:8443" }, "127.0.0.1:7401"},
		{"duplicate-scope", func(c *sandboxDeployment) { c.Profiles = append(c.Profiles, c.Profiles[0]) }, "127.0.0.1:7401"},
		{"unbounded-model", func(c *sandboxDeployment) { c.Profiles[0].Model.MaxRequests = 0 }, "127.0.0.1:7401"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := config
			copy.Profiles = append([]sandboxAllocation(nil), config.Profiles...)
			tc.change(&copy)
			if runtime, err := newSandboxRuntime(t.Context(), writeSandboxDeployment(t, copy), tc.listen, registry, catalog); err == nil {
				runtime.gateway.Close()
				t.Fatal("invalid deployment accepted")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("invalid deployment sent a Kubernetes or model request")
	}
}

func TestSandboxConfigurationJSONIsCanonicalAndBounded(t *testing.T) {
	for _, raw := range []string{`{"version":"a","version":"b"}`, `{"Version":"a"}`, `{"version":null}`, `{"unknown":1}`, `{} {}`, `{"profiles":[` + strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40) + "]}"} {
		var config sandboxDeployment
		if err := sandboxConfigJSON([]byte(raw), &config); err == nil {
			t.Fatalf("accepted malformed configuration %.80q", raw)
		}
	}
}

type listenerAnnouncements chan string

func (c listenerAnnouncements) Write(raw []byte) (int, error) {
	c <- strings.TrimPrefix(strings.TrimSpace(string(raw)), "listening ")
	return len(raw), nil
}

func TestSandboxDedicatedTLSListenerAndJointShutdown(t *testing.T) {
	config, registry, catalog, certPath, keyPath, _ := sandboxDeploymentFixture(t)
	runtime, err := newSandboxRuntime(t.Context(), writeSandboxDeployment(t, config), "127.0.0.1:7401", registry, catalog)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.gateway.Close()
	api := &http.Server{Addr: "127.0.0.1:0", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "private-main-api") })}
	gw := sandboxgateway.NewTLSServer("127.0.0.1:0", runtime.gateway)
	announcements := make(listenerAnnouncements, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveHTTPServers(ctx, announcements, certPath, keyPath, api, gw) }()
	addresses := []string{}
	for range 2 {
		select {
		case address := <-announcements:
			addresses = append(addresses, address)
		case <-time.After(5 * time.Second):
			t.Fatal("listeners did not start")
		}
	}
	ca, _ := os.ReadFile(certPath)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}, Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	for i, address := range addresses {
		response, err := client.Get("https://" + address + "/v1/tasks")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if i == 0 && string(raw) != "private-main-api" || i == 1 && (response.StatusCode != 404 || strings.Contains(string(raw), "private-main-api")) {
			t.Fatalf("listener %d leaked API: %d %q", i, response.StatusCode, raw)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("joint shutdown did not finish")
	}
	for _, address := range addresses {
		if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
			conn.Close()
			t.Fatal("listener survived shutdown")
		}
	}
}

func TestHTTPListenersFailTogetherBeforeServing(t *testing.T) {
	available, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	apiAddress := available.Addr().String()
	available.Close()
	blocked, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	var called atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Add(1) })
	announcements := make(listenerAnnouncements, 2)
	err = serveHTTPServers(t.Context(), announcements, "", "", &http.Server{Addr: apiAddress, Handler: handler}, &http.Server{Addr: blocked.Addr().String(), Handler: handler})
	if err == nil || len(announcements) != 0 || called.Load() != 0 {
		t.Fatal("partial startup was announced or served")
	}
	if conn, err := net.DialTimeout("tcp", apiAddress, time.Second); err == nil {
		conn.Close()
		t.Fatal("API listener survived gateway bind failure")
	}
}

func TestSandboxServeRequiresTLSBeforeOpeningPrivateState(t *testing.T) {
	var out strings.Builder
	err := run(t.Context(), []string{"serve", "--data", filepath.Join(t.TempDir(), "missing-private-state"), "--sandbox-config", "missing-config.json"}, &out)
	if err == nil || !strings.Contains(err.Error(), "requires API and gateway TLS") || out.Len() != 0 {
		t.Fatalf("unexpected partial startup: %v, %q", err, out.String())
	}
}
