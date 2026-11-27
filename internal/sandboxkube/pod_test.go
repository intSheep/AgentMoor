package sandboxkube

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestPodHasCredentiallessFixedSecurityBoundary(t *testing.T) {
	f := newControllerFixture(t)
	if _, err := f.c.Start(t.Context(), f.a); err != nil {
		t.Fatal(err)
	}
	f.kube.mu.Lock()
	defer f.kube.mu.Unlock()
	s := f.kube.pod["spec"].(map[string]any)
	if s["runtimeClassName"] != "gvisor" || s["restartPolicy"] != "Never" || s["automountServiceAccountToken"] != false || s["enableServiceLinks"] != false || s["dnsPolicy"] != "None" || s["preemptionPolicy"] != "Never" || s["priorityClassName"] != "agentmoor-sandbox" {
		t.Fatal("wrong fixed Pod execution policy")
	}
	security := s["securityContext"].(map[string]any)
	if security["runAsNonRoot"] != true || security["runAsUser"] != json.Number("65532") || security["seccompProfile"].(map[string]any)["type"] != "RuntimeDefault" {
		t.Fatal("wrong Pod identity/seccomp policy")
	}
	containers := s["containers"].([]any)
	if len(containers) != 1 {
		t.Fatal("unexpected sibling container")
	}
	c := containers[0].(map[string]any)
	cs := c["securityContext"].(map[string]any)
	if cs["allowPrivilegeEscalation"] != false || cs["privileged"] != false || cs["readOnlyRootFilesystem"] != true || !reflect.DeepEqual(cs["capabilities"].(map[string]any)["drop"], []any{"ALL"}) {
		t.Fatal("container privilege boundary differs")
	}
	for _, key := range []string{"env", "envFrom", "ports", "volumeDevices"} {
		if _, ok := c[key]; ok {
			t.Fatal("unexpected environment/device input")
		}
	}
	resources := c["resources"].(map[string]any)
	if !reflect.DeepEqual(resources["requests"], resources["limits"]) {
		t.Fatal("hard memory/CPU limits missing")
	}
	volumes := s["volumes"].([]any)
	if len(volumes) != 3 {
		t.Fatal("unexpected credential/state volume")
	}
	for _, v := range volumes {
		m := v.(map[string]any)
		if m["name"] == "caps" {
			if len(m) != 2 || m["downwardAPI"] == nil {
				t.Fatal("caps were not projected from own annotations")
			}
		} else {
			e := m["emptyDir"].(map[string]any)
			if len(m) != 2 || e["medium"] != "Memory" || e["sizeLimit"] == nil {
				t.Fatal("unbounded or host-backed writable volume")
			}
		}
	}
	body, _ := json.Marshal(f.kube.pod)
	for _, forbidden := range []string{"fixture-controller-token", "serviceAccountToken", "hostPath", "OPENAI_API_KEY", "AGENTMOOR_TOKEN", "journal", "worker.json"} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatal("platform credential/state appeared in guest Pod")
		}
	}
}

func TestPodJSONRejectsDuplicateAliasTrailingAndExcessiveNesting(t *testing.T) {
	f := newControllerFixture(t)
	r, err := f.c.Start(t.Context(), f.a)
	if err != nil {
		t.Fatal(err)
	}
	f.kube.mu.Lock()
	raw, _ := json.Marshal(f.kube.pod)
	f.kube.mu.Unlock()
	for _, bad := range [][]byte{
		bytes.Replace(raw, []byte(`"metadata":{`), []byte(`"metadata":{},"metadata":{`), 1),
		bytes.Replace(raw, []byte(`"metadata":`), []byte(`"Metadata":`), 1),
		append(append([]byte{}, raw...), []byte(` {}`)...),
		[]byte(strings.Repeat(`{"a":`, 66) + `{}` + strings.Repeat(`}`, 66)),
	} {
		if _, _, err := inspectPod(r, bad); !errors.Is(err, ErrPod) {
			t.Fatal("ambiguous or unbounded Pod response admitted", err)
		}
	}
}

func TestCalicoInformationalAnnotationsPreserveAllControlledFields(t *testing.T) {
	f := newControllerFixture(t)
	r, err := f.c.Start(t.Context(), f.a)
	if err != nil {
		t.Fatal(err)
	}
	f.kube.mu.Lock()
	original, _ := json.Marshal(f.kube.pod)
	f.kube.mu.Unlock()
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any, map[string]any)
		valid  bool
	}{
		{"actual-v4-add", func(_, _ map[string]any) {}, true},
		{"dual-stack", func(_, a map[string]any) { a["cni.projectcalico.org/podIPs"] = "10.244.68.73/32,2001:db8::1/128" }, true},
		{"actual-delete", func(_, a map[string]any) {
			a["cni.projectcalico.org/podIP"], a["cni.projectcalico.org/podIPs"] = "", ""
		}, true},
		{"unknown-calico-key", func(_, a map[string]any) { a["cni.projectcalico.org/forged"] = "anything" }, false},
		{"partial-patch", func(_, a map[string]any) { delete(a, "cni.projectcalico.org/containerID") }, false},
		{"invalid-container-id", func(_, a map[string]any) { a["cni.projectcalico.org/containerID"] = strings.Repeat("A", 64) }, false},
		{"wrong-type", func(_, a map[string]any) { a["cni.projectcalico.org/podIPs"] = []any{"10.244.68.73/32"} }, false},
		{"network-cidr", func(_, a map[string]any) {
			a["cni.projectcalico.org/podIP"], a["cni.projectcalico.org/podIPs"] = "10.244.68.0/24", "10.244.68.0/24"
		}, false},
		{"disagreeing-ips", func(_, a map[string]any) { a["cni.projectcalico.org/podIPs"] = "10.244.68.74/32" }, false},
		{"duplicate-ips", func(_, a map[string]any) { a["cni.projectcalico.org/podIPs"] = "10.244.68.73/32,10.244.68.73/32" }, false},
		{"empty-partial-delete", func(_, a map[string]any) { a["cni.projectcalico.org/podIPs"] = "" }, false},
		{"changed-model-cap", func(_, a map[string]any) { a[modelAnnotation] = "replacement" }, false},
		{"changed-image", func(p, _ map[string]any) {
			p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] = "other@sha256:" + strings.Repeat("b", 64)
		}, false},
		{"added-env", func(p, _ map[string]any) {
			p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"] = []any{map[string]any{"name": "UNTRUSTED", "value": "value"}}
		}, false},
		{"added-hostpath", func(p, _ map[string]any) {
			p["spec"].(map[string]any)["volumes"] = []any{map[string]any{"name": "unsafe", "hostPath": map[string]any{"path": "/"}}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod, err := object(original)
			if err != nil {
				t.Fatal(err)
			}
			a := pod["metadata"].(map[string]any)["annotations"].(map[string]any)
			a["cni.projectcalico.org/containerID"] = strings.Repeat("a", 64)
			a["cni.projectcalico.org/podIP"], a["cni.projectcalico.org/podIPs"] = "10.244.68.73/32", "10.244.68.73/32"
			tc.mutate(pod, a)
			raw, _ := json.Marshal(pod)
			_, _, err = inspectPod(r, raw)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}
