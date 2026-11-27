package sandboxkube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const maxResponseBytes = 1 << 20
const maxTokenBytes = 16 << 10
const maxCABytes = 1 << 20

// ClientConfig is operator configuration, never a guest request.
type ClientConfig struct {
	URL, CAFile, TokenFile string
	Timeout                time.Duration
}

// Client exposes fixed Pod operations including a narrowly constructed
// conditional removal of only the platform's guest-stop finalizer.
// Mutations have no retry path, including transport replay.
type Client struct {
	origin, tokenFile string
	http              *http.Client
}

func NewClient(config ClientConfig) (*Client, error) {
	endpoint, err := url.Parse(config.URL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.Hostname() == "" || strings.HasSuffix(endpoint.Host, ":") || strings.Contains(config.URL, "#") || endpoint.User != nil || endpoint.Path != "" || endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.RawFragment != "" || endpoint.Opaque != "" {
		return nil, fmt.Errorf("invalid Kubernetes API origin")
	}
	if port := endpoint.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("invalid Kubernetes API origin")
		}
	}
	if config.TokenFile == "" {
		return nil, fmt.Errorf("Kubernetes token file is required")
	}
	if _, err = readToken(config.TokenFile); err != nil {
		return nil, err
	}
	if config.Timeout == 0 {
		config.Timeout = 5 * time.Second
	}
	if config.Timeout < 0 || config.Timeout > 30*time.Second {
		return nil, fmt.Errorf("invalid Kubernetes API timeout")
	}
	var roots *x509.CertPool
	if config.CAFile != "" {
		data, err := readRegularFile(config.CAFile, maxCABytes)
		if err != nil {
			return nil, fmt.Errorf("Kubernetes CA is unavailable")
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("invalid Kubernetes CA")
		}
	}
	dialer := &net.Dialer{Timeout: config.Timeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{DialContext: dialer.DialContext, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}, TLSHandshakeTimeout: config.Timeout, ResponseHeaderTimeout: config.Timeout, MaxResponseHeaderBytes: 64 << 10, IdleConnTimeout: 30 * time.Second}
	client := &http.Client{Transport: transport, Timeout: config.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{origin: config.URL, tokenFile: config.TokenFile, http: client}, nil
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("private file unavailable")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("private file unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("private file unavailable")
	}
	return data, nil
}
func readToken(path string) (string, error) {
	data, err := readRegularFile(path, maxTokenBytes)
	if err != nil {
		return "", fmt.Errorf("Kubernetes token file unavailable")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("invalid Kubernetes token file")
	}
	for _, b := range []byte(token) {
		if b <= 32 || b >= 127 {
			return "", fmt.Errorf("invalid Kubernetes token file")
		}
	}
	return token, nil
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

func namespaceValid(namespace string) bool {
	return len(namespace) > 0 && len(namespace) <= 63 && dnsLabel.MatchString(namespace)
}
func nameValid(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || !dnsLabel.MatchString(label) {
			return false
		}
	}
	return true
}
func podRoute(namespace, name string) (string, error) {
	if !namespaceValid(namespace) || (name != "" && !nameValid(name)) {
		return "", fmt.Errorf("invalid Kubernetes Pod address")
	}
	route := "/api/v1/namespaces/" + namespace + "/pods"
	if name != "" {
		route += "/" + name
	}
	return route, nil
}
func safeRequestError(operation string, ctx context.Context, err error) error {
	if cause := ctx.Err(); cause != nil {
		return fmt.Errorf("Kubernetes %s request cancelled: %w", operation, cause)
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return fmt.Errorf("Kubernetes %s request timed out: %w", operation, context.DeadlineExceeded)
	}
	return fmt.Errorf("Kubernetes %s request failed", operation)
}
func (c *Client) request(ctx context.Context, operation, method, route string, body []byte) (int, []byte, error) {
	token, err := readToken(c.tokenFile)
	if err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, c.origin+route, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("invalid Kubernetes %s request", operation)
	}
	// A nonempty mutation body with no GetBody cannot be transparently replayed
	// by net/http, even if an idle connection fails before its first write.
	request.GetBody = nil
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
		if method == http.MethodPatch {
			request.Header.Set("Content-Type", "application/json-patch+json")
		}
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, safeRequestError(operation, ctx, err)
	}
	defer response.Body.Close()
	if response.ContentLength > maxResponseBytes {
		return 0, nil, fmt.Errorf("Kubernetes %s response exceeds limit", operation)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, safeRequestError(operation, ctx, err)
	}
	if len(data) > maxResponseBytes {
		return 0, nil, fmt.Errorf("Kubernetes %s response exceeds limit", operation)
	}
	return response.StatusCode, data, nil
}

// releaseGuestFinalizer performs one atomic compare-and-remove. For an
// unscheduled Pod the complete observed spec is tested too, so a concurrent
// scheduler binding cannot authorize a never-scheduled stop-proof.
func (c *Client) releaseGuestFinalizer(ctx context.Context, namespace, name string, p observedPod) error {
	route, err := podRoute(namespace, name)
	if err != nil || !canonicalUUID(p.uid) || !podResourceVersion(p.resourceVersion) || !p.deleting || p.deletionTimestamp == "" || !p.ownFinalizer || len(p.finalizers) == 0 || len(p.finalizers) > 32 || p.casSpec == nil {
		return ErrPod
	}
	index := -1
	for i, value := range p.finalizers {
		if value == guestStopFinalizer {
			if index != -1 {
				return ErrPod
			}
			index = i
		}
	}
	if index < 0 {
		return ErrPod
	}
	type patch struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value any    `json:"value,omitempty"`
	}
	operations := []patch{{"test", "/metadata/uid", p.uid}, {"test", "/metadata/resourceVersion", p.resourceVersion},
		{"test", "/metadata/deletionTimestamp", p.deletionTimestamp}, {"test", "/metadata/finalizers", p.finalizers},
		{"test", "/spec", p.casSpec}, {"remove", "/metadata/finalizers/" + strconv.Itoa(index), nil}}
	body, err := json.Marshal(operations)
	if err != nil {
		return ErrPod
	}
	status, raw, err := c.request(ctx, "release guest finalizer", http.MethodPatch, route, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK || !podJSON(raw) {
		return fmt.Errorf("Kubernetes conditional guest finalizer release failed: HTTP %d", status)
	}
	var response struct {
		Metadata struct {
			UID        string   `json:"uid"`
			Finalizers []string `json:"finalizers"`
		} `json:"metadata"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Metadata.UID != p.uid {
		return ErrPod
	}
	for _, value := range response.Metadata.Finalizers {
		if value == guestStopFinalizer {
			return ErrPod
		}
	}
	return nil
}
func podJSON(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}

func (c *Client) Get(ctx context.Context, namespace, name string) (json.RawMessage, bool, error) {
	if !nameValid(name) {
		return nil, false, fmt.Errorf("invalid Kubernetes Pod address")
	}
	route, err := podRoute(namespace, name)
	if err != nil {
		return nil, false, err
	}
	status, data, err := c.request(ctx, "get", "GET", route, nil)
	if err != nil {
		return nil, false, err
	}
	if status == http.StatusNotFound {
		return nil, false, nil
	}
	if status != http.StatusOK {
		return nil, false, fmt.Errorf("Kubernetes get failed: HTTP %d", status)
	}
	if !podJSON(data) {
		return nil, false, fmt.Errorf("invalid Kubernetes get response")
	}
	return json.RawMessage(data), true, nil
}
func (c *Client) Create(ctx context.Context, namespace string, body json.RawMessage) (json.RawMessage, error) {
	route, err := podRoute(namespace, "")
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes || !podJSON(body) {
		return nil, fmt.Errorf("invalid Kubernetes create body")
	}
	status, data, err := c.request(ctx, "create", "POST", route, body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("Kubernetes create failed: HTTP %d", status)
	}
	if !podJSON(data) {
		return nil, fmt.Errorf("invalid Kubernetes create response")
	}
	return json.RawMessage(data), nil
}
func (c *Client) Delete(ctx context.Context, namespace, name, uid string) error {
	route, err := podRoute(namespace, name)
	if err != nil || !nameValid(name) {
		return fmt.Errorf("invalid Kubernetes Pod address")
	}
	id, err := uuid.Parse(uid)
	if err != nil || id == uuid.Nil || id.String() != uid {
		return fmt.Errorf("invalid Kubernetes Pod UID")
	}
	options := struct {
		APIVersion    string `json:"apiVersion"`
		Kind          string `json:"kind"`
		Preconditions struct {
			UID string `json:"uid"`
		} `json:"preconditions"`
		GracePeriodSeconds int `json:"gracePeriodSeconds"`
	}{APIVersion: "v1", Kind: "DeleteOptions", GracePeriodSeconds: 5}
	options.Preconditions.UID = uid
	body, err := json.Marshal(options)
	if err != nil {
		return fmt.Errorf("invalid Kubernetes delete body")
	}
	status, _, err := c.request(ctx, "delete", "DELETE", route, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusAccepted && status != http.StatusNotFound {
		return fmt.Errorf("Kubernetes delete failed: HTTP %d", status)
	}
	return nil
}

func InClusterClient() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if host == "" || port == "" {
		return nil, fmt.Errorf("Kubernetes in-cluster endpoint unavailable")
	}
	const serviceAccount = "/var/run/secrets/kubernetes.io/serviceaccount"
	return NewClient(ClientConfig{URL: "https://" + net.JoinHostPort(host, port), CAFile: filepath.Join(serviceAccount, "ca.crt"), TokenFile: filepath.Join(serviceAccount, "token")})
}
