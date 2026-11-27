// Package faultproxy is a test-only, bounded publication fault injector.
package faultproxy

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
)

const maxCases, maxEvents = 32, 4096

var turnWorkspace = regexp.MustCompile(`^turns/[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}/root$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var casePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$`)

type Arm struct {
	CaseID    string    `json:"case_id"`
	Stage     string    `json:"stage"`
	Timing    string    `json:"timing"`
	Limit     int       `json:"limit"`
	Target    asset.Key `json:"target"`
	HoldFirst bool      `json:"hold_first"`
}

// Event contains identities and outcomes only. Bodies, headers, credentials
// and URL queries are deliberately absent from all exported evidence.
type Event struct {
	Stage          string    `json:"stage"`
	Operation      string    `json:"operation"`
	Path           string    `json:"path"`
	At             time.Time `json:"at"`
	CompletedAt    time.Time `json:"completed_at,omitempty"`
	UpstreamStatus int       `json:"upstream_status,omitempty"`
	ManifestDigest string    `json:"manifest_digest,omitempty"`
	WireSHA256     string    `json:"wire_sha256,omitempty"`
	BodySHA256     string    `json:"body_sha256,omitempty"`
	Size           int64     `json:"size,omitempty"`
	MissingCount   int       `json:"missing_count,omitempty"`
	FirstMissing   string    `json:"first_missing,omitempty"`
	Injected       bool      `json:"injected"`
	Cancelled      bool      `json:"cancelled"`
	Error          string    `json:"error,omitempty"`
}

type CaseRecord struct {
	Arm
	Operation      string        `json:"operation,omitempty"`
	Change         *asset.Change `json:"change,omitempty"`
	ManifestDigest string        `json:"manifest_digest,omitempty"`
	WireSHA256     string        `json:"wire_sha256,omitempty"`
	PartTarget     string        `json:"part_target,omitempty"`
	Hits           int           `json:"hits"`
	Blocked        bool          `json:"blocked"`
	Consistent     bool          `json:"consistent"`
	Error          string        `json:"error,omitempty"`
	ArmedAt        time.Time     `json:"armed_at"`
	Events         []Event       `json:"events"`
}

type faultCase struct {
	CaseRecord
	parts    map[string]int64
	gate     chan struct{}
	heldOnce bool
}

type Proxy struct {
	mu        sync.Mutex
	upstream  *url.URL
	transport *http.Transport
	forward   *httputil.ReverseProxy
	active    *faultCase
	cases     []*faultCase
}

// HTTP1TLSConfig constrains both proxy TLS links to HTTP/1.1. The caller must
// still configure a trusted certificate when serving the downstream listener.
func HTTP1TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
}

func New(upstream string, roots *x509.CertPool) (*Proxy, error) {
	u, err := url.Parse(upstream)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || roots == nil {
		return nil, errors.New("upstream must be an HTTPS origin with an explicit CA pool")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = HTTP1TLSConfig()
	transport.TLSClientConfig.RootCAs = roots
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	transport.ForceAttemptHTTP2 = false
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = 30 * time.Second
	p := &Proxy{upstream: u, transport: transport}
	p.forward = httputil.NewSingleHostReverseProxy(u)
	p.forward.Transport = transport
	p.forward.ErrorLog = log.New(io.Discard, "", 0)
	p.forward.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "fault proxy upstream transport failed", http.StatusBadGateway)
	}
	return p, nil
}

func (p *Proxy) Close() { p.transport.CloseIdleConnections() }

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("request or response exceeds its bound or was interrupted")
	}
	return raw, nil
}

func decode(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("expected exactly one JSON value")
	}
	return nil
}

func hash(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (p *Proxy) Admin() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /arm", func(w http.ResponseWriter, r *http.Request) {
		raw, err := readBounded(r.Body, 16<<10)
		var arm Arm
		if err != nil || decode(raw, &arm) != nil || !casePattern.MatchString(arm.CaseID) || !arm.Target.Valid() || arm.Target.Kind != "workspace" || !turnWorkspace.MatchString(arm.Target.ID) || strings.Contains(arm.Target.ID, "00000000-0000-0000-0000-000000000000") || arm.Limit < 1 || arm.Limit > 3 || (arm.Timing != "before" && arm.Timing != "after") || (arm.Stage != "begin" && arm.Stage != "manifest" && arm.Stage != "part" && arm.Stage != "commit") {
			http.Error(w, "invalid arm specification", 400)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if len(p.cases) >= maxCases {
			http.Error(w, "case ledger is full", 429)
			return
		}
		if p.active != nil && p.active.Blocked {
			http.Error(w, "release the blocked case before arming another", 409)
			return
		}
		for _, prior := range p.cases {
			if prior.CaseID == arm.CaseID || prior.Target == arm.Target {
				http.Error(w, "case IDs and target turn keys cannot be reused", 409)
				return
			}
		}
		c := &faultCase{CaseRecord: CaseRecord{Arm: arm, Consistent: true, ArmedAt: time.Now().UTC(), Events: []Event{}}}
		p.cases = append(p.cases, c)
		p.active = c
		writeJSON(w, []byte(`{"armed":true}`))
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		var value any
		if p.active != nil {
			value = p.active.CaseRecord
		}
		raw, _ := json.Marshal(value)
		p.mu.Unlock()
		writeJSON(w, raw)
	})
	mux.HandleFunc("GET /ledger", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		values := make([]CaseRecord, len(p.cases))
		for i, c := range p.cases {
			values[i] = c.CaseRecord
		}
		raw, _ := json.Marshal(values)
		p.mu.Unlock()
		writeJSON(w, raw)
	})
	mux.HandleFunc("POST /release", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		if p.active != nil {
			release(p.active)
		}
		p.mu.Unlock()
		writeJSON(w, []byte(`{"released":true}`))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			http.Error(w, "admin requires a loopback connection", 403)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, raw []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func release(c *faultCase) {
	if c.gate != nil {
		close(c.gate)
		c.gate = nil
	}
	c.Blocked = false
}

func (p *Proxy) event(c *faultCase, i int, edit func(*Event)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	edit(&c.Events[i])
}

func (p *Proxy) reject(w http.ResponseWriter, c *faultCase, message string) {
	p.mu.Lock()
	c.Consistent, c.Error = false, message
	p.mu.Unlock()
	http.Error(w, "fault proxy rejected publication drift or invalid framing", 409)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	c := p.active
	p.mu.Unlock()
	if c == nil {
		p.forward.ServeHTTP(w, r)
		return
	}
	stage := ""
	var ch asset.Change
	var raw []byte
	if r.Method == "POST" && r.URL.Path == "/v1/changes" {
		var err error
		raw, err = readBounded(r.Body, asset.MaxTarPartsManifestBytes)
		if err != nil {
			http.Error(w, "begin metadata bound exceeded", 413)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if decode(raw, &ch) == nil && ch.Key == c.Target {
			if ch.Validate() != nil || ch.Format != "workspace-tar.v1" || ch.MediaType != "application/x-tar" {
				p.reject(w, c, "invalid_change")
				return
			}
			stage = "begin"
		}
	} else {
		p.mu.Lock()
		op := c.Operation
		p.mu.Unlock()
		prefix := "/v1/changes/" + op
		if op != "" {
			switch {
			case r.Method == "POST" && r.URL.Path == prefix+"/parts":
				stage = "manifest"
			case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, prefix+"/parts/"):
				stage = "part"
			case r.Method == "POST" && r.URL.Path == prefix+"/commit":
				stage = "commit"
			}
		}
	}
	if stage == "" {
		p.forward.ServeHTTP(w, r)
		return
	}
	e := Event{Stage: stage, Path: r.URL.Path, At: time.Now().UTC()}
	var manifest asset.TarParts
	switch stage {
	case "begin":
		e.Operation = asset.Fingerprint(ch)
	case "manifest", "part":
		limit := int64(asset.MaxTarPartsManifestBytes)
		if stage == "part" {
			limit = asset.MaxTarPartBytes
		}
		var err error
		raw, err = readBounded(r.Body, limit)
		if err != nil {
			http.Error(w, "publication request bound exceeded", 413)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		e.Size = int64(len(raw))
		if stage == "manifest" {
			e.WireSHA256 = hash(raw)
			decoded := raw
			if encoding := r.Header.Get("Content-Encoding"); encoding == "gzip" {
				zr, err := gzip.NewReader(bytes.NewReader(raw))
				if err != nil {
					p.reject(w, c, "invalid_gzip")
					return
				}
				decoded, err = readBounded(zr, asset.MaxTarPartsManifestBytes)
				closeErr := zr.Close()
				if err != nil || closeErr != nil {
					p.reject(w, c, "invalid_gzip_bound")
					return
				}
			} else if encoding != "" {
				p.reject(w, c, "invalid_content_encoding")
				return
			}
			p.mu.Lock()
			change := *c.Change
			p.mu.Unlock()
			if decode(decoded, &manifest) != nil || manifest.Validate(change) != nil {
				p.reject(w, c, "invalid_manifest")
				return
			}
			e.ManifestDigest, _ = manifest.Digest()
		} else {
			e.BodySHA256 = hash(raw)
			if !digestPattern.MatchString(strings.TrimPrefix(r.URL.Path, "/v1/changes/"+c.Operation+"/parts/")) || !strings.HasSuffix(r.URL.Path, "/"+e.BodySHA256) {
				p.reject(w, c, "part_digest_differs")
				return
			}
		}
	}
	p.mu.Lock()
	if len(c.Events) >= maxEvents {
		c.Error = "event_ledger_full"
		p.mu.Unlock()
		http.Error(w, "case event ledger is full", 429)
		return
	}
	consistent := true
	if stage == "begin" {
		consistent = c.Operation == "" || c.Operation == e.Operation
		if c.Operation == "" {
			c.Operation, c.Change = e.Operation, &ch
		}
	} else {
		e.Operation = c.Operation
	}
	if stage == "manifest" {
		consistent = c.ManifestDigest == "" || (c.ManifestDigest == e.ManifestDigest && c.WireSHA256 == e.WireSHA256)
		if c.ManifestDigest == "" {
			c.ManifestDigest, c.WireSHA256 = e.ManifestDigest, e.WireSHA256
			c.parts = make(map[string]int64)
			for _, part := range manifest.Parts {
				if len(part.Inline) == 0 {
					c.parts[part.SHA256] = part.Size
				}
			}
		}
	}
	if stage == "part" {
		size, found := c.parts[e.BodySHA256]
		consistent = found && size == e.Size
	}
	if !consistent {
		c.Consistent, c.Error = false, "publication_identity_drift"
		p.mu.Unlock()
		http.Error(w, "publication identity drift", 409)
		return
	}
	i := len(c.Events)
	c.Events = append(c.Events, e)
	p.mu.Unlock()
	if c.Timing == "before" && p.inject(w, r, c, i, stage, e.BodySHA256) {
		return
	}
	response, err := p.roundTrip(r)
	if err != nil {
		p.event(c, i, func(e *Event) { e.Error = "upstream_transport"; e.CompletedAt = time.Now().UTC() })
		http.Error(w, "fault proxy upstream transport failed", 502)
		return
	}
	// Targeted publication responses are metadata only. Ordinary asset GETs
	// never take this path and remain streamed by ReverseProxy.
	body, err := readBounded(response.Body, asset.MaxTarPartsManifestBytes)
	closeErr := response.Body.Close()
	p.event(c, i, func(e *Event) { e.UpstreamStatus = response.StatusCode })
	if err != nil || closeErr != nil {
		p.event(c, i, func(e *Event) { e.Error = "upstream_response_incomplete"; e.CompletedAt = time.Now().UTC() })
		http.Error(w, "fault proxy upstream response incomplete", 502)
		return
	}
	if stage == "manifest" && response.StatusCode/100 == 2 {
		if !p.recordMissing(c, i, body) {
			p.reject(w, c, "invalid_missing_response")
			return
		}
	}
	if c.Timing == "after" && response.StatusCode/100 == 2 && p.inject(w, r, c, i, stage, e.BodySHA256) {
		return
	}
	for _, token := range strings.Split(response.Header.Get("Connection"), ",") {
		response.Header.Del(strings.TrimSpace(token))
	}
	for key, values := range response.Header {
		if !hopHeader(key) {
			w.Header()[key] = values
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
	p.event(c, i, func(e *Event) { e.CompletedAt = time.Now().UTC() })
}

func (p *Proxy) recordMissing(c *faultCase, index int, raw []byte) bool {
	var response struct {
		Missing []asset.TarPart `json:"missing"`
	}
	if decode(raw, &response) != nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool)
	for _, part := range response.Missing {
		size, found := c.parts[part.SHA256]
		if !found || size != part.Size || len(part.Inline) != 0 || seen[part.SHA256] {
			return false
		}
		seen[part.SHA256] = true
	}
	e := &c.Events[index]
	e.MissingCount = len(response.Missing)
	if len(response.Missing) > 0 {
		e.FirstMissing = response.Missing[0].SHA256
		if c.PartTarget == "" {
			c.PartTarget = e.FirstMissing
		}
	}
	return true
}

func (p *Proxy) inject(w http.ResponseWriter, r *http.Request, c *faultCase, i int, stage, bodySHA string) bool {
	p.mu.Lock()
	if stage != c.Stage || c.Hits >= c.Limit || (stage == "part" && (c.PartTarget == "" || bodySHA != c.PartTarget)) {
		p.mu.Unlock()
		return false
	}
	c.Hits++
	c.Events[i].Injected = true
	var gate chan struct{}
	if c.HoldFirst && !c.heldOnce {
		c.heldOnce, c.Blocked = true, true
		c.gate = make(chan struct{})
		gate = c.gate
	}
	p.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			p.mu.Lock()
			c.Events[i].Cancelled = true
			c.Events[i].CompletedAt = time.Now().UTC()
			release(c)
			p.mu.Unlock()
			return true
		}
	}
	if r.Context().Err() != nil {
		p.event(c, i, func(e *Event) { e.Cancelled = true; e.CompletedAt = time.Now().UTC() })
		return true
	}
	p.event(c, i, func(e *Event) { e.CompletedAt = time.Now().UTC() })
	if c.Timing == "before" {
		http.Error(w, "fixture publication fault before upstream", http.StatusServiceUnavailable)
		return true
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		p.event(c, i, func(e *Event) { e.Error = "downstream_not_http1" })
		http.Error(w, "fault proxy requires HTTP/1.1", 500)
		return true
	}
	conn, _, err := hijacker.Hijack()
	if err == nil {
		_ = conn.Close()
	} else {
		p.event(c, i, func(e *Event) { e.Error = "downstream_hijack_failed" })
	}
	return true
}

func hopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

func (p *Proxy) roundTrip(r *http.Request) (*http.Response, error) {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme, out.URL.Host = p.upstream.Scheme, p.upstream.Host
	out.Host = p.upstream.Host
	for _, token := range strings.Split(out.Header.Get("Connection"), ",") {
		out.Header.Del(strings.TrimSpace(token))
	}
	for key := range out.Header {
		if hopHeader(key) {
			out.Header.Del(key)
		}
	}
	return p.transport.RoundTrip(out)
}
