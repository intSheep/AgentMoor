// Package kubejobs dispatches durable turns using operator-provided Pod specs.
package kubejobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/platform"
)

const serviceAccount = "/var/run/secrets/kubernetes.io/serviceaccount"

type Profile struct {
	Scope   asset.Scope     `json:"scope"`
	PodSpec json.RawMessage `json:"pod_spec"`
}
type Dispatcher struct {
	profiles           map[asset.Scope]Profile
	client             *http.Client
	jobsURL, tokenFile string
}

func InCluster(file string) (*Dispatcher, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var profiles []Profile
	if err = json.Unmarshal(b, &profiles); err != nil {
		return nil, err
	}
	ns, err := os.ReadFile(filepath.Join(serviceAccount, "namespace"))
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(filepath.Join(serviceAccount, "ca.crt"))
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("invalid Kubernetes CA")
	}
	host := net.JoinHostPort(os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	d := &Dispatcher{
		profiles:  make(map[asset.Scope]Profile, len(profiles)),
		jobsURL:   "https://" + host + "/apis/batch/v1/namespaces/" + strings.TrimSpace(string(ns)) + "/jobs",
		tokenFile: filepath.Join(serviceAccount, "token"),
		client: &http.Client{
			Transport:     transport,
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("empty turn Job configuration")
	}
	for _, p := range profiles {
		if !p.Scope.Valid() || d.Enabled(p.Scope) {
			return nil, fmt.Errorf("invalid or duplicate Job scope")
		}
		if _, err := p.job("validation"); err != nil {
			return nil, err
		}
		d.profiles[p.Scope] = p
	}
	return d, nil
}

func (d *Dispatcher) Enabled(s asset.Scope) bool {
	_, ok := d.profiles[s]
	return ok
}

func identity(s asset.Scope, id string) string {
	digest := sha256.Sum256([]byte(s.Prefix() + "/" + id))
	return fmt.Sprintf("turn-%x", digest[:16])
}

func (p Profile) job(id string) ([]byte, error) {
	var pod map[string]json.RawMessage
	if err := json.Unmarshal(p.PodSpec, &pod); err != nil {
		return nil, err
	}
	var containers []map[string]json.RawMessage
	if err := json.Unmarshal(pod["containers"], &containers); err != nil {
		return nil, err
	}
	found := false
	for _, c := range containers {
		if string(c["name"]) != `"worker"` {
			continue
		}
		found = true
		c["command"], _ = json.Marshal([]string{"agentmoor", "run-task", "--config", "/config/worker.json", "--turn", id})
		delete(c, "args")
	}
	if !found {
		return nil, fmt.Errorf("turn Job profile requires a worker container")
	}
	pod["containers"], _ = json.Marshal(containers)
	pod["restartPolicy"] = json.RawMessage(`"Never"`)
	pod["automountServiceAccountToken"] = json.RawMessage("false")
	name := identity(p.Scope, id)
	labels := map[string]string{"agentmoor.io/turn": id, "agentmoor.io/allocation": name}
	return json.Marshal(map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"name": name, "labels": labels},
		"spec": map[string]any{
			"backoffLimit":            0,
			"activeDeadlineSeconds":   900,
			"ttlSecondsAfterFinished": 600,
			"template":                map[string]any{"metadata": map[string]any{"labels": labels}, "spec": pod},
		},
	})
}

func (d *Dispatcher) request(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	token, err := os.ReadFile(d.tokenFile)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, d.jobsURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	return d.client.Do(req)
}

func (d *Dispatcher) ensure(ctx context.Context, s asset.Scope, id string) (bool, error) {
	name := identity(s, id)
	r, err := d.request(ctx, "GET", "/"+name, nil)
	if err != nil {
		return false, err
	}
	defer r.Body.Close()
	switch r.StatusCode {
	case http.StatusNotFound:
		body, err := d.profiles[s].job(id)
		if err != nil {
			return false, err
		}
		created, err := d.request(ctx, "POST", "", body)
		if err != nil {
			return false, err
		}
		defer created.Body.Close()
		switch created.StatusCode {
		case http.StatusCreated, http.StatusConflict:
			return false, nil
		case http.StatusUnprocessableEntity:
			log.Printf("invalid turn Job for %s: HTTP %d", s.Prefix(), created.StatusCode)
			return true, nil
		default:
			return false, fmt.Errorf("create Job: HTTP %d", created.StatusCode)
		}
	case http.StatusOK:
		var job struct {
			Metadata struct{ Labels map[string]string }
			Status   struct{ Succeeded, Failed int }
		}
		if err = json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&job); err != nil {
			return false, err
		}
		if job.Metadata.Labels["agentmoor.io/allocation"] != name || job.Metadata.Labels["agentmoor.io/turn"] != id {
			return true, nil
		}
		return job.Status.Failed > 0 || job.Status.Succeeded > 0, nil
	default:
		return false, fmt.Errorf("get Job: HTTP %d", r.StatusCode)
	}
}

func (d *Dispatcher) Run(ctx context.Context, c *platform.Catalog) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		for scope := range d.profiles {
			if ctx.Err() != nil {
				return
			}
			if err := c.DispatchQueued(ctx, scope, d.ensure); err != nil && ctx.Err() == nil {
				log.Printf("dispatch %s: %v", scope.Prefix(), err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
