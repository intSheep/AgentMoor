// Package sandboxgateway exposes a dedicated capability-only TLS surface. It
// never mounts the platform API and does not establish a sandbox by itself.
package sandboxgateway

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/httptrace"
	"github.com/intSheep/AgentMoor/internal/platform"
)

const MaxProfiles = 1024
const MaxOutputTokens = 16384
const authorityInterval = 50 * time.Millisecond

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// Profile is trusted deployment configuration. Guests select none of these
// fields. The API key remains on this side and is re-read for key rotation.
type Profile struct {
	Scope                  asset.Scope `json:"scope"`
	ExecutionProfileSHA256 string      `json:"execution_profile_sha256"`
	Model                  string      `json:"model"`
	UpstreamURL            string      `json:"upstream_url"`
	APIKeyFile             string      `json:"api_key_file"`
	CAFile                 string      `json:"ca_file"`
	MaxRequests            int64       `json:"max_requests"`
	MaxInputBytes          int64       `json:"max_input_bytes"`
	MaxOutputBytes         int64       `json:"max_output_bytes"`
	PerRequestOutputBytes  int64       `json:"per_request_output_bytes"`
	ModelConcurrency       int         `json:"model_concurrency"`
	MaxOutputTokens        int         `json:"max_output_tokens"`
}

type Config struct {
	Profiles   []Profile      `json:"profiles"`
	RequestLog httptrace.Sink `json:"-"`
}

type boundProfile struct {
	profile        Profile
	policy         platform.SandboxModelPolicy
	client         *http.Client
	transport      *http.Transport
	handoff, model chan struct{}
}

type Gateway struct {
	catalog    *platform.Catalog
	profiles   map[string]*boundProfile
	active     chan struct{}
	requestLog httptrace.Sink
}

func profileKey(scope asset.Scope, fingerprint string) string {
	return scope.Prefix() + ":" + fingerprint
}

func New(catalog *platform.Catalog, config *Config) (*Gateway, error) {
	if catalog == nil || config == nil || len(config.Profiles) == 0 || len(config.Profiles) > MaxProfiles {
		return nil, fmt.Errorf("invalid sandbox gateway configuration")
	}
	g := &Gateway{catalog: catalog, profiles: make(map[string]*boundProfile), active: make(chan struct{}, 32), requestLog: config.RequestLog}
	for _, p := range config.Profiles {
		u, err := url.Parse(p.UpstreamURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || !validUpstreamPort(u) || u.User != nil || u.Path != "/v1/responses" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.String() != p.UpstreamURL ||
			!p.Scope.Valid() || !digestPattern.MatchString(p.ExecutionProfileSHA256) || !modelPattern.MatchString(p.Model) || p.ModelConcurrency < 1 || p.ModelConcurrency > 4 || p.MaxOutputTokens < 1 || p.MaxOutputTokens > MaxOutputTokens ||
			p.MaxRequests < 1 || p.MaxRequests > platform.SandboxModelMaxRequests || p.MaxInputBytes < 1 || p.MaxInputBytes > platform.SandboxModelMaxInputBytes || p.MaxOutputBytes < 1 || p.MaxOutputBytes > platform.SandboxModelMaxOutputBytes || p.PerRequestOutputBytes < 1 || p.PerRequestOutputBytes > platform.SandboxModelMaxRequestOutput || p.PerRequestOutputBytes > p.MaxOutputBytes {
			g.Close()
			return nil, fmt.Errorf("invalid sandbox model profile")
		}
		if _, err = readKey(p.APIKeyFile); err != nil {
			g.Close()
			return nil, err
		}
		ca, err := readConfiguration(p.CAFile, 1<<20)
		if err != nil {
			g.Close()
			return nil, fmt.Errorf("sandbox upstream CA unavailable")
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			g.Close()
			return nil, fmt.Errorf("invalid sandbox upstream CA")
		}
		key := profileKey(p.Scope, p.ExecutionProfileSHA256)
		if g.profiles[key] != nil {
			g.Close()
			return nil, fmt.Errorf("duplicate sandbox model profile")
		}
		raw, _ := json.Marshal(p)
		sum := sha256.Sum256(raw)
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 32 << 10, DisableKeepAlives: true, DisableCompression: true, MaxConnsPerHost: p.ModelConcurrency}
		g.profiles[key] = &boundProfile{profile: p, policy: platform.SandboxModelPolicy{Fingerprint: hex.EncodeToString(sum[:]), MaxRequests: p.MaxRequests, MaxInputBytes: p.MaxInputBytes, MaxOutputBytes: p.MaxOutputBytes, PerRequestOutputBytes: p.PerRequestOutputBytes},
			transport: transport, client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, handoff: make(chan struct{}, 2), model: make(chan struct{}, p.ModelConcurrency)}
	}
	return g, nil
}

func (g *Gateway) Close() {
	if g != nil {
		for _, p := range g.profiles {
			p.transport.CloseIdleConnections()
		}
	}
}

// NewTLSServer accepts only this dedicated gateway, never an arbitrary platform
// handler. HTTP/1.1 keeps actual socket read/write deadlines available. Call
// ListenAndServeTLS with the operator's certificate; plain HTTP is rejected.
func NewTLSServer(address string, gateway *Gateway) *http.Server {
	return &http.Server{Addr: address, Handler: gateway, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}, MaxHeaderBytes: 32 << 10, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
}

func validUpstreamPort(u *url.URL) bool {
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		return err == nil && n >= 1 && n <= 65535
	}
	return true
}

func readConfiguration(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("invalid gateway configuration path")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("gateway configuration unavailable")
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("gateway configuration is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, fmt.Errorf("gateway configuration exceeds its bound")
	}
	return raw, nil
}
func readKey(path string) (string, error) {
	raw, err := readConfiguration(path, 4096)
	if err != nil {
		return "", fmt.Errorf("sandbox upstream key unavailable")
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("invalid sandbox upstream key")
	}
	for _, b := range []byte(key) {
		if b < 33 || b > 126 {
			return "", fmt.Errorf("invalid sandbox upstream key")
		}
	}
	return key, nil
}
func bearer(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) != 55 || !strings.HasPrefix(values[0], "Bearer sbx1.") {
		return "", false
	}
	token := values[0][7:]
	if strings.Trim(token[5:], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
		return "", false
	}
	return token, true
}
func replyError(w http.ResponseWriter, status int) {
	http.Error(w, "sandbox gateway request rejected", status)
}
func catalogStatus(err error) int {
	switch {
	case errors.Is(err, platform.ErrSandboxDenied):
		return http.StatusUnauthorized
	case errors.Is(err, platform.ErrSandboxModelBudget):
		return http.StatusTooManyRequests
	case errors.Is(err, platform.ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusServiceUnavailable
	}
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var sink httptrace.Sink
	if g != nil {
		sink = g.requestLog
	}
	httptrace.Handler("sandbox-gateway", http.HandlerFunc(g.serveHTTP), sink).ServeHTTP(w, r)
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if g == nil || r.TLS == nil || r.TLS.Version < tls.VersionTLS12 {
		replyError(w, 503)
		return
	}
	if r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Fragment != "" || r.URL.Scheme != "" || r.URL.Host != "" || r.RequestURI != r.URL.Path {
		replyError(w, 404)
		return
	}
	var purpose platform.SandboxPurpose
	switch r.Method + " " + r.URL.Path {
	case "GET /input":
		purpose = platform.SandboxInput
		httptrace.Route(r.Context(), "GET /input")
	case "PUT /output":
		purpose = platform.SandboxOutput
		httptrace.Route(r.Context(), "PUT /output")
	case "POST /v1/responses":
		purpose = platform.SandboxModel
		httptrace.Route(r.Context(), "POST /v1/responses")
	default:
		replyError(w, 404)
		return
	}
	headerBytes := len(r.RequestURI) + len(r.Method) + len(r.Host) + 16
	for name, values := range r.Header {
		for _, value := range values {
			headerBytes += len(name) + len(value) + 4
		}
	}
	if headerBytes > 32<<10 {
		replyError(w, http.StatusRequestHeaderFieldsTooLarge)
		return
	}
	if len(r.Header.Values("Content-Encoding")) != 0 || (purpose == platform.SandboxInput && (r.ContentLength > 0 || len(r.TransferEncoding) > 0)) {
		replyError(w, 400)
		return
	}
	select {
	case g.active <- struct{}{}:
		defer func() { <-g.active }()
	default:
		replyError(w, 429)
		return
	}
	token, ok := bearer(r)
	if !ok {
		replyError(w, 401)
		return
	}
	run, err := g.catalog.AuthenticateSandboxCapability(r.Context(), token, purpose)
	if err != nil {
		replyError(w, catalogStatus(err))
		return
	}
	httptrace.Capability(r.Context(), run.Scope, run.ID, run.TurnID, string(purpose))
	profile := g.profiles[profileKey(run.Scope, run.ProfileFingerprint)]
	if profile == nil {
		replyError(w, 403)
		return
	}
	semaphore := profile.handoff
	if purpose == platform.SandboxModel {
		semaphore = profile.model
	}
	select {
	case semaphore <- struct{}{}:
		defer func() { <-semaphore }()
	default:
		replyError(w, 429)
		return
	}
	op, err := g.begin(w, r, run, token, purpose)
	if err != nil {
		replyError(w, 503)
		return
	}
	defer op.close()
	switch purpose {
	case platform.SandboxInput:
		g.input(w, op, token)
	case platform.SandboxOutput:
		g.output(w, r.WithContext(op.ctx), op, token)
	case platform.SandboxModel:
		g.model(w, r.WithContext(op.ctx), op, token, profile)
	}
}

type operation struct {
	ctx         context.Context
	parent      context.Context
	cancel      context.CancelFunc
	controller  *http.ResponseController
	stop, done  chan struct{}
	deadline    time.Time
	interrupted atomic.Bool
}

func (g *Gateway) begin(w http.ResponseWriter, r *http.Request, run platform.SandboxRun, token string, purpose platform.SandboxPurpose) (*operation, error) {
	ctx, cancel := context.WithDeadline(r.Context(), run.Deadline)
	op := &operation{ctx: ctx, parent: r.Context(), cancel: cancel, controller: http.NewResponseController(w), stop: make(chan struct{}), done: make(chan struct{}), deadline: run.Deadline}
	if op.controller.SetReadDeadline(run.Deadline) != nil || op.controller.SetWriteDeadline(run.Deadline) != nil {
		cancel()
		return nil, fmt.Errorf("socket deadlines unavailable")
	}
	go func() {
		defer close(op.done)
		ticker := time.NewTicker(authorityInterval)
		defer ticker.Stop()
		interrupt := func() {
			select {
			case <-op.stop:
				return
			default:
			}
			op.interrupted.Store(true)
			cancel()
			// net/http can call tls.Conn.Close after a failed write. TLS
			// close-notify resets its write deadline to five seconds in the
			// future; keep enforcing revocation until the handler unwinds.
			for {
				_ = op.controller.SetReadDeadline(time.Now())
				_ = op.controller.SetWriteDeadline(time.Now())
				select {
				case <-op.stop:
					return
				case <-ticker.C:
				}
			}
		}
		for {
			select {
			case <-op.stop:
				return
			case <-ctx.Done():
				interrupt()
				return
			case <-ticker.C:
				current, err := g.catalog.AuthenticateSandboxCapability(ctx, token, purpose)
				if err != nil || current.ID != run.ID || current.ProfileFingerprint != run.ProfileFingerprint {
					interrupt()
					return
				}
			}
		}
	}()
	return op, nil
}
func (op *operation) close() {
	close(op.stop)
	op.cancel() // Also interrupt a monitoring SQL read before waiting for it.
	<-op.done
	if !op.interrupted.Load() && op.parent.Err() == nil && time.Now().Before(op.deadline) {
		// Normal completion leaves time for net/http to flush the final 204/chunk;
		// it must not look like a revoked capability to the monitoring goroutine.
		until := time.Now().Add(10 * time.Second)
		if op.deadline.Before(until) {
			until = op.deadline
		}
		_ = op.controller.SetReadDeadline(until)
		_ = op.controller.SetWriteDeadline(until)
	}
}

func (g *Gateway) input(w http.ResponseWriter, op *operation, token string) {
	blob, body, err := g.catalog.ReadSandboxInput(op.ctx, token)
	if err != nil {
		replyError(w, catalogStatus(err))
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Length", strconv.FormatInt(blob.Size, 10))
	w.Header().Set("X-Agentmoor-SHA256", blob.SHA256)
	w.Header().Set("X-Agentmoor-Size", strconv.FormatInt(blob.Size, 10))
	if _, err = io.Copy(w, body); err != nil {
		panic(http.ErrAbortHandler)
	}
	if err = body.Close(); err != nil {
		panic(http.ErrAbortHandler)
	}
}
func (g *Gateway) output(w http.ResponseWriter, r *http.Request, op *operation, token string) {
	shas, sizes := r.Header.Values("X-Agentmoor-SHA256"), r.Header.Values("X-Agentmoor-Size")
	if len(shas) != 1 || !digestPattern.MatchString(shas[0]) || len(sizes) != 1 {
		replyError(w, 400)
		return
	}
	size, err := strconv.ParseInt(sizes[0], 10, 64)
	if err != nil || size < 1 || size > platform.SandboxMaxBytes || strconv.FormatInt(size, 10) != sizes[0] || (r.ContentLength >= 0 && r.ContentLength != size) {
		replyError(w, 400)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, platform.SandboxMaxBytes)
	source := &verifiedUpload{reader: r.Body, digest: sha256.New()}
	_, err = g.catalog.SealSandboxOutput(op.ctx, token, platform.SandboxBlob{SHA256: shas[0], Size: size}, source)
	if err != nil {
		replyError(w, catalogStatus(err))
		return
	}
	// A sealed Catalog replay returns the immutable acknowledgement without
	// reading the new body. Finish validating this HTTP envelope through EOF
	// before replying, so large retries never depend on net/http's small
	// post-handler drain limit or an early response racing the client's PUT.
	_, err = io.Copy(io.Discard, source)
	if err != nil || source.size != size || hex.EncodeToString(source.digest.Sum(nil)) != shas[0] {
		replyError(w, http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type verifiedUpload struct {
	reader io.Reader
	digest hash.Hash
	size   int64
}

func (v *verifiedUpload) Read(p []byte) (int, error) {
	n, err := v.reader.Read(p)
	v.size += int64(n)
	_, _ = v.digest.Write(p[:n])
	return n, err
}

func jsonContent(r *http.Request) bool {
	values := r.Header.Values("Content-Type")
	if len(values) != 1 {
		return false
	}
	media, params, err := mime.ParseMediaType(values[0])
	return err == nil && media == "application/json" && (len(params) == 0 || len(params) == 1 && params["charset"] == "utf-8")
}
