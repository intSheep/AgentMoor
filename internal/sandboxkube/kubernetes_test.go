//go:build integration

package sandboxkube

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/platform"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

const controllerGatewayName = "controller-gateway.agentmoor.test"
const controllerFixtureOutput = "synthetic-controller:sealed-result"

type controllerKubeCounts struct {
	Posts   int `json:"posts"`
	Gets    int `json:"gets"`
	Deletes int `json:"deletes"`
}

type controllerKubeCounter struct {
	base      http.RoundTripper
	namespace string
	mu        sync.Mutex
	counts    controllerKubeCounts
}

func (c *controllerKubeCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	prefix := "/api/v1/namespaces/" + c.namespace + "/pods"
	if r.URL.RawQuery != "" || (r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/")) {
		return nil, errors.New("controller fixture rejected an unexpected API route")
	}
	c.mu.Lock()
	switch r.Method {
	case http.MethodPost:
		c.counts.Posts++
	case http.MethodGet:
		c.counts.Gets++
	case http.MethodDelete:
		c.counts.Deletes++
	default:
		c.mu.Unlock()
		return nil, errors.New("controller fixture rejected an unexpected API method")
	}
	c.mu.Unlock()
	return c.base.RoundTrip(r)
}

func (c *controllerKubeCounter) snapshot() controllerKubeCounts {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts
}

func (c *controllerKubeCounter) CloseIdleConnections() {
	if closer, ok := c.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (c controllerKubeCounts) since(before controllerKubeCounts) controllerKubeCounts {
	return controllerKubeCounts{c.Posts - before.Posts, c.Gets - before.Gets, c.Deletes - before.Deletes}
}

type controllerGatewayRun struct {
	mode         string
	entered      chan struct{}
	once         sync.Once
	mu           sync.Mutex
	inputs, puts int
}

func (r *controllerGatewayRun) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inputs, r.puts
}

type controllerKubeGateway struct {
	catalog   *platform.Catalog
	directory string
	mu        sync.Mutex
	runs      map[string]*controllerGatewayRun
	active    *controllerGatewayRun
	forbidden int
}

func (g *controllerKubeGateway) register(id, mode string) *controllerGatewayRun {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := &controllerGatewayRun{mode: mode, entered: make(chan struct{})}
	g.runs[id] = r
	g.active = r
	return r
}

func (g *controllerKubeGateway) find(id string) *controllerGatewayRun {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runs[id]
}

func (g *controllerKubeGateway) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Count attempts independently of successful authorization. In particular,
	// a cancelled or missing-handler fixture must not hide a PUT/GET behind 403.
	// Modes run sequentially and each Pod is observed gone before the next one.
	g.mu.Lock()
	active := g.active
	g.mu.Unlock()
	if active != nil {
		active.mu.Lock()
		switch req.Method + " " + req.URL.Path {
		case "GET /input":
			active.inputs++
		case "PUT /output":
			active.puts++
		}
		active.mu.Unlock()
	}
	token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok || req.URL.RawQuery != "" {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	switch req.Method + " " + req.URL.Path {
	case "GET /input":
		r, err := g.catalog.AuthenticateSandboxCapability(req.Context(), token, platform.SandboxInput)
		if err != nil {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		run := g.find(r.ID)
		if run == nil || run.mode == "missing" {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		blob, stream, err := g.catalog.ReadSandboxInput(req.Context(), token)
		if err != nil {
			http.Error(w, "unavailable", http.StatusForbidden)
			return
		}
		defer stream.Close()
		run.once.Do(func() { close(run.entered) })
		if err := controllerWaitRelease(req.Context(), g.directory, run.mode); err != nil {
			http.Error(w, "observation expired", http.StatusRequestTimeout)
			return
		}
		if _, err := g.catalog.AuthenticateSandboxCapability(req.Context(), token, platform.SandboxInput); err != nil {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Length", strconv.FormatInt(blob.Size, 10))
		_, _ = io.Copy(w, stream)
	case "PUT /output":
		r, err := g.catalog.AuthenticateSandboxCapability(req.Context(), token, platform.SandboxOutput)
		if err != nil {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		run := g.find(r.ID)
		if run == nil || run.mode != "success" {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		blob := controllerKubeBlob(controllerFixtureOutput)
		if req.Header.Get("X-Agentmoor-SHA256") != blob.SHA256 || req.Header.Get("X-Agentmoor-Size") != strconv.FormatInt(blob.Size, 10) || req.ContentLength != blob.Size {
			http.Error(w, "invalid frozen output", http.StatusBadRequest)
			return
		}
		if _, err := g.catalog.SealSandboxOutput(req.Context(), token, blob, req.Body); err != nil {
			http.Error(w, "output denied", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		g.mu.Lock()
		g.forbidden++
		g.mu.Unlock()
		http.Error(w, "route denied", http.StatusNotFound)
	}
}

type controllerKubeMetric struct {
	Mode           string               `json:"mode"`
	RunID          string               `json:"run_id"`
	PodName        string               `json:"pod_name"`
	PodUID         string               `json:"pod_uid"`
	Namespace      string               `json:"namespace"`
	RuntimeClass   string               `json:"runtime_class"`
	ObservedPhase  string               `json:"observed_phase"`
	State          string               `json:"state"`
	Requests       controllerKubeCounts `json:"requests"`
	InputGETs      int                  `json:"input_gets"`
	OutputPUTs     int                  `json:"output_puts"`
	PodDeleted     bool                 `json:"pod_deleted"`
	CapsDenied     bool                 `json:"caps_denied"`
	OutputVerified bool                 `json:"output_verified"`
	Passed         bool                 `json:"passed"`
}

func controllerKubeBlob(value string) platform.SandboxBlob {
	h := sha256.Sum256([]byte(value))
	return platform.SandboxBlob{SHA256: hex.EncodeToString(h[:]), Size: int64(len(value))}
}

func controllerWait(ctx context.Context, timeout time.Duration, check func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if done, err := check(); done || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func controllerWaitRelease(ctx context.Context, directory, mode string) error {
	return controllerWait(ctx, 60*time.Second, func() (bool, error) {
		info, err := os.Lstat(filepath.Join(directory, mode+".release"))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1024 {
			return false, errors.New("invalid controller observation release")
		}
		return true, nil
	})
}

func controllerKubeEvent(stage string, metric controllerKubeMetric) {
	// This deliberately excludes capabilities, raw Pod metadata and errors.
	b, _ := json.Marshal(struct {
		Stage     string `json:"stage"`
		Mode      string `json:"mode"`
		PodName   string `json:"pod_name"`
		PodUID    string `json:"pod_uid"`
		RunID     string `json:"run_id"`
		Namespace string `json:"namespace"`
	}{stage, metric.Mode, metric.PodName, metric.PodUID, metric.RunID, metric.Namespace})
	fmt.Printf("SANDBOX_CONTROLLER_EVENT %s\n", b)
}

func controllerKubeTask(t *testing.T, ctx context.Context, api *httptest.Server, secret []byte, scope asset.Scope, method, suffix string, value any) task.Record {
	t.Helper()
	token, err := platform.Issue(secret, platform.Grant{Scope: scope, Read: true, Write: true, Submit: true, Execute: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal("could not issue a host-only task fixture grant")
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal("could not encode a public task fixture request")
	}
	req, err := http.NewRequestWithContext(ctx, method, api.URL+"/v1/tasks/"+scope.Runtime+suffix, bytes.NewReader(body))
	if err != nil {
		t.Fatal("could not construct a public task fixture request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := api.Client().Do(req)
	if err != nil {
		t.Fatal("public task fixture request failed")
	}
	defer res.Body.Close()
	var record task.Record
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 128<<10)).Decode(&record) != nil {
		t.Fatal("public task fixture rejected submit or claim")
	}
	return record
}

func TestSandboxControllerKubernetes(t *testing.T) {
	const prefix = "AGENTMOOR_CONTROLLER_"
	if os.Getenv(prefix+"API") == "" {
		t.Skip("actual Kubernetes controller fixture is not configured")
	}
	values := make(map[string]string)
	for _, key := range []string{"API", "KUBE_CA", "TOKEN_FILE", "NAMESPACE", "IMAGE", "GATEWAY_IP", "GATEWAY_CA", "GATEWAY_KEY", "DIRECTORY"} {
		values[key] = os.Getenv(prefix + key)
		if values[key] == "" {
			t.Fatalf("controller integration fixture is missing %s", key)
		}
	}
	directory := values["DIRECTORY"]
	info, err := os.Lstat(directory)
	if !filepath.IsAbs(directory) || err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || net.ParseIP(values["GATEWAY_IP"]).To4() == nil {
		t.Fatal("controller integration fixture requires an owned private directory and reachable IPv4")
	}
	for _, mode := range []string{"success", "cancel", "missing"} {
		if _, err := os.Lstat(filepath.Join(directory, mode+".release")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("controller observation release already exists")
		}
	}
	if _, err := os.Lstat(filepath.Join(directory, "result.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("controller integration result already exists")
	}
	catalog, err := platform.OpenCatalog(t.Context(), t.TempDir(), "", platform.DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal("could not open the real host-side catalog")
	}
	t.Cleanup(func() { catalog.Close() })
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal("could not initialize the host-only task fixture key")
	}
	api := httptest.NewServer((&platform.API{Catalog: catalog, Secret: secret, Temp: t.TempDir(), TaskAdapters: map[string]bool{"codex": true}}).Handler())
	t.Cleanup(api.Close)
	gateway := &controllerKubeGateway{catalog: catalog, directory: directory, runs: make(map[string]*controllerGatewayRun)}
	certificate, err := tls.LoadX509KeyPair(values["GATEWAY_CA"], values["GATEWAY_KEY"])
	if err != nil {
		t.Fatal("could not load the host-only gateway TLS fixture")
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal("could not bind the host gateway TLS fixture")
	}
	server := &http.Server{Handler: gateway, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	t.Cleanup(func() { server.Close() })
	go func() {
		_ = server.Serve(tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}))
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	config := ClientConfig{URL: values["API"], CAFile: values["KUBE_CA"], TokenFile: values["TOKEN_FILE"]}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal("could not initialize the minimum-permission production Kubernetes client")
	}
	t.Cleanup(client.http.CloseIdleConnections)
	counter := &controllerKubeCounter{base: client.http.Transport, namespace: values["NAMESPACE"]}
	client.http.Transport = counter
	var metrics []controllerKubeMetric
	t.Cleanup(func() {
		body, err := json.MarshalIndent(struct {
			ComponentOnly     bool                   `json:"component_only"`
			NoNativeExecution bool                   `json:"no_native_execution"`
			Passed            bool                   `json:"passed"`
			Modes             []controllerKubeMetric `json:"modes"`
		}{true, true, !t.Failed(), metrics}, "", "  ")
		if err != nil {
			t.Error("could not encode sanitized controller evidence")
			return
		}
		file, err := os.OpenFile(filepath.Join(directory, "result.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Error("could not create sanitized controller evidence")
			return
		}
		_, writeErr := file.Write(append(body, '\n'))
		if errors.Join(writeErr, file.Sync(), file.Close()) != nil {
			t.Error("could not persist sanitized controller evidence")
		}
	})
	for _, mode := range []string{"success", "cancel", "missing"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			before := counter.snapshot()
			metric := controllerKubeMetric{Mode: mode, Namespace: values["NAMESPACE"], RuntimeClass: "agentmoor-gvisor"}
			defer func() {
				metric.Requests = counter.snapshot().since(before)
				metric.Passed = !t.Failed()
				metrics = append(metrics, metric)
			}()
			if mode == "missing" {
				metric.RuntimeClass = "agentmoor-missing"
			}
			scope := asset.Scope{Tenant: "controller-fixture", Project: "actual-kubernetes", Runtime: "component-" + mode + "-" + uuid.NewString(), Adapter: "codex"}
			profile := Profile{Scope: scope, Namespace: values["NAMESPACE"], Image: values["IMAGE"], RuntimeClass: metric.RuntimeClass,
				GatewayURL: "https://" + net.JoinHostPort(controllerGatewayName, strconv.Itoa(port)), GatewayIP: values["GATEWAY_IP"], GatewayCAPath: "/etc/agentmoor/fixture-ca.pem",
				RuntimeExecutable: "/usr/local/bin/fixture-native-disabled", Model: "synthetic-controller-fixture", FilesystemClass: "gvisor-overlay2-memory.v1",
				CPUMillis: 1000, MemoryMiB: 1024, WorkspaceMiB: 512, TempMiB: 128}
			controllerKubeTask(t, ctx, api, secret, scope, http.MethodPut, "", task.Spec{Instruction: "synthetic controller component acceptance", Output: "report.txt"})
			a := platform.SandboxAddress{Scope: scope, Attempt: uuid.NewString()}
			controllerKubeTask(t, ctx, api, secret, scope, http.MethodPost, "/claim", task.Claim{Attempt: a.Attempt})
			input := "synthetic-controller:success"
			if mode != "success" {
				input = "synthetic-controller:hold"
			}
			fingerprint, err := profile.Fingerprint()
			if err != nil {
				t.Fatal("actual controller fixture profile is invalid")
			}
			r, err := catalog.BeginSandbox(ctx, a, fingerprint, controllerKubeBlob(input))
			if err != nil {
				t.Fatal("could not freeze the real claimed task")
			}
			metric.RunID = r.ID
			gate := gateway.register(r.ID, mode)
			if _, err := catalog.SealSandboxInput(ctx, a, strings.NewReader(input)); err != nil {
				t.Fatal("could not seal the actual input fixture")
			}
			newController := func() *Controller {
				t.Helper()
				c, err := New(config, []Profile{profile}, catalog)
				if err != nil {
					t.Fatal("could not initialize the production controller")
				}
				c.client.http.CloseIdleConnections()
				c.client = client // Keep the real TLS transport and POST counter across restarts.
				return c
			}
			controller := newController()
			defer controllerKubeCleanup(t, controller, a)
			r, err = controller.Start(ctx, a)
			if err != nil || r.PodUID == "" || r.PodTemplateSHA == "" {
				t.Fatal("production controller could not create and bind the actual Pod")
			}
			metric.PodName, metric.PodUID = r.PodName, r.PodUID
			var caps platform.SandboxCapabilities
			if err := controllerWait(ctx, 60*time.Second, func() (bool, error) {
				raw, exists, err := client.Get(ctx, profile.Namespace, r.PodName)
				if err != nil || !exists {
					return false, errors.New("actual controller Pod could not be observed")
				}
				pod, observedCaps, err := inspectPod(r, raw)
				if err != nil || pod.uid != metric.PodUID {
					return false, errors.New("actual Pod changed UID or frozen template")
				}
				value, err := object(raw)
				if err != nil {
					return false, errors.New("invalid actual Pod response")
				}
				status, _ := value["status"].(map[string]any)
				phase, _ := status["phase"].(string)
				caps = observedCaps
				if mode == "missing" {
					spec, _ := value["spec"].(map[string]any)
					node, _ := spec["nodeName"].(string)
					metric.ObservedPhase = phase
					return phase == "Pending" && node != "", nil
				}
				if phase == "Failed" || phase == "Succeeded" {
					return false, errors.New("fixture Pod terminated before the observation barrier")
				}
				metric.ObservedPhase = phase
				return phase == "Running", nil
			}); err != nil {
				t.Fatal("actual Pod did not reach its required observation phase")
			}
			if mode != "missing" {
				select {
				case <-gate.entered:
				case <-ctx.Done():
					t.Fatal("running fixture did not enter the authenticated gateway")
				case <-time.After(60 * time.Second):
					t.Fatal("running fixture did not enter the authenticated gateway")
				}
			}
			var group sync.WaitGroup
			for range 4 {
				group.Go(func() {
					current, err := controller.Start(ctx, a)
					if err != nil || current.PodUID != metric.PodUID {
						t.Error("concurrent Start changed the actual Pod identity")
					}
				})
			}
			group.Wait()
			controller = newController()
			for range 2 {
				if current, err := controller.Start(ctx, a); err != nil || current.PodUID != metric.PodUID {
					t.Fatal("restarted controller changed the actual Pod identity")
				}
			}
			if counter.snapshot().since(before).Posts != 1 {
				t.Fatal("actual controller issued more than one create POST")
			}
			controllerKubeEvent("observe", metric)
			if err := controllerWaitRelease(ctx, directory, mode); err != nil {
				t.Fatal("external runtime observation did not release the fixture")
			}
			if mode != "success" {
				if _, err := controller.Cancel(ctx, a); err != nil {
					t.Fatal("production controller could not begin cancellation")
				}
			}
			if err := controllerWait(ctx, 60*time.Second, func() (bool, error) {
				current, err := controller.Reconcile(ctx, a)
				if err != nil {
					return false, errors.New("actual controller reconciliation failed")
				}
				r = current
				if mode == "success" && stopped(r) && r.State != platform.SandboxReady {
					return false, errors.New("actual fixture Pod failed before publication readiness")
				}
				return r.PodDeleted && (mode != "success" || r.State == platform.SandboxReady), nil
			}); err != nil {
				t.Fatal("actual Pod did not reach observed deletion and expected terminal state")
			}
			metric.State, metric.PodDeleted = r.State, r.PodDeleted
			for purpose, token := range map[platform.SandboxPurpose]string{platform.SandboxInput: caps.Input, platform.SandboxOutput: caps.Output, platform.SandboxModel: caps.Model} {
				if _, err := catalog.AuthenticateSandboxCapability(ctx, token, purpose); !errors.Is(err, platform.ErrSandboxDenied) {
					t.Fatal("deleted actual Pod retained a capability")
				}
			}
			metric.CapsDenied = true
			metric.InputGETs, metric.OutputPUTs = gate.counts()
			if mode == "success" {
				blob, stream, err := catalog.ReadSandboxOutput(ctx, a)
				if err != nil {
					t.Fatal("ready actual fixture result was not readable")
				}
				raw, readErr := io.ReadAll(io.LimitReader(stream, int64(len(controllerFixtureOutput))+1))
				closeErr := stream.Close()
				if errors.Join(readErr, closeErr) != nil || string(raw) != controllerFixtureOutput || !blob.Sealed || blob.SHA256 != controllerKubeBlob(controllerFixtureOutput).SHA256 || metric.InputGETs < 1 || metric.OutputPUTs < 1 {
					t.Fatal("actual fixture output did not match its sealed bytes")
				}
				metric.OutputVerified = true
			} else {
				if r.State != platform.SandboxCancelled || r.Output.Sealed || metric.OutputPUTs != 0 || (mode == "missing" && metric.InputGETs != 0) {
					t.Fatal("cancelled or missing-handler Pod executed or produced output")
				}
				if _, stream, err := catalog.ReadSandboxOutput(ctx, a); err == nil || stream != nil {
					if stream != nil {
						stream.Close()
					}
					t.Fatal("cancelled or missing-handler result was exposed")
				}
			}
			if current, err := controller.Start(ctx, a); err != nil || current.PodUID != metric.PodUID || !current.PodDeleted || counter.snapshot().since(before).Posts != 1 {
				t.Fatal("terminal retry recreated or changed the actual Pod")
			}
			metric.Passed = !t.Failed()
			controllerKubeEvent("done", metric)
		})
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if gateway.forbidden != 0 {
		t.Error("fixture reached an unimplemented model or platform gateway route")
	}
}

func controllerKubeCleanup(t *testing.T, controller *Controller, a platform.SandboxAddress) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := controller.load(ctx, a)
	if err != nil || r.PodDeleted || !r.CreateStarted {
		return
	}
	_, _ = controller.Cancel(ctx, a)
	if err := controllerWait(ctx, 25*time.Second, func() (bool, error) {
		r, err := controller.Reconcile(ctx, a)
		return err == nil && r.PodDeleted, nil
	}); err != nil {
		t.Error("actual fixture Pod cleanup did not finish; operator cleanup is required")
	}
}
