package sandboxkube

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"agentmoor/internal/platform"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

var ErrPod = errors.New("sandbox Pod does not match its frozen template")

const (
	inputAnnotation  = "agentmoor.io/input-cap"
	outputAnnotation = "agentmoor.io/output-cap"
	modelAnnotation  = "agentmoor.io/model-cap"
)

func cpuQuantity(n int64) string {
	if n%1000 == 0 {
		return strconv.FormatInt(n/1000, 10)
	}
	return strconv.FormatInt(n, 10) + "m"
}

func memoryQuantity(n int64) string {
	if n%1024 == 0 {
		return strconv.FormatInt(n/1024, 10) + "Gi"
	}
	return strconv.FormatInt(n, 10) + "Mi"
}

func podTemplate(p Profile, r platform.SandboxRun, caps platform.SandboxCapabilities, seconds int64) (json.RawMessage, string, error) {
	profile, err := p.Fingerprint()
	if err != nil || r.Scope != p.Scope || profile != r.ProfileFingerprint || len(r.ID) != 64 ||
		r.PodName != "sandbox-"+r.ID[:32] || seconds < 1 || seconds > int64(task.Timeout.Seconds()) {
		return nil, "", ErrPod
	}
	u, _ := url.Parse(p.GatewayURL)
	args := []string{"guest-run", "--adapter", p.Scope.Adapter, "--runtime-cli", p.RuntimeExecutable,
		"--root", "/run/guest/run", "--gateway", p.GatewayURL, "--ca-file", p.GatewayCAPath,
		"--capabilities", "/run/capabilities", "--model", p.Model,
		"--input-sha256", r.Input.SHA256, "--input-size", strconv.FormatInt(r.Input.Size, 10),
		"--deadline", r.Deadline.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")}
	if p.FileTool != "" {
		args = append(args, "--file-tool", p.FileTool)
	}
	if p.APIProbePort != 0 {
		args = append(args, "--deny-api-address", net.JoinHostPort(p.GatewayIP, strconv.Itoa(p.APIProbePort)),
			"--deny-kube-address", p.KubeProbeAddress)
	}
	resources := map[string]any{"cpu": cpuQuantity(p.CPUMillis), "memory": memoryQuantity(p.MemoryMiB)}
	items := []any{}
	for _, item := range []struct{ path, annotation string }{{"input", inputAnnotation}, {"output", outputAnnotation}, {"model", modelAnnotation}} {
		items = append(items, map[string]any{"path": item.path, "mode": 0444,
			"fieldRef": map[string]any{"apiVersion": "v1", "fieldPath": "metadata.annotations['" + item.annotation + "']"}})
	}
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": r.PodName, "namespace": p.Namespace,
			"labels": map[string]string{"app.kubernetes.io/name": "agentmoor-sandbox", "agentmoor.io/run": r.ID[:32]},
			"annotations": map[string]string{inputAnnotation: caps.Input, outputAnnotation: caps.Output, modelAnnotation: caps.Model,
				"agentmoor.io/profile": profile, "agentmoor.io/record": r.RecordFingerprint}},
		"spec": map[string]any{
			"runtimeClassName": p.RuntimeClass, "restartPolicy": "Never", "activeDeadlineSeconds": seconds,
			"automountServiceAccountToken": false, "enableServiceLinks": false,
			"terminationGracePeriodSeconds": 5, "schedulerName": "default-scheduler", "preemptionPolicy": "Never", "priorityClassName": "agentmoor-sandbox",
			"dnsPolicy": "None", "dnsConfig": map[string]any{"nameservers": []string{"127.0.0.1"}},
			"hostAliases": []any{map[string]any{"ip": p.GatewayIP, "hostnames": []string{u.Hostname()}}},
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532,
				"fsGroup": 65532, "fsGroupChangePolicy": "OnRootMismatch", "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"containers": []any{map[string]any{
				"name": "guest", "image": p.Image, "imagePullPolicy": "IfNotPresent",
				"command":    []string{"/usr/local/bin/agentmoor"},
				"args":       args,
				"workingDir": "/run/guest", "terminationMessagePath": "/dev/termination-log", "terminationMessagePolicy": "File",
				"resources": map[string]any{"requests": resources, "limits": resources},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
					"privileged": false, "procMount": "Default", "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"volumeMounts": []any{
					map[string]any{"name": "work", "mountPath": "/run/guest"},
					map[string]any{"name": "tmp", "mountPath": "/tmp"},
					map[string]any{"name": "caps", "mountPath": "/run/capabilities", "readOnly": true}},
			}},
			"volumes": []any{
				map[string]any{"name": "work", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": memoryQuantity(p.WorkspaceMiB)}},
				map[string]any{"name": "tmp", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": memoryQuantity(p.TempMiB)}},
				map[string]any{"name": "caps", "downwardAPI": map[string]any{"defaultMode": 0444, "items": items}}},
		},
	}
	b, err := json.Marshal(pod)
	h := sha256.Sum256(b)
	return b, hex.EncodeToString(h[:]), err
}

// Decode maps rather than Go structs: field names are exact and duplicates at
// every depth are rejected, including security fields and metadata identity.
func jsonValue(d *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, ErrPod
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		m := make(map[string]any)
		for d.More() {
			k, err := d.Token()
			key, ok := k.(string)
			if err != nil || !ok {
				return nil, ErrPod
			}
			if _, duplicate := m[key]; duplicate {
				return nil, ErrPod
			}
			value, err := jsonValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			m[key] = value
		}
		_, err = d.Token()
		return m, err
	case '[':
		a := []any{}
		for d.More() {
			v, err := jsonValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err = d.Token()
		return a, err
	default:
		return nil, ErrPod
	}
}

func object(b []byte) (map[string]any, error) {
	if len(b) > maxResponseBytes {
		return nil, ErrPod
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	v, err := jsonValue(d, 0)
	m, ok := v.(map[string]any)
	if err != nil || !ok {
		return nil, ErrPod
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrPod
	}
	return m, nil
}

type observedPod struct {
	uid      string
	terminal bool
	exit     int
	deleting bool
}

func canonicalUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

func number(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// normalizeDefaults accepts only enumerated inert API defaults, never an
// arbitrary admission-injected container, env, volume, network or identity.
func normalizeDefaults(spec map[string]any) error {
	for key, want := range map[string]any{"serviceAccountName": "default", "serviceAccount": "default", "hostNetwork": false,
		"hostPID": false, "hostIPC": false, "shareProcessNamespace": false, "setHostnameAsFQDN": false, "hostUsers": true, "priority": json.Number("0")} {
		if got, ok := spec[key]; ok {
			if !reflect.DeepEqual(got, want) {
				return ErrPod
			}
			delete(spec, key)
		}
	}
	if node, ok := spec["nodeName"]; ok {
		if name, ok := node.(string); !ok || !dnsName(name, 253) {
			return ErrPod
		}
		delete(spec, "nodeName")
	}
	for _, key := range []string{"imagePullSecrets", "schedulingGates", "resourceClaims"} {
		if v, ok := spec[key]; ok {
			if a, ok := v.([]any); !ok || len(a) != 0 {
				return ErrPod
			}
			delete(spec, key)
		}
	}
	if v, ok := spec["tolerations"]; ok {
		a, ok := v.([]any)
		if !ok || len(a) > 2 {
			return ErrPod
		}
		seen := map[string]bool{}
		for _, item := range a {
			m, ok := item.(map[string]any)
			if !ok || len(m) != 4 || m["operator"] != "Exists" || m["effect"] != "NoExecute" || m["tolerationSeconds"] != json.Number("300") {
				return ErrPod
			}
			key, ok := m["key"].(string)
			if !ok || seen[key] || (key != "node.kubernetes.io/not-ready" && key != "node.kubernetes.io/unreachable") {
				return ErrPod
			}
			seen[key] = true
		}
		delete(spec, "tolerations")
	}
	return nil
}

// Calico v3.33 writes these informational values atomically during CNI ADD,
// then clears only the two IP values at teardown. They grant no authority;
// identity, capabilities and every spec field still match the frozen hash.
// Unknown annotations, including other Calico keys, remain in that hash.
func normalizeCalicoAnnotations(annotations map[string]any) error {
	const container = "cni.projectcalico.org/containerID"
	const single = "cni.projectcalico.org/podIP"
	const multiple = "cni.projectcalico.org/podIPs"
	_, hasContainer := annotations[container]
	_, hasSingle := annotations[single]
	_, hasMultiple := annotations[multiple]
	if !hasContainer && !hasSingle && !hasMultiple {
		return nil
	}
	id, validID := annotations[container].(string)
	ip, validIP := annotations[single].(string)
	ips, validIPs := annotations[multiple].(string)
	if !validID || !validIP || !validIPs || len(id) != 64 || len(ips) > 256 {
		return ErrPod
	}
	if decoded, err := hex.DecodeString(id); err != nil || len(decoded) != 32 || strings.ToLower(id) != id {
		return ErrPod
	}
	if ip != "" || ips != "" {
		parts := strings.Split(ips, ",")
		if len(parts) > 2 || ip != parts[0] {
			return ErrPod
		}
		seen := map[string]bool{}
		for _, address := range parts {
			parsed, network, err := net.ParseCIDR(address)
			if err != nil || parsed.IsUnspecified() || parsed.String() != network.IP.String() || seen[address] {
				return ErrPod
			}
			ones, bits := network.Mask.Size()
			if ones != bits || address != parsed.String()+"/"+strconv.Itoa(bits) {
				return ErrPod
			}
			seen[address] = true
		}
	}
	delete(annotations, container)
	delete(annotations, single)
	delete(annotations, multiple)
	return nil
}

func inspectPod(r platform.SandboxRun, raw []byte) (observedPod, platform.SandboxCapabilities, error) {
	var result observedPod
	var caps platform.SandboxCapabilities
	pod, err := object(raw)
	if err != nil || pod["apiVersion"] != "v1" || pod["kind"] != "Pod" {
		return result, caps, ErrPod
	}
	for k := range pod {
		if k != "apiVersion" && k != "kind" && k != "metadata" && k != "spec" && k != "status" {
			return result, caps, ErrPod
		}
	}
	meta, ok := pod["metadata"].(map[string]any)
	if !ok {
		return result, caps, ErrPod
	}
	uid, ok := meta["uid"].(string)
	if !ok || !canonicalUUID(uid) || (r.PodUID != "" && r.PodUID != uid) {
		return result, caps, ErrPod
	}
	result.uid = uid
	result.deleting = meta["deletionTimestamp"] != nil
	annotations, ok := meta["annotations"].(map[string]any)
	if !ok {
		return result, caps, ErrPod
	}
	caps.Input, _ = annotations[inputAnnotation].(string)
	caps.Output, _ = annotations[outputAnnotation].(string)
	caps.Model, _ = annotations[modelAnnotation].(string)
	if normalizeCalicoAnnotations(annotations) != nil {
		return result, caps, ErrPod
	}
	spec, ok := pod["spec"].(map[string]any)
	if !ok {
		return result, caps, ErrPod
	}
	for key := range meta {
		switch key {
		case "name", "namespace", "labels", "annotations":
		case "uid", "resourceVersion", "creationTimestamp", "generation", "managedFields", "deletionTimestamp", "deletionGracePeriodSeconds":
			delete(meta, key)
		default:
			return result, caps, ErrPod
		}
	}
	if meta["name"] != r.PodName || meta["namespace"] != r.PodNamespace || normalizeDefaults(spec) != nil {
		return result, caps, ErrPod
	}
	// Hash every controlled field, not ownership labels alone. In particular,
	// recovery uses the persisted namespace/template even after a profile edit.
	// No expired or revoked capability is being granted fresh authority here.
	delete(pod, "status")
	canonical, err := json.Marshal(pod)
	hash := sha256.Sum256(canonical)
	if err != nil || r.PodTemplateSHA == "" || hex.EncodeToString(hash[:]) != r.PodTemplateSHA {
		return result, caps, ErrPod
	}
	// Keep status separate from the template fingerprint.
	full, _ := object(raw)
	if status, ok := full["status"].(map[string]any); ok {
		phase, _ := status["phase"].(string)
		if phase == "Succeeded" || phase == "Failed" {
			result.terminal, result.exit = true, 255
			statuses, ok := status["containerStatuses"].([]any)
			if !ok || len(statuses) != 1 {
				if phase == "Failed" {
					return result, caps, nil
				}
				return result, caps, ErrPod
			}
			container, ok := statuses[0].(map[string]any)
			if !ok || container["name"] != "guest" || container["restartCount"] != json.Number("0") {
				return result, caps, ErrPod
			}
			state, _ := container["state"].(map[string]any)
			terminated, _ := state["terminated"].(map[string]any)
			exit, ok := number(terminated["exitCode"])
			if !ok || exit < 0 || exit > 255 || (phase == "Succeeded" && exit != 0) {
				return result, caps, ErrPod
			}
			result.exit = int(exit)
			if phase == "Failed" && result.exit == 0 {
				result.exit = 255 // an infrastructure failure is never publishable
			}
		}
	}
	return result, caps, nil
}
