package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"agentmoor/internal/adapter"
	"agentmoor/internal/asset"
	"agentmoor/internal/platform"
	"agentmoor/internal/sandboxgateway"
	"agentmoor/internal/sandboxkube"
)

type sandboxModelConfig struct {
	UpstreamURL           string `json:"upstream_url"`
	APIKeyFile            string `json:"api_key_file"`
	CAFile                string `json:"ca_file"`
	MaxRequests           int64  `json:"max_requests"`
	MaxInputBytes         int64  `json:"max_input_bytes"`
	MaxOutputBytes        int64  `json:"max_output_bytes"`
	PerRequestOutputBytes int64  `json:"per_request_output_bytes"`
	Concurrency           int    `json:"concurrency"`
	MaxOutputTokens       int    `json:"max_output_tokens"`
}

type sandboxAllocation struct {
	Execution sandboxkube.Profile `json:"execution"`
	Model     sandboxModelConfig  `json:"model"`
}

type sandboxDeployment struct {
	Version       string `json:"version"`
	NetworkPolicy string `json:"network_policy"`
	GatewayListen string `json:"gateway_listen"`
	Kubernetes    struct {
		URL          string `json:"url"`
		CAFile       string `json:"ca_file"`
		TokenFile    string `json:"token_file"`
		ProbeAddress string `json:"probe_address"`
	} `json:"kubernetes"`
	Profiles []sandboxAllocation `json:"profiles"`
}

type sandboxRuntime struct {
	controller *sandboxkube.Controller
	gateway    *sandboxgateway.Gateway
	listen     string
	selections map[asset.Scope]platform.SandboxExecutionProfile
}

func sandboxConfigJSON(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func(int) error
	nodes := 0
	walk = func(depth int) error {
		nodes++
		if depth > 32 || nodes > 131072 {
			return fmt.Errorf("sandbox configuration exceeds structural bounds")
		}
		token, err := d.Token()
		if err != nil || token == nil {
			return fmt.Errorf("invalid sandbox configuration")
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				token, err := d.Token()
				key, ok := token.(string)
				if err != nil || !ok || key == "" || seen[key] {
					return fmt.Errorf("duplicate sandbox configuration member")
				}
				for _, c := range key {
					if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
						return fmt.Errorf("noncanonical sandbox configuration member")
					}
				}
				seen[key] = true
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid sandbox configuration structure")
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing sandbox configuration")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

func newSandboxRuntime(ctx context.Context, path, apiListen string, registry *adapter.Registry, catalog *platform.Catalog) (*sandboxRuntime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("sandbox configuration is unavailable")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("sandbox configuration must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4<<20+1))
	if err != nil || len(raw) > 4<<20 {
		return nil, fmt.Errorf("sandbox configuration exceeds its bound")
	}
	var config sandboxDeployment
	if err = sandboxConfigJSON(raw, &config); err != nil {
		return nil, err
	}
	if config.Version != "sandbox-deployment.v1" || config.NetworkPolicy != "gateway-only.v1" || len(config.Profiles) == 0 || len(config.Profiles) > sandboxgateway.MaxProfiles {
		return nil, fmt.Errorf("unsupported sandbox deployment or missing network policy contract")
	}
	_, apiPortText, err := net.SplitHostPort(apiListen)
	apiPort, parseErr := strconv.Atoi(apiPortText)
	_, gatewayPortText, gatewayErr := net.SplitHostPort(config.GatewayListen)
	gatewayPort, portErr := strconv.Atoi(gatewayPortText)
	if err != nil || parseErr != nil || apiPort < 1 || apiPort > 65535 || gatewayErr != nil || portErr != nil || gatewayPort < 1 || gatewayPort > 65535 || apiPort == gatewayPort {
		return nil, fmt.Errorf("sandbox requires distinct, fixed API and TLS gateway ports")
	}
	serviceAccount := "/var/run/secrets/kubernetes.io/serviceaccount"
	if config.Kubernetes.URL == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
		if port == "" {
			port = "443"
		}
		if net.ParseIP(host) == nil {
			return nil, fmt.Errorf("sandbox requires a trusted Kubernetes API origin")
		}
		config.Kubernetes.URL = "https://" + net.JoinHostPort(host, port)
		if config.Kubernetes.ProbeAddress == "" {
			config.Kubernetes.ProbeAddress = net.JoinHostPort(host, port)
		}
	}
	if config.Kubernetes.CAFile == "" {
		config.Kubernetes.CAFile = filepath.Join(serviceAccount, "ca.crt")
	}
	if config.Kubernetes.TokenFile == "" {
		config.Kubernetes.TokenFile = filepath.Join(serviceAccount, "token")
	}
	kubeHost, kubePortText, err := net.SplitHostPort(config.Kubernetes.ProbeAddress)
	kubePort, portErr := strconv.Atoi(kubePortText)
	if err != nil || portErr != nil || net.ParseIP(kubeHost) == nil || kubePort < 1 || kubePort > 65535 {
		return nil, fmt.Errorf("sandbox requires the actual Kubernetes Service canary")
	}
	runtime := &sandboxRuntime{listen: config.GatewayListen, selections: make(map[asset.Scope]platform.SandboxExecutionProfile)}
	var profiles []sandboxkube.Profile
	modelConfig := &sandboxgateway.Config{}
	for _, allocation := range config.Profiles {
		p := allocation.Execution
		gatewayOrigin, parseErr := url.Parse(p.GatewayURL)
		publishedPort := 443
		if parseErr == nil && gatewayOrigin.Port() != "" {
			publishedPort, parseErr = strconv.Atoi(gatewayOrigin.Port())
		}
		if parseErr != nil || publishedPort != gatewayPort {
			return nil, fmt.Errorf("sandbox gateway Service must preserve the TLS listener port")
		}
		if p.APIProbePort != 0 && p.APIProbePort != apiPort || p.KubeProbeAddress != "" && p.KubeProbeAddress != config.Kubernetes.ProbeAddress {
			return nil, fmt.Errorf("sandbox canaries differ from trusted service configuration")
		}
		p.APIProbePort, p.KubeProbeAddress = apiPort, config.Kubernetes.ProbeAddress
		fingerprint, err := p.Fingerprint()
		if err != nil {
			return nil, err
		}
		a, err := registry.Lookup(p.Scope.Adapter)
		if err != nil {
			return nil, err
		}
		descriptor := a.Descriptor()
		if _, exists := runtime.selections[p.Scope]; exists || descriptor.TestedRuntime == "" {
			return nil, fmt.Errorf("duplicate or unverified sandbox allocation")
		}
		runtime.selections[p.Scope] = platform.SandboxExecutionProfile{Fingerprint: fingerprint, AdapterID: descriptor.ID, AdapterVersion: descriptor.Version, RuntimeVersion: descriptor.TestedRuntime}
		profiles = append(profiles, p)
		m := allocation.Model
		modelConfig.Profiles = append(modelConfig.Profiles, sandboxgateway.Profile{Scope: p.Scope, ExecutionProfileSHA256: fingerprint, Model: p.Model,
			UpstreamURL: m.UpstreamURL, APIKeyFile: m.APIKeyFile, CAFile: m.CAFile,
			MaxRequests: m.MaxRequests, MaxInputBytes: m.MaxInputBytes, MaxOutputBytes: m.MaxOutputBytes, PerRequestOutputBytes: m.PerRequestOutputBytes,
			ModelConcurrency: m.Concurrency, MaxOutputTokens: m.MaxOutputTokens})
	}
	runtime.controller, err = sandboxkube.New(sandboxkube.ClientConfig{URL: config.Kubernetes.URL, CAFile: config.Kubernetes.CAFile, TokenFile: config.Kubernetes.TokenFile}, profiles, catalog)
	if err != nil {
		return nil, err
	}
	runtime.gateway, err = sandboxgateway.New(catalog, modelConfig)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		runtime.gateway.Close()
		return nil, err
	}
	return runtime, nil
}

func (s *sandboxRuntime) selection(scope asset.Scope) (platform.SandboxExecutionProfile, bool) {
	p, ok := s.selections[scope]
	return p, ok
}

func (s *sandboxRuntime) reconcile(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if err := s.controller.ReconcileAll(ctx); err != nil && ctx.Err() == nil {
			log.Printf("sandbox reconciliation failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
