package sandboxkube

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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

func testProfile() Profile {
	return Profile{Scope: asset.Scope{Tenant: "fixture", Project: "report", Runtime: "run-1", Adapter: "codex"},
		Namespace: "sandbox", Image: "registry.example/guest@sha256:" + strings.Repeat("a", 64), RuntimeClass: "gvisor",
		GatewayURL: "https://gateway.example:7443", GatewayIP: "10.20.0.9", GatewayCAPath: "/etc/gateway/ca.crt",
		RuntimeExecutable: "/usr/local/bin/codex", Model: "fixture-model", FilesystemClass: "gvisor-overlay2-memory.v1",
		CPUMillis: 1000, MemoryMiB: 1024, WorkspaceMiB: 512, TempMiB: 128}
}

type kubeFixture struct {
	t                                               *testing.T
	mu                                              sync.Mutex
	pod                                             map[string]any
	posts, gets, deletes, patches                   int
	ambiguous, missing, keepDeleted, replaceDeleted bool
	raceBindOnPatch, dropPatchAck                   bool
	onCreate                                        func(map[string]any)
}

func (f *kubeFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer fixture-controller-token" || !strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/sandbox/pods") {
		f.t.Error("unexpected Kubernetes credential or route")
		w.WriteHeader(403)
		return
	}
	switch r.Method {
	case "POST":
		f.posts++
		if r.URL.Path != "/api/v1/namespaces/sandbox/pods" {
			f.t.Error("create used a Pod subresource")
		}
		data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		pod, err := object(data)
		if err != nil {
			f.t.Error("invalid creation template")
			w.WriteHeader(422)
			return
		}
		if !f.missing {
			meta := pod["metadata"].(map[string]any)
			meta["uid"], meta["resourceVersion"], meta["creationTimestamp"] = uuid.NewString(), "1", time.Now().UTC().Format(time.RFC3339Nano)
			spec := pod["spec"].(map[string]any)
			spec["serviceAccountName"], spec["serviceAccount"], spec["nodeName"], spec["priority"] = "default", "default", "fixture-node", json.Number("0")
			spec["tolerations"] = []any{
				map[string]any{"key": "node.kubernetes.io/unreachable", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": json.Number("300")},
				map[string]any{"key": "node.kubernetes.io/not-ready", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": json.Number("300")}}
			pod["status"] = map[string]any{"phase": "Pending"}
			if f.onCreate != nil {
				f.onCreate(pod)
			}
			f.pod = pod
		}
		if f.ambiguous || f.missing {
			w.WriteHeader(503)
			io.WriteString(w, "owned ambiguous create response")
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(f.pod)
	case "GET":
		f.gets++
		if f.pod == nil || r.URL.Path != "/api/v1/namespaces/sandbox/pods/"+f.pod["metadata"].(map[string]any)["name"].(string) {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(f.pod)
	case "DELETE":
		f.deletes++
		var options struct {
			Kind          string `json:"kind"`
			Preconditions struct {
				UID string `json:"uid"`
			} `json:"preconditions"`
			Grace int `json:"gracePeriodSeconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&options); err != nil || options.Kind != "DeleteOptions" || options.Grace != 5 {
			f.t.Error("delete did not use fixed graceful UID preconditions")
		}
		if f.pod == nil {
			http.NotFound(w, r)
			return
		}
		if options.Preconditions.UID != f.pod["metadata"].(map[string]any)["uid"] {
			f.t.Error("controller attempted to delete a foreign UID")
			w.WriteHeader(409)
			return
		}
		if f.replaceDeleted {
			f.pod["metadata"].(map[string]any)["uid"] = uuid.NewString()
		} else {
			f.pod["metadata"].(map[string]any)["deletionTimestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
			f.bumpVersion()
			if !f.keepDeleted {
				meta := f.pod["metadata"].(map[string]any)
				finalizers, _ := meta["finalizers"].([]any)
				if len(finalizers) == 0 {
					f.pod = nil
				} else {
					status, _ := f.pod["status"].(map[string]any)
					statuses, _ := status["containerStatuses"].([]any)
					terminated := false
					if len(statuses) == 1 {
						s, _ := statuses[0].(map[string]any)
						state, _ := s["state"].(map[string]any)
						_, terminated = state["terminated"]
					}
					if !terminated && f.pod["spec"].(map[string]any)["nodeName"] != nil {
						f.setTerminal(143)
					}
				}
			}
		}
		w.WriteHeader(202)
		io.WriteString(w, `{}`)
	case "PATCH":
		f.patches++
		if r.Header.Get("Content-Type") != "application/json-patch+json" || f.pod == nil {
			w.WriteHeader(409)
			return
		}
		var operations []struct {
			Op, Path string
			Value    any
		}
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if decoder.Decode(&operations) != nil || len(operations) != 6 {
			f.t.Error("unexpected finalizer patch")
			w.WriteHeader(422)
			return
		}
		if f.raceBindOnPatch {
			f.pod["spec"].(map[string]any)["nodeName"] = "racing-node"
			f.bumpVersion()
			f.raceBindOnPatch = false
		}
		meta := f.pod["metadata"].(map[string]any)
		values := []any{meta["uid"], meta["resourceVersion"], meta["deletionTimestamp"], meta["finalizers"], f.pod["spec"]}
		paths := []string{"/metadata/uid", "/metadata/resourceVersion", "/metadata/deletionTimestamp", "/metadata/finalizers", "/spec"}
		for i, path := range paths {
			if operations[i].Op != "test" || operations[i].Path != path || !reflect.DeepEqual(operations[i].Value, values[i]) {
				w.WriteHeader(409)
				return
			}
		}
		finals, _ := meta["finalizers"].([]any)
		index := -1
		for i, value := range finals {
			if value == guestStopFinalizer {
				index = i
			}
		}
		if index < 0 || operations[5].Op != "remove" || operations[5].Path != "/metadata/finalizers/"+strconv.Itoa(index) {
			f.t.Error("patch did not remove only owned finalizer")
			w.WriteHeader(422)
			return
		}
		meta["finalizers"] = append(append([]any{}, finals[:index]...), finals[index+1:]...)
		f.bumpVersion()
		response, _ := json.Marshal(f.pod)
		if !f.keepDeleted && len(meta["finalizers"].([]any)) == 0 {
			f.pod = nil
		}
		if f.dropPatchAck {
			f.dropPatchAck = false
			connection, _, _ := w.(http.Hijacker).Hijack()
			connection.Close()
			return
		}
		w.WriteHeader(200)
		w.Write(response)
	default:
		f.t.Error("controller used a forbidden Kubernetes operation")
		w.WriteHeader(405)
	}
}

func (f *kubeFixture) terminal(exit int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setTerminal(exit)
}

func (f *kubeFixture) bumpVersion() {
	meta := f.pod["metadata"].(map[string]any)
	old, _ := strconv.Atoi(meta["resourceVersion"].(string))
	meta["resourceVersion"] = strconv.Itoa(old + 1)
}
func (f *kubeFixture) setTerminal(exit int) {
	phase := "Succeeded"
	if exit != 0 {
		phase = "Failed"
	}
	sum := sha256.Sum256([]byte(f.pod["metadata"].(map[string]any)["uid"].(string)))
	id := "containerd://" + hex.EncodeToString(sum[:])
	f.pod["status"] = map[string]any{"phase": phase, "containerStatuses": []any{
		map[string]any{"name": "guest", "containerID": id, "restartCount": 0, "state": map[string]any{"terminated": map[string]any{"exitCode": exit, "containerID": id}}}}}
	f.bumpVersion()
}

type controllerFixture struct {
	c        *Controller
	kube     *kubeFixture
	catalog  *platform.Catalog
	a        platform.SandboxAddress
	request  func(string, string, any) task.Record
	platform *platform.Client
}

func newControllerFixture(t *testing.T, deadlineAfter ...time.Duration) controllerFixture {
	t.Helper()
	p := testProfile()
	catalogDir := t.TempDir()
	catalog, err := platform.OpenCatalog(t.Context(), catalogDir, "", platform.DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { catalog.Close() })
	secret := bytes.Repeat([]byte{9}, 32)
	api := httptest.NewServer((&platform.API{Catalog: catalog, Secret: secret, Temp: t.TempDir(), TaskAdapters: map[string]bool{"codex": true}, TurnEnabled: func(asset.Scope) bool { return true }}).Handler())
	t.Cleanup(api.Close)
	token, err := platform.Issue(secret, platform.Grant{Scope: p.Scope, Read: true, Write: true, Submit: true, Execute: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	platformClient, err := platform.NewClient(api.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, value any) task.Record {
		t.Helper()
		body, _ := json.Marshal(value)
		req, _ := http.NewRequestWithContext(t.Context(), method, api.URL+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := api.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal("task fixture request failed", method, path, res.StatusCode)
		}
		var record task.Record
		if err := json.NewDecoder(res.Body).Decode(&record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	a := platform.SandboxAddress{Scope: p.Scope, Attempt: uuid.NewString()}
	request("PUT", "/v1/tasks/run-1", task.Spec{Instruction: "make a report", Output: "report.txt"})
	claimed := request("POST", "/v1/tasks/run-1/claim", task.Claim{Attempt: a.Attempt})
	if len(deadlineAfter) != 0 {
		// Shorten only this owned clock fixture before Begin freezes the real
		// record. Production has no API for replacing an allocation deadline.
		claimed.Deadline = time.Now().UTC().Add(deadlineAfter[0])
		db, err := sql.Open("sqlite3", filepath.Join(catalogDir, "catalog.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(claimed)
		_, err = db.ExecContext(t.Context(), "UPDATE tasks SET record=$1 WHERE scope=$2", string(raw), a.Scope.Prefix())
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	raw := "sealed credentialless input fixture"
	h := sha256.Sum256([]byte(raw))
	profile, _ := p.Fingerprint()
	if _, err := catalog.BeginSandbox(t.Context(), a, profile, platform.SandboxBlob{SHA256: hex.EncodeToString(h[:]), Size: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.SealSandboxInput(t.Context(), a, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	kube := &kubeFixture{t: t}
	cfg := newClientFixture(t, kube.ServeHTTP).config
	// The client fixture's initial token is unrelated to this controller.
	if err := os.WriteFile(cfg.TokenFile, []byte("fixture-controller-token"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg, []Profile{p}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	return controllerFixture{c: c, kube: kube, catalog: catalog, a: a, request: request, platform: platformClient}
}

func outputCaps(t *testing.T, f controllerFixture) platform.SandboxCapabilities {
	t.Helper()
	f.kube.mu.Lock()
	defer f.kube.mu.Unlock()
	a := f.kube.pod["metadata"].(map[string]any)["annotations"].(map[string]any)
	return platform.SandboxCapabilities{Input: a[inputAnnotation].(string), Output: a[outputAnnotation].(string), Model: a[modelAnnotation].(string)}
}

func sealOutput(t *testing.T, f controllerFixture) platform.SandboxCapabilities {
	t.Helper()
	caps := outputCaps(t, f)
	raw := "sealed guest result"
	h := sha256.Sum256([]byte(raw))
	if _, err := f.catalog.SealSandboxOutput(t.Context(), caps.Output, platform.SandboxBlob{SHA256: hex.EncodeToString(h[:]), Size: int64(len(raw))}, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	return caps
}

func TestControllerConcurrentStartAndRestartNeverRecreate(t *testing.T) {
	f := newControllerFixture(t)
	f.kube.ambiguous = true
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if r, err := f.c.Start(t.Context(), f.a); err != nil || r.PodUID == "" || r.State != platform.SandboxRunning {
				t.Error("matching ambiguous creation was not recovered", err)
			}
		})
	}
	group.Wait()
	if f.kube.posts != 1 {
		t.Fatal("concurrent start created more than once", f.kube.posts)
	}
	// A fresh controller has no process-local create state or raw capabilities.
	f.c = &Controller{profiles: f.c.profiles, client: f.c.client, catalog: f.catalog}
	if r, err := f.c.Start(t.Context(), f.a); err != nil || r.PodUID == "" || f.kube.posts != 1 {
		t.Fatal("recovery reissued create", err, f.kube.posts)
	}
}

func TestControllerMissingAfterFenceFailsWithoutRecreate(t *testing.T) {
	f := newControllerFixture(t)
	f.kube.missing = true
	if r, err := f.c.Start(t.Context(), f.a); !errors.Is(err, ErrMissingPod) || r.State != platform.SandboxFailed {
		t.Fatal("ambiguous missing create did not stop", err, r.State)
	}
	for range 2 {
		f.c.Start(t.Context(), f.a)
		f.c.Reconcile(t.Context(), f.a)
	}
	if f.kube.posts != 1 || f.kube.deletes != 0 {
		t.Fatal("missing create was repeated", f.kube.posts, f.kube.deletes)
	}
}

func TestControllerOutputWaitsForExitAndObservedDeletion(t *testing.T) {
	f := newControllerFixture(t)
	r, err := f.c.Start(t.Context(), f.a)
	if err != nil {
		t.Fatal(err)
	}
	caps := sealOutput(t, f)
	if _, _, err := f.catalog.ReadSandboxOutput(t.Context(), f.a); !errors.Is(err, platform.ErrNotReady) {
		t.Fatal("output visible while guest runs", err)
	}
	f.kube.terminal(0)
	f.kube.keepDeleted = true
	r, err = f.c.Reconcile(t.Context(), f.a)
	if err != nil || !r.PodTerminal || r.PodDeleted || r.State == platform.SandboxReady {
		t.Fatal("accepted delete ack without observed gone", err)
	}
	if _, _, err := f.catalog.ReadSandboxOutput(t.Context(), f.a); !errors.Is(err, platform.ErrNotReady) {
		t.Fatal("delete-in-progress output was visible", err)
	}
	f.kube.keepDeleted = false
	r, err = f.c.Reconcile(t.Context(), f.a)
	if err != nil || !r.PodDeleted || r.State != platform.SandboxReady || f.kube.posts != 1 {
		t.Fatal("successful stopped guest did not become publishable", err)
	}
	_, stream, err := f.catalog.ReadSandboxOutput(t.Context(), f.a)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(stream)
	stream.Close()
	if err != nil || string(data) != "sealed guest result" {
		t.Fatal("sealed result differs", err)
	}
	for purpose, token := range map[platform.SandboxPurpose]string{platform.SandboxInput: caps.Input, platform.SandboxOutput: caps.Output, platform.SandboxModel: caps.Model} {
		if _, err := f.catalog.AuthenticateSandboxCapability(t.Context(), token, purpose); !errors.Is(err, platform.ErrSandboxDenied) {
			t.Fatal("deleted guest retained authority", purpose, err)
		}
	}
}

func TestControllerForeignUIDNeverDeleted(t *testing.T) {
	f := newControllerFixture(t)
	if _, err := f.c.Start(t.Context(), f.a); err != nil {
		t.Fatal(err)
	}
	f.kube.mu.Lock()
	f.kube.pod["metadata"].(map[string]any)["uid"] = uuid.NewString()
	f.kube.mu.Unlock()
	if r, err := f.c.Reconcile(t.Context(), f.a); !errors.Is(err, ErrPod) || r.State != platform.SandboxFailed || f.kube.deletes != 0 {
		t.Fatal("replacement Pod was adopted/deleted", err, f.kube.deletes)
	}
}

func TestControllerCancelSurvivesCallerCancellation(t *testing.T) {
	f := newControllerFixture(t)
	if _, err := f.c.Start(t.Context(), f.a); err != nil {
		t.Fatal(err)
	}
	caps := outputCaps(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r, err := f.c.Cancel(ctx, f.a)
	if err != nil || !r.PodDeleted || r.State != platform.SandboxCancelled || f.kube.deletes != 1 {
		t.Fatal("cancelled caller prevented safe cleanup", err)
	}
	if _, err := f.catalog.AuthenticateSandboxCapability(t.Context(), caps.Model, platform.SandboxModel); !errors.Is(err, platform.ErrSandboxDenied) {
		t.Fatal("cancel left model authority", err)
	}
}

func TestControllerTaskCancellationAndProfileDriftCleanupOriginalNamespace(t *testing.T) {
	for _, change := range []string{"task-cancel", "profile-namespace"} {
		t.Run(change, func(t *testing.T) {
			f := newControllerFixture(t)
			if _, err := f.c.Start(t.Context(), f.a); err != nil {
				t.Fatal(err)
			}
			if change == "task-cancel" {
				f.request("POST", "/v1/tasks/run-1/cancel", nil)
			} else {
				p := f.c.profiles[f.a.Scope]
				p.Namespace = "changed-namespace"
				f.c.profiles[f.a.Scope] = p
			}
			r, err := f.c.Reconcile(t.Context(), f.a)
			if err != nil || !r.PodDeleted || f.kube.deletes != 1 || r.State == platform.SandboxReady {
				t.Fatal("stopped run did not clean its original namespace", err)
			}
		})
	}
}

func TestControllerWholeTemplateDriftRejected(t *testing.T) {
	for _, mutation := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"image", func(p map[string]any) {
			p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] = "registry.example/other:latest"
		}},
		{"runtime", func(p map[string]any) { p["spec"].(map[string]any)["runtimeClassName"] = "runc" }},
		{"command", func(p map[string]any) {
			p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["command"] = []string{"sh"}
		}},
		{"env", func(p map[string]any) {
			p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"] = []any{map[string]any{"name": "OPENAI_API_KEY", "value": "untrusted"}}
		}},
		{"host-volume", func(p map[string]any) {
			s := p["spec"].(map[string]any)
			s["volumes"] = append(s["volumes"].([]any), map[string]any{"name": "host", "hostPath": map[string]any{"path": "/"}})
		}},
		{"sidecar", func(p map[string]any) {
			s := p["spec"].(map[string]any)
			s["containers"] = append(s["containers"].([]any), map[string]any{"name": "sidecar", "image": "other"})
		}},
		{"init", func(p map[string]any) {
			p["spec"].(map[string]any)["initContainers"] = []any{map[string]any{"name": "init", "image": "other"}}
		}},
		{"service-account", func(p map[string]any) { p["spec"].(map[string]any)["serviceAccountName"] = "privileged" }},
		{"automount", func(p map[string]any) { p["spec"].(map[string]any)["automountServiceAccountToken"] = true }},
		{"host-network", func(p map[string]any) { p["spec"].(map[string]any)["hostNetwork"] = true }},
		{"processes", func(p map[string]any) { p["spec"].(map[string]any)["shareProcessNamespace"] = true }},
		{"security", func(p map[string]any) {
			p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["securityContext"].(map[string]any)["privileged"] = true
		}},
		{"deadline", func(p map[string]any) { p["spec"].(map[string]any)["activeDeadlineSeconds"] = 999 }},
		{"restart", func(p map[string]any) { p["spec"].(map[string]any)["restartPolicy"] = "Always" }},
		{"dns", func(p map[string]any) { p["spec"].(map[string]any)["dnsPolicy"] = "ClusterFirst" }},
		{"caps", func(p map[string]any) {
			a := p["metadata"].(map[string]any)["annotations"].(map[string]any)
			a[inputAnnotation], a[modelAnnotation] = a[modelAnnotation], a[inputAnnotation]
		}},
		{"label-only-owner", func(p map[string]any) {
			p["metadata"].(map[string]any)["annotations"].(map[string]any)["injected"] = "yes"
		}},
		{"toleration", func(p map[string]any) {
			p["spec"].(map[string]any)["tolerations"] = []any{map[string]any{"operator": "Exists"}}
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			f := newControllerFixture(t)
			f.kube.onCreate = mutation.edit
			r, err := f.c.Start(t.Context(), f.a)
			if !errors.Is(err, ErrPod) || r.State != platform.SandboxFailed || r.PodUID != "" || f.kube.deletes != 0 {
				t.Fatal("changed template was adopted", err)
			}
			caps := outputCaps(t, f)
			if _, err := f.catalog.AuthenticateSandboxCapability(t.Context(), caps.Output, platform.SandboxOutput); !errors.Is(err, platform.ErrSandboxDenied) {
				t.Fatal("rejected template retained authority", err)
			}
		})
	}
}

func TestControllerKnownUIDTemplateDriftIsCleaned(t *testing.T) {
	f := newControllerFixture(t)
	if _, err := f.c.Start(t.Context(), f.a); err != nil {
		t.Fatal(err)
	}
	f.kube.mu.Lock()
	f.kube.pod["spec"].(map[string]any)["hostNetwork"] = true
	f.kube.mu.Unlock()
	if r, err := f.c.Reconcile(t.Context(), f.a); !errors.Is(err, ErrPod) || r.State != platform.SandboxFailed || f.kube.deletes != 1 {
		t.Fatal("known UID drift was not revoked and cleaned", err)
	}
}

func TestControllerReplacementAfterDeleteCannotPublishOrBeDeleted(t *testing.T) {
	f := newControllerFixture(t)
	if _, err := f.c.Start(t.Context(), f.a); err != nil {
		t.Fatal(err)
	}
	sealOutput(t, f)
	f.kube.terminal(0)
	f.kube.replaceDeleted = true
	if r, err := f.c.Reconcile(t.Context(), f.a); !errors.Is(err, ErrPod) || r.PodDeleted || r.State == platform.SandboxReady {
		t.Fatal("replacement counted as gone", err)
	}
	f.c.Reconcile(t.Context(), f.a)
	if f.kube.deletes != 1 {
		t.Fatal("replacement UID was deleted", f.kube.deletes)
	}
}

func TestControllerFailedPodZeroExitCannotPublish(t *testing.T) {
	f := newControllerFixture(t)
	if _, err := f.c.Start(t.Context(), f.a); err != nil {
		t.Fatal(err)
	}
	sealOutput(t, f)
	f.kube.terminal(0)
	f.kube.mu.Lock()
	f.kube.pod["status"].(map[string]any)["phase"] = "Failed"
	f.kube.mu.Unlock()
	if r, err := f.c.Reconcile(t.Context(), f.a); err != nil || r.State != platform.SandboxFailed || !r.PodDeleted || r.ExitCode == 0 {
		t.Fatal("failed Pod infrastructure state was published", err)
	}
	if _, _, err := f.catalog.ReadSandboxOutput(t.Context(), f.a); !errors.Is(err, platform.ErrNotReady) {
		t.Fatal("failed Pod result became readable", err)
	}
}

func TestControllerCreateTimeoutRecoversWithGETOnly(t *testing.T) {
	f := newControllerFixture(t)
	f.kube.onCreate = func(map[string]any) { time.Sleep(150 * time.Millisecond) }
	f.c.client.http.Timeout = 20 * time.Millisecond
	if _, err := f.c.Start(t.Context(), f.a); err == nil {
		t.Fatal("owned response timeout not exercised")
	}
	time.Sleep(200 * time.Millisecond)
	if r, err := f.c.Start(t.Context(), f.a); err != nil || r.PodUID == "" || f.kube.posts != 1 {
		t.Fatal("timeout recovery recreated or lost accepted Pod", err)
	}
}

func TestControllerCrashBeforePOSTDoesNotCreateOnRecovery(t *testing.T) {
	for _, seal := range []bool{false, true} {
		t.Run(map[bool]string{false: "fence-only", true: "template-sealed"}[seal], func(t *testing.T) {
			f := newControllerFixture(t)
			r, _ := f.catalog.SandboxRun(t.Context(), f.a)
			permit, err := f.catalog.FenceSandboxCreate(t.Context(), f.a, r.Deadline)
			if err != nil {
				t.Fatal(err)
			}
			if seal {
				p, _ := f.c.Profile(f.a.Scope)
				_, hash, err := podTemplate(p, permit.Run, permit.Capabilities, 900)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.catalog.SealSandboxPodTemplate(t.Context(), f.a, p.Namespace, hash); err != nil {
					t.Fatal(err)
				}
			}
			if r, err := f.c.Start(t.Context(), f.a); !errors.Is(err, ErrMissingPod) || r.State != platform.SandboxFailed || f.kube.posts != 0 {
				t.Fatal("recovery created after an uncertain fence", err)
			}
		})
	}
}

func TestControllerDeadlineRevokesAndCleansWithoutRefreshingDeadline(t *testing.T) {
	f := newControllerFixture(t, 400*time.Millisecond)
	r, err := f.c.Start(t.Context(), f.a)
	if err != nil {
		t.Fatal(err)
	}
	deadline := r.Deadline
	caps := outputCaps(t, f)
	time.Sleep(time.Until(deadline) + 20*time.Millisecond)
	if _, err := f.catalog.AuthenticateSandboxCapability(t.Context(), caps.Model, platform.SandboxModel); !errors.Is(err, platform.ErrSandboxDenied) {
		t.Fatal("deadline did not remove authority", err)
	}
	r, err = f.c.Reconcile(t.Context(), f.a)
	if err != nil || !r.PodDeleted || r.State != platform.SandboxFailed || r.ErrorCode != "deadline-exceeded" || !r.Deadline.Equal(deadline) || f.kube.posts != 1 {
		t.Fatal("deadline was renewed or stopped Pod not cleaned", err)
	}
}

func TestControllerCancelledStartRevokesAndCleansUncertainCreate(t *testing.T) {
	f := newControllerFixture(t)
	started := make(chan struct{})
	f.kube.onCreate = func(map[string]any) { close(started); time.Sleep(75 * time.Millisecond) }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := f.c.Start(ctx, f.a); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("start cancellation not observed", err)
	}
	r, err := f.catalog.SandboxRun(t.Context(), f.a)
	if err != nil || !r.PodDeleted || r.State != platform.SandboxCancelled || f.kube.posts != 1 || f.kube.deletes != 1 {
		t.Fatal("cancelled create was not revoked/cleaned", err)
	}
}

func TestControllerLockWaitHonorsContext(t *testing.T) {
	f := newControllerFixture(t)
	unlock, err := f.c.lock(t.Context(), f.a)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := f.c.Reconcile(ctx, f.a); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("cleanup lock ignored context", err)
	}
	if f.kube.gets != 0 || f.kube.posts != 0 {
		t.Fatal("timed out wait contacted Kubernetes")
	}
}

func TestControllerReconcileAllContinuesAfterPermanentMissingRun(t *testing.T) {
	f := newControllerFixture(t)
	f.kube.missing = true
	if _, err := f.c.Start(t.Context(), f.a); !errors.Is(err, ErrMissingPod) {
		t.Fatal("missing first run not exercised", err)
	}
	// Publish a synthetic successful checkpoint through the public asset/task
	// API so a new logical turn can be allocated in this same sorted scope.
	session := uuid.NewString()
	for _, key := range []asset.Key{{Kind: "artifact", ID: "out/report.txt"}, {Kind: "workspace", ID: "root"}, {Kind: "session", ID: session}} {
		raw := "synthetic " + key.Kind
		h := sha256.Sum256([]byte(raw))
		file := filepath.Join(t.TempDir(), "blob")
		if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		change := asset.Change{Metadata: asset.Metadata{Key: key, AdapterVersion: "1", Format: "file.v1", MediaType: "text/plain", Name: key.ID,
			Origin: "runtime", Completeness: "persisted-files", SessionID: session, SHA256: hex.EncodeToString(h[:]), Size: int64(len(raw))}}
		if _, err := f.platform.Publish(t.Context(), change, file); err != nil {
			t.Fatal(err)
		}
	}
	f.request("POST", "/v1/tasks/run-1/finish", task.Finish{Attempt: f.a.Attempt, SessionID: session})
	second := f
	second.a.TurnID, second.a.Attempt = uuid.NewString(), uuid.NewString()
	route := "/v1/tasks/run-1/turns/" + second.a.TurnID
	f.request("PUT", route, task.Question{SessionID: session, Message: "continue"})
	f.request("POST", route+"/claim", task.Claim{Attempt: second.a.Attempt})
	raw := "continuation sealed input"
	h := sha256.Sum256([]byte(raw))
	profile, _ := testProfile().Fingerprint()
	if _, err := f.catalog.BeginSandbox(t.Context(), second.a, profile, platform.SandboxBlob{SHA256: hex.EncodeToString(h[:]), Size: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.catalog.SealSandboxInput(t.Context(), second.a, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	f.kube.missing = false
	if _, err := f.c.Start(t.Context(), second.a); err != nil {
		t.Fatal(err)
	}
	sealOutput(t, second)
	f.kube.terminal(0)
	if err := f.c.ReconcileAll(t.Context()); !errors.Is(err, ErrMissingPod) {
		t.Fatal("permanent first-run error was not reported", err)
	}
	r, err := f.catalog.SandboxRun(t.Context(), second.a)
	if err != nil || r.State != platform.SandboxReady || !r.PodDeleted {
		t.Fatal("first broken run starved independent terminal turn", err)
	}
}
