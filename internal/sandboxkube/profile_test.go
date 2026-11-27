package sandboxkube

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestProfileRejectsUnboundedAndUnpinnedConfiguration(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*Profile)
	}{
		{"scope", func(p *Profile) { p.Scope.Runtime = "../other" }},
		{"unsupported-adapter", func(p *Profile) { p.Scope.Adapter = "opencode" }},
		{"namespace", func(p *Profile) { p.Namespace = "other/namespace" }},
		{"tag-image", func(p *Profile) { p.Image = "registry.example/guest:latest" }},
		{"upper-digest", func(p *Profile) { p.Image = "registry.example/guest@sha256:" + strings.Repeat("A", 64) }},
		{"runtime", func(p *Profile) { p.RuntimeClass = "" }},
		{"gateway-http", func(p *Profile) { p.GatewayURL = "http://gateway.example:7443" }},
		{"gateway-user", func(p *Profile) { p.GatewayURL = "https://secret@gateway.example" }},
		{"gateway-query", func(p *Profile) { p.GatewayURL += "?token=secret" }},
		{"gateway-path", func(p *Profile) { p.GatewayURL += "/main-api" }},
		{"gateway-ip-host", func(p *Profile) { p.GatewayURL = "https://10.20.0.9:7443" }},
		{"gateway-no-ip", func(p *Profile) { p.GatewayIP = "resolve-me.example" }},
		{"ca-host-path", func(p *Profile) { p.GatewayCAPath = "../ca.pem" }},
		{"runtime-relative", func(p *Profile) { p.RuntimeExecutable = "codex" }},
		{"file-tool-relative", func(p *Profile) { p.FileTool = "tool" }},
		{"model", func(p *Profile) { p.Model = "model --host other" }},
		{"cpu", func(p *Profile) { p.CPUMillis = 0 }},
		{"memory", func(p *Profile) { p.MemoryMiB = 32769 }},
		{"tmpfs", func(p *Profile) { p.WorkspaceMiB = p.MemoryMiB }},
		{"tmpfs-overflow", func(p *Profile) { p.WorkspaceMiB, p.TempMiB = math.MaxInt64, math.MaxInt64 }},
		{"unverified-filesystem", func(p *Profile) { p.FilesystemClass = "emptydir-memory" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			p := testProfile()
			change.edit(&p)
			if p.Validate() == nil {
				t.Fatal("unsafe profile admitted")
			}
		})
	}
}

func TestProfileRejectsExecutablesAndCAOnNonImageMounts(t *testing.T) {
	for _, location := range []string{"/run/guest/tool", "/run/capabilities/input", "/tmp/tool", "/var/run/tool", "/proc/self/exe", "/sys/fs/tool", "/dev/fd/3", "/etc/hosts", "/etc/hostname", "/etc/resolv.conf"} {
		for _, field := range []string{"ca", "runtime", "file-tool"} {
			t.Run(field+location, func(t *testing.T) {
				p := testProfile()
				switch field {
				case "ca":
					p.GatewayCAPath = location
				case "runtime":
					p.RuntimeExecutable = location
				case "file-tool":
					p.FileTool = location
				}
				if p.Validate() == nil {
					t.Fatal("mutable/nonimage path admitted")
				}
			})
		}
	}
}

func TestProfileFingerprintBindsEveryField(t *testing.T) {
	p := testProfile()
	p.FileTool = "/usr/local/bin/file-tool"
	h, err := p.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	var fields map[string]json.RawMessage
	json.Unmarshal(b, &fields)
	// Valid alternative choices for every field must change the identity.
	for _, edit := range []func(*Profile){
		func(p *Profile) { p.Scope.Runtime = "other" }, func(p *Profile) { p.Namespace = "other" },
		func(p *Profile) { p.Image = "other.example/image@sha256:" + strings.Repeat("b", 64) },
		func(p *Profile) { p.RuntimeClass = "runsc" }, func(p *Profile) { p.GatewayURL = "https://other.example:7443" },
		func(p *Profile) { p.GatewayIP = "10.20.0.10" }, func(p *Profile) { p.GatewayCAPath = "/etc/other/ca.pem" },
		func(p *Profile) { p.RuntimeExecutable = "/usr/bin/codex" }, func(p *Profile) { p.Model = "other" }, func(p *Profile) { p.FileTool = "/usr/bin/file-tool" },
		func(p *Profile) { p.CPUMillis = 2000 }, func(p *Profile) { p.MemoryMiB = 2048 }, func(p *Profile) { p.WorkspaceMiB = 256 }, func(p *Profile) { p.TempMiB = 64 },
	} {
		q := p
		edit(&q)
		got, err := q.Fingerprint()
		if err != nil || got == h {
			t.Fatal("valid profile change did not change identity", err)
		}
	}
	if len(fields) != 15 {
		t.Fatal("update fingerprint coverage for new profile fields", len(fields))
	}
}
