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
	"reflect"
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

// This is a real production Controller/Catalog against the supplied owned
// Kubernetes API, with a synthetic guest and host-only TLS input fixture.
// Python injects the node fault and observes the runtime independently.
type nodeLossMetric struct {
	Mode                      string               `json:"mode"`
	RunID                     string               `json:"run_id"`
	PodName                   string               `json:"pod_name"`
	PodUID                    string               `json:"pod_uid"`
	Namespace                 string               `json:"namespace"`
	TurnID                    string               `json:"turn_id"`
	AttemptSHA256             string               `json:"attempt_sha256"`
	OriginalDeadline          time.Time            `json:"original_deadline"`
	State                     string               `json:"state"`
	PodTerminal               bool                 `json:"pod_terminal"`
	PodDeleted                bool                 `json:"pod_deleted"`
	APINotFound               bool                 `json:"api_not_found"`
	CapabilitiesDenied        bool                 `json:"capabilities_denied"`
	CheckpointUnchanged       bool                 `json:"checkpoint_unchanged"`
	IdentityDeadlineUnchanged bool                 `json:"identity_deadline_unchanged"`
	Requests                  controllerKubeCounts `json:"requests"`
	StatusHistory             []nodeLossPodStatus  `json:"status_history"`
}

// Keep only status/identity fields, never Pod annotations, environment,
// termination message contents, or raw HTTP request/response bodies.
type nodeLossPodStatus struct {
	UID               string                    `json:"uid"`
	ResourceVersion   string                    `json:"resource_version"`
	NodeName          string                    `json:"node_name"`
	Phase             string                    `json:"phase"`
	DeletionTimestamp string                    `json:"deletion_timestamp"`
	Containers        []nodeLossContainerStatus `json:"containers"`
}
type nodeLossContainerStatus struct {
	Name         string `json:"name"`
	ContainerID  string `json:"containerID"`
	RestartCount *int   `json:"restartCount"`
	State        struct {
		Running *struct {
			StartedAt string `json:"startedAt"`
		} `json:"running,omitempty"`
		Terminated *struct {
			ExitCode    *int   `json:"exitCode"`
			ContainerID string `json:"containerID"`
			StartedAt   string `json:"startedAt"`
			FinishedAt  string `json:"finishedAt"`
		} `json:"terminated,omitempty"`
	} `json:"state"`
}
type nodeLossStatusTrace struct {
	base http.RoundTripper
	mu   sync.Mutex
	rows map[string][]nodeLossPodStatus
}

func (s *nodeLossStatusTrace) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := s.base.RoundTrip(req)
	if err != nil || req.Method != http.MethodGet || res.StatusCode != http.StatusOK {
		return res, err
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	res.Body.Close()
	if err != nil || len(raw) > maxResponseBytes {
		return nil, errors.New("bounded node-loss status observation failed")
	}
	res.Body = io.NopCloser(bytes.NewReader(raw))
	var pod struct {
		Metadata struct {
			UID               string `json:"uid"`
			ResourceVersion   string `json:"resourceVersion"`
			DeletionTimestamp string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
		Status struct {
			Phase      string                    `json:"phase"`
			Containers []nodeLossContainerStatus `json:"containerStatuses"`
		} `json:"status"`
	}
	if json.Unmarshal(raw, &pod) == nil && pod.Metadata.UID != "" {
		row := nodeLossPodStatus{pod.Metadata.UID, pod.Metadata.ResourceVersion, pod.Spec.NodeName, pod.Status.Phase, pod.Metadata.DeletionTimestamp, pod.Status.Containers}
		s.mu.Lock()
		rows := append(s.rows[row.UID], row)
		if len(rows) > 32 {
			rows = rows[len(rows)-32:]
		}
		s.rows[row.UID] = rows
		s.mu.Unlock()
	}
	return res, nil
}
func (s *nodeLossStatusTrace) history(uid string) []nodeLossPodStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]nodeLossPodStatus{}, s.rows[uid]...)
}
func (s *nodeLossStatusTrace) CloseIdleConnections() {
	if c, ok := s.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

func nodeLossEvent(stage string, metric nodeLossMetric) {
	raw, _ := json.Marshal(struct {
		Stage  string         `json:"stage"`
		Metric nodeLossMetric `json:"metric"`
	}{stage, metric})
	fmt.Println("SANDBOX_NODE_LOSS_EVENT " + string(raw))
}

func nodeLossWaitFile(t *testing.T, ctx context.Context, dir, name string) {
	t.Helper()
	if err := controllerWait(ctx, 90*time.Second, func() (bool, error) {
		f, err := os.Lstat(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil || !f.Mode().IsRegular() || f.Size() > 4096 {
			return false, errors.New("invalid node-loss coordination file")
		}
		return true, nil
	}); err != nil {
		t.Fatal("node-loss observer did not complete " + name)
	}
}

func nodeLossBaseline(t *testing.T, ctx context.Context, api *httptest.Server, secret []byte, scope asset.Scope) (*platform.Client, task.Checkpoint, string) {
	t.Helper()
	token, err := platform.Issue(secret, platform.Grant{Scope: scope, Read: true, Write: true, Submit: true, Execute: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal("could not issue the host-only baseline grant")
	}
	client, err := platform.NewClient(api.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	route := "/v1/tasks/" + scope.Runtime
	var row task.Record
	if err = client.JSON(ctx, "PUT", route, task.Spec{Instruction: "synthetic successful checkpoint", Output: "report.txt"}, &row); err != nil {
		t.Fatal(err)
	}
	attempt := uuid.NewString()
	if err = client.JSON(ctx, "POST", route+"/claim", task.Claim{Attempt: attempt}, &row); err != nil {
		t.Fatal(err)
	}
	session := "ses_synthetic_node_loss_" + uuid.NewString()
	for _, key := range []asset.Key{{Kind: "artifact", ID: "out/report.txt"}, {Kind: "workspace", ID: "root"}, {Kind: "session", ID: session}} {
		body := []byte("synthetic checkpoint " + key.Kind)
		sum := sha256.Sum256(body)
		file := filepath.Join(t.TempDir(), "bytes")
		if err = os.WriteFile(file, body, 0600); err != nil {
			t.Fatal(err)
		}
		change := asset.Change{Metadata: asset.Metadata{Key: key, AdapterVersion: "synthetic-fixture.v1", Format: "synthetic.v1", MediaType: "text/plain", Name: "synthetic-checkpoint", SessionID: session, Origin: "runtime", Completeness: "persisted-files", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}}
		if _, err = client.Publish(ctx, change, file); err != nil {
			t.Fatal(err)
		}
	}
	if err = client.JSON(ctx, "POST", route+"/finish", task.Finish{Attempt: attempt, SessionID: session}, &row); err != nil {
		t.Fatal(err)
	}
	var cp task.Checkpoint
	if err = client.JSON(ctx, "GET", route+"/checkpoint", nil, &cp); err != nil {
		t.Fatal(err)
	}
	return client, cp, session
}

func TestSandboxNodeLossKubernetes(t *testing.T) {
	if os.Getenv("AGENTMOOR_NODE_LOSS") == "" {
		t.Skip("owned node-loss fixture is not configured")
	}
	const prefix = "AGENTMOOR_CONTROLLER_"
	env := map[string]string{}
	for _, key := range []string{"API", "KUBE_CA", "TOKEN_FILE", "NAMESPACE", "IMAGE", "GATEWAY_IP", "GATEWAY_CA", "GATEWAY_KEY", "DIRECTORY"} {
		env[key] = os.Getenv(prefix + key)
		if env[key] == "" {
			t.Fatal("missing owned node-loss fixture " + key)
		}
	}
	dir := env["DIRECTORY"]
	info, err := os.Lstat(dir)
	if !filepath.IsAbs(dir) || err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("node-loss directory is not private")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	catalog, err := platform.OpenCatalog(ctx, t.TempDir(), "", platform.DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { catalog.Close() })
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer((&platform.API{Catalog: catalog, Secret: secret, Temp: t.TempDir(), TaskAdapters: map[string]bool{"codex": true}, TurnEnabled: func(asset.Scope) bool { return true }}).Handler())
	t.Cleanup(api.Close)
	gateway := &controllerKubeGateway{catalog: catalog, directory: dir, runs: map[string]*controllerGatewayRun{}}
	certificate, err := tls.LoadX509KeyPair(env["GATEWAY_CA"], env["GATEWAY_KEY"])
	if err != nil {
		t.Fatal("gateway TLS fixture unavailable")
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: gateway, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	t.Cleanup(func() { server.Close() })
	go func() {
		_ = server.Serve(tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}))
	}()
	client, err := NewClient(ClientConfig{URL: env["API"], CAFile: env["KUBE_CA"], TokenFile: env["TOKEN_FILE"]})
	if err != nil {
		t.Fatal(err)
	}
	counter := &controllerKubeCounter{base: client.http.Transport, namespace: env["NAMESPACE"]}
	trace := &nodeLossStatusTrace{base: counter, rows: map[string][]nodeLossPodStatus{}}
	client.http.Transport = trace
	t.Cleanup(client.http.CloseIdleConnections)
	var metrics []nodeLossMetric
	t.Cleanup(func() {
		raw, err := json.MarshalIndent(struct {
			Passed       bool             `json:"passed"`
			NativeVendor bool             `json:"native_vendor"`
			Modes        []nodeLossMetric `json:"modes"`
		}{!t.Failed(), false, metrics}, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		if err = os.WriteFile(filepath.Join(dir, "node-loss-driver-result.json"), append(raw, '\n'), 0600); err != nil {
			t.Error(err)
		}
	})
	for _, mode := range []string{"pending", "normal", "lost"} {
		t.Run(mode, func(t *testing.T) {
			scope := asset.Scope{Tenant: "node-loss-fixture", Project: "actual-kubernetes", Runtime: mode + "-" + uuid.NewString(), Adapter: "codex"}
			business, oldCP, session := nodeLossBaseline(t, ctx, api, secret, scope)
			turn, attempt := uuid.NewString(), uuid.NewString()
			route := "/v1/tasks/" + scope.Runtime + "/turns/" + turn
			var record task.Record
			if err = business.JSON(ctx, "PUT", route, task.Question{SessionID: session, Message: "synthetic node-loss heartbeat"}, &record); err != nil {
				t.Fatal(err)
			}
			if err = business.JSON(ctx, "POST", route+"/claim", task.Claim{Attempt: attempt}, &record); err != nil {
				t.Fatal(err)
			}
			originalDeadline := record.Deadline
			a := platform.SandboxAddress{Scope: scope, TurnID: turn, Attempt: attempt}
			profile := Profile{Scope: scope, Namespace: env["NAMESPACE"], Image: env["IMAGE"], RuntimeClass: "agentmoor-gvisor", GatewayURL: "https://" + net.JoinHostPort(controllerGatewayName, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)), GatewayIP: env["GATEWAY_IP"], GatewayCAPath: "/etc/agentmoor/fixture-ca.pem", RuntimeExecutable: "/usr/local/bin/fixture-native-disabled", Model: "synthetic-controller-fixture", FilesystemClass: "gvisor-overlay2-memory.v1", CPUMillis: 250, MemoryMiB: 256, WorkspaceMiB: 128, TempMiB: 64}
			fingerprint, err := profile.Fingerprint()
			if err != nil {
				t.Fatal(err)
			}
			input := "synthetic-controller:heartbeat"
			r, err := catalog.BeginSandbox(ctx, a, fingerprint, controllerKubeBlob(input))
			if err != nil {
				t.Fatal(err)
			}
			gateway.register(r.ID, mode)
			if _, err = catalog.SealSandboxInput(ctx, a, strings.NewReader(input)); err != nil {
				t.Fatal(err)
			}
			controller, err := New(ClientConfig{URL: env["API"], CAFile: env["KUBE_CA"], TokenFile: env["TOKEN_FILE"]}, []Profile{profile}, catalog)
			if err != nil {
				t.Fatal(err)
			}
			controller.client.http.CloseIdleConnections()
			controller.client = client
			defer controllerKubeCleanup(t, controller, a)
			before := counter.snapshot()
			if mode == "pending" {
				nodeLossEvent("cordon-node", nodeLossMetric{Mode: mode})
				nodeLossWaitFile(t, ctx, dir, "pending.node-cordoned")
			}
			r, err = controller.Start(ctx, a)
			if err != nil || r.PodUID == "" {
				t.Fatal("production controller did not bind the guest", err)
			}
			var caps platform.SandboxCapabilities
			if err = controllerWait(ctx, 60*time.Second, func() (bool, error) {
				raw, exists, err := client.Get(ctx, r.PodNamespace, r.PodName)
				if err != nil || !exists {
					return false, err
				}
				p, c, err := inspectPod(r, raw)
				if err != nil {
					return false, err
				}
				caps = c
				var pod struct {
					Spec struct {
						NodeName string `json:"nodeName"`
					} `json:"spec"`
					Status struct {
						Phase string `json:"phase"`
					} `json:"status"`
				}
				if err = json.Unmarshal(raw, &pod); err != nil {
					return false, err
				}
				if mode == "pending" {
					return p.uid == r.PodUID && pod.Status.Phase == "Pending" && pod.Spec.NodeName == "", nil
				}
				return p.uid == r.PodUID && pod.Status.Phase == "Running", nil
			}); err != nil {
				t.Fatal("guest did not reach Running", err)
			}
			h := sha256.Sum256([]byte(attempt))
			metric := nodeLossMetric{Mode: mode, RunID: r.ID, PodName: r.PodName, PodUID: r.PodUID, Namespace: r.PodNamespace, TurnID: turn, AttemptSHA256: hex.EncodeToString(h[:]), OriginalDeadline: originalDeadline}
			defer func() {
				metric.Requests = counter.snapshot().since(before)
				metric.StatusHistory = trace.history(metric.PodUID)
				metrics = append(metrics, metric)
			}()
			nodeLossEvent("observe", metric)
			nodeLossWaitFile(t, ctx, dir, mode+".observation-ready")
			if mode != "lost" {
				if err = business.JSON(ctx, "POST", route+"/cancel", nil, &record); err != nil {
					t.Fatal(err)
				}
				if _, err = controller.Cancel(ctx, a); err != nil {
					t.Fatal(err)
				}
				if err = controllerWait(ctx, 30*time.Second, func() (bool, error) { var e error; r, e = controller.Reconcile(ctx, a); return r.PodDeleted, e }); err != nil {
					t.Fatal(err)
				}
			} else {
				nodeLossEvent("stop-kubelet", metric)
				nodeLossWaitFile(t, ctx, dir, "lost.kubelet-stopped")
				// Freeze the actual last status after kubelet stop, before API deletion.
				if _, exists, e := client.Get(ctx, r.PodNamespace, r.PodName); e != nil || !exists {
					t.Fatal("lost Pod disappeared before the UID force-delete")
				}
				path, _ := podRoute(r.PodNamespace, r.PodName)
				body, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "gracePeriodSeconds": 0, "preconditions": map[string]string{"uid": r.PodUID}})
				status, _, e := client.request(ctx, "delete", "DELETE", path, body)
				if e != nil || (status != http.StatusOK && status != http.StatusAccepted) {
					t.Fatal("owned UID force-delete failed")
				}
				if _, exists, e := client.Get(ctx, r.PodNamespace, r.PodName); e != nil || exists {
					t.Fatal("force-deleted API object still exists")
				}
				metric.APINotFound = true
				r, err = controller.Reconcile(ctx, a)
				if err != nil {
					t.Fatal("production reconcile failed", err)
				}
				afterReplay, e := controller.Start(ctx, a)
				if e != nil || afterReplay.ID != r.ID || counter.snapshot().since(before).Posts != 1 {
					t.Fatal("lost Pod was re-created or adopted")
				}
			}
			metric.State, metric.PodDeleted, metric.PodTerminal = r.State, r.PodDeleted, r.PodTerminal
			metric.IdentityDeadlineUnchanged = r.Address() == a && r.Deadline.Equal(originalDeadline)
			metric.CapabilitiesDenied = true
			for token, purpose := range map[string]platform.SandboxPurpose{caps.Input: platform.SandboxInput, caps.Output: platform.SandboxOutput, caps.Model: platform.SandboxModel} {
				if _, e := catalog.AuthenticateSandboxCapability(ctx, token, purpose); !errors.Is(e, platform.ErrSandboxDenied) {
					metric.CapabilitiesDenied = false
				}
			}
			var cp task.Checkpoint
			if err = business.JSON(ctx, "GET", "/v1/tasks/"+scope.Runtime+"/checkpoint", nil, &cp); err != nil {
				t.Fatal(err)
			}
			metric.CheckpointUnchanged = reflect.DeepEqual(oldCP, cp)
			if !metric.CapabilitiesDenied || !metric.IdentityDeadlineUnchanged || !metric.CheckpointUnchanged {
				t.Fatal("node loss changed authority, identity, deadline or previous checkpoint")
			}
			metric.Requests = counter.snapshot().since(before)
			metric.StatusHistory = trace.history(metric.PodUID)
			nodeLossEvent("reconciled", metric)
			nodeLossWaitFile(t, ctx, dir, mode+".physically-cleaned")
			if mode == "lost" && r.PodDeleted && !r.PodTerminal {
				t.Error("PHYSICAL_CLEANUP_INVARIANT: missing API object was declared deleted without terminal proof")
			}
		})
	}
}

func TestNodeLossStatusTraceKeepsBodyAndDropsPrivateFields(t *testing.T) {
	private := "synthetic-private-value"
	uid := uuid.NewString()
	body := []byte(fmt.Sprintf(`{"metadata":{"uid":%q,"resourceVersion":"12","deletionTimestamp":"2026-11-27T00:00:00Z","annotations":{"secret":%q}},"spec":{"nodeName":"owned-node","containers":[{"env":[{"name":"secret","value":%q}]}]},"status":{"phase":"Failed","containerStatuses":[{"name":"guest","containerID":"containerd://fixture","restartCount":0,"state":{"terminated":{"exitCode":137,"message":%q,"startedAt":"2026-11-27T00:00:00Z","finishedAt":"2026-11-27T00:00:01Z"}}}]}}`, uid, private, private, private))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	trace := &nodeLossStatusTrace{base: server.Client().Transport, rows: map[string][]nodeLossPodStatus{}}
	req, err := http.NewRequestWithContext(t.Context(), "GET", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := trace.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	actual, err := io.ReadAll(res.Body)
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatal("observer altered the actual controller response", err)
	}
	rows := trace.history(uid)
	if len(rows) != 1 || rows[0].NodeName != "owned-node" || rows[0].ResourceVersion != "12" || len(rows[0].Containers) != 1 || rows[0].Containers[0].State.Terminated == nil || *rows[0].Containers[0].State.Terminated.ExitCode != 137 {
		t.Fatal("observer lost real termination fields")
	}
	public, err := json.Marshal(rows)
	if err != nil || bytes.Contains(public, []byte(private)) {
		t.Fatal("private fields entered status evidence")
	}
}
