// Package httptrace emits bounded operational request telemetry. It is not a
// transactional audit log and never accepts a request body, URL or error value.
package httptrace

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/intSheep/AgentMoor/internal/asset"
)

const Schema = "http-request.v1"

type Event struct {
	Schema           string       `json:"schema"`
	TimeUnixNano     int64        `json:"time_unix_nano"`
	RequestID        string       `json:"request_id"`
	Component        string       `json:"component"`
	Method           string       `json:"method"`
	Route            string       `json:"route"`
	Status           int          `json:"status"`
	Outcome          string       `json:"outcome"`
	DurationNanos    int64        `json:"duration_nanos"`
	ResponseBytes    int64        `json:"response_bytes"`
	Actor            string       `json:"actor"`
	RequestedRole    string       `json:"requested_role,omitempty"`
	CredentialSHA256 string       `json:"credential_sha256,omitempty"`
	Scope            *asset.Scope `json:"scope,omitempty"`
	RunID            string       `json:"run_id,omitempty"`
	TurnID           string       `json:"turn_id,omitempty"`
}

// Sink is trusted service configuration, never supplied over HTTP. A nil sink
// writes one JSON line to stderr. Failure to record never changes HTTP results.
type Sink func(Event)

var logger = log.New(os.Stderr, "", 0)
var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var routePattern = regexp.MustCompile(`^(GET|HEAD|POST|PUT|DELETE|PATCH|OPTIONS) /[A-Za-z0-9/{}/._-]*$`)

type contextKey struct{}
type trace struct{ event Event }

// Route accepts only the fixed template registered by trusted routing code.
// Callers must never pass a request path or URL here.
func Route(ctx context.Context, template string) {
	if t, ok := ctx.Value(contextKey{}).(*trace); ok && len(template) <= 160 && routePattern.MatchString(template) {
		t.event.Route = template
	}
}

// Platform is called only after credential authentication, including a valid
// credential subsequently denied the requested role. The digest is the same
// domain-separated credential identity used by the transaction audit.
func Platform(ctx context.Context, scope asset.Scope, credentialSHA256, requestedRole string) {
	t, ok := ctx.Value(contextKey{}).(*trace)
	if !ok || !scope.Valid() || !digest.MatchString(credentialSHA256) {
		return
	}
	switch requestedRole {
	case "context", "read", "write", "submit", "execute", "collect":
	default:
		return
	}
	t.event.Actor, t.event.Scope, t.event.CredentialSHA256, t.event.RequestedRole = "platform-credential", &scope, credentialSHA256, requestedRole
}

// Capability is called only after the exact delegated capability and purpose
// have been checked. No capability value or capability digest is accepted.
func Capability(ctx context.Context, scope asset.Scope, runID, turnID, purpose string) {
	t, ok := ctx.Value(contextKey{}).(*trace)
	if !ok || !scope.Valid() || !digest.MatchString(runID) || (turnID != "" && uuid.Validate(turnID) != nil) {
		return
	}
	switch purpose {
	case "input", "output", "model":
	default:
		return
	}
	t.event.Actor, t.event.Scope, t.event.RunID, t.event.TurnID, t.event.RequestedRole = "guest-capability", &scope, runID, turnID, purpose
}

func method(value string) string {
	switch value {
	case "GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS", "CONNECT", "TRACE":
		return value
	default:
		return "other"
	}
}

func outcome(status int) string {
	switch {
	case status == 401 || status == 403:
		return "denied"
	case status == 404 || status == 405:
		return "not_found"
	case status == 409:
		return "conflict"
	case status == 425:
		return "not_ready"
	case status == 429:
		return "limited"
	case status == 422:
		return "unsupported"
	case status >= 500:
		return "service_error"
	case status >= 400:
		return "bad_request"
	case status >= 300:
		return "redirect"
	default:
		return "ok"
	}
}

func emit(sink Sink, event Event) {
	// Observability must not replace a handler result or an existing panic.
	defer func() { _ = recover() }()
	if sink != nil {
		sink(event)
		return
	}
	data, err := json.Marshal(event)
	if err == nil {
		logger.Print(string(data))
	}
}

// Handler records each dispatch, including unmatched routes and aborted
// streams. The wrapper deliberately exposes Unwrap so ResponseController can
// reach the real connection's read/write deadlines and streaming flush.
func Handler(component string, next http.Handler, sink Sink) http.Handler {
	if component != "platform-api" && component != "sandbox-gateway" {
		panic("invalid request telemetry component")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		t := &trace{event: Event{Schema: Schema, TimeUnixNano: started.UTC().UnixNano(), RequestID: uuid.NewString(), Component: component, Method: method(r.Method), Route: "unmatched", Actor: "unauthenticated"}}
		w.Header().Set("X-Agentmoor-Request-ID", t.event.RequestID)
		observed := &responseWriter{ResponseWriter: w}
		defer func() {
			failure := recover()
			t.event.Status, t.event.ResponseBytes, t.event.DurationNanos = observed.status, observed.bytes, time.Since(started).Nanoseconds()
			if t.event.Status == 0 && failure == nil {
				t.event.Status = http.StatusOK
			}
			t.event.Outcome = outcome(t.event.Status)
			switch {
			case failure != nil || observed.failed:
				t.event.Outcome = "aborted"
			case r.Context().Err() == context.DeadlineExceeded:
				t.event.Outcome = "deadline"
			case r.Context().Err() == context.Canceled:
				t.event.Outcome = "cancelled"
			}
			emit(sink, t.event)
			if failure != nil {
				// net/http otherwise formats unexpected panic values into its
				// error log. Preserve the connection abort without logging any
				// arbitrary panic text, capability or body fragment.
				panic(http.ErrAbortHandler)
			}
		}()
		next.ServeHTTP(observed, r.WithContext(context.WithValue(r.Context(), contextKey{}, t)))
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	failed bool
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 && (status >= 200 || status == http.StatusSwitchingProtocols) {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	w.failed = w.failed || err != nil
	return n, err
}

func (w *responseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	w.failed = w.failed || err != nil
	return err
}
