// Package sandboxkube controls fixed, one-attempt gVisor Pods. It does not
// enable the application execution policy or attest a node's runtime/network.
package sandboxkube

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/intSheep/AgentMoor/internal/asset"
)

var ErrProfile = errors.New("invalid sandbox profile")

// Profile is operator configuration, never a task/customer template. The
// gateway CA and executables are preinstalled in the digest-pinned image.
// Both writable volumes are memory backed and charged to the memory limit.
type Profile struct {
	Scope             asset.Scope `json:"scope"`
	Namespace         string      `json:"namespace"`
	Image             string      `json:"image"`
	RuntimeClass      string      `json:"runtime_class"`
	GatewayURL        string      `json:"gateway_url"`
	GatewayIP         string      `json:"gateway_ip"`
	GatewayCAPath     string      `json:"gateway_ca_path"`
	RuntimeExecutable string      `json:"runtime_executable"`
	Model             string      `json:"model"`
	FileTool          string      `json:"file_tool,omitempty"`
	FilesystemClass   string      `json:"filesystem_class"`
	CPUMillis         int64       `json:"cpu_millis"`
	MemoryMiB         int64       `json:"memory_mib"`
	WorkspaceMiB      int64       `json:"workspace_mib"`
	TempMiB           int64       `json:"temp_mib"`
	APIProbePort      int         `json:"api_probe_port,omitempty"`
	KubeProbeAddress  string      `json:"kubernetes_probe_address,omitempty"`
}

var dnsPart = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
var imageName = regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]*@sha256:[a-f0-9]{64}$`)
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

func dnsName(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if len(part) > 63 || !dnsPart.MatchString(part) {
			return false
		}
	}
	return true
}

func imagePath(s string) bool {
	if !strings.HasPrefix(s, "/") || path.Clean(s) != s || s == "/" || strings.ContainsAny(s, "\\\x00\r\n\t ") {
		return false
	}
	for _, mount := range []string{"/run", "/var/run", "/tmp", "/proc", "/sys", "/dev"} {
		if s == mount || strings.HasPrefix(s, mount+"/") {
			return false
		}
	}
	return s != "/etc/hosts" && s != "/etc/hostname" && s != "/etc/resolv.conf"
}

func (p Profile) Validate() error {
	u, err := url.Parse(p.GatewayURL)
	if !p.Scope.Valid() || p.Scope.Adapter != "codex" || !dnsName(p.Namespace, 63) || strings.Contains(p.Namespace, ".") ||
		!imageName.MatchString(p.Image) || len(p.Image) > 512 || !dnsName(p.RuntimeClass, 253) ||
		err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" ||
		!dnsName(u.Hostname(), 253) || strings.HasSuffix(u.Host, ":") || u.ForceQuery || net.ParseIP(u.Hostname()) != nil || net.ParseIP(p.GatewayIP) == nil ||
		!imagePath(p.GatewayCAPath) || !imagePath(p.RuntimeExecutable) ||
		!modelName.MatchString(p.Model) || (p.FileTool != "" && !imagePath(p.FileTool)) || p.FilesystemClass != "gvisor-overlay2-memory.v1" ||
		p.CPUMillis < 100 || p.CPUMillis > 32000 || p.MemoryMiB < 128 || p.MemoryMiB > 32768 ||
		p.WorkspaceMiB < 32 || p.TempMiB < 16 || p.WorkspaceMiB > p.MemoryMiB || p.TempMiB > p.MemoryMiB || p.WorkspaceMiB+p.TempMiB > p.MemoryMiB {
		return ErrProfile
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return ErrProfile
		}
	}
	if p.APIProbePort != 0 || p.KubeProbeAddress != "" {
		port := 443
		if u.Port() != "" {
			port, _ = strconv.Atoi(u.Port())
		}
		host, kubePort, err := net.SplitHostPort(p.KubeProbeAddress)
		n, parseErr := strconv.Atoi(kubePort)
		ip := net.ParseIP(host)
		if p.APIProbePort < 1 || p.APIProbePort > 65535 || p.APIProbePort == port ||
			err != nil || parseErr != nil || n < 1 || n > 65535 || ip == nil || ip.IsUnspecified() {
			return ErrProfile
		}
	}
	return nil
}

// Fingerprint includes every operator-selected field, including the Scope.
func (p Profile) Fingerprint() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
