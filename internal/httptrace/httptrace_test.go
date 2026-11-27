package httptrace

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTraceRealTLSControllerAndStreaming(t *testing.T) {
	events := make(chan Event, 1)
	release := make(chan struct{})
	server := httptest.NewTLSServer(Handler("sandbox-gateway", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Route(r.Context(), "POST /v1/responses")
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Error(err)
		}
		if err := controller.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		if err := controller.Flush(); err != nil {
			t.Error(err)
		}
		<-release
		_, _ = io.WriteString(w, "data: second\n\n")
	}), func(event Event) { events <- event }))
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first event did not flush before handler completion: %q, %v", line, err)
	}
	close(release)
	_, _ = io.ReadAll(reader)
	_ = response.Body.Close()
	event := <-events
	if event.Outcome != "ok" || event.Status != 200 || event.ResponseBytes != int64(len("data: first\n\ndata: second\n\n")) || event.DurationNanos <= 0 || uuid.Validate(event.RequestID) != nil || event.RequestID != response.Header.Get("X-Agentmoor-Request-ID") {
		t.Fatalf("bad stream telemetry: %+v", event)
	}
}

func TestTraceAbortSuppressesArbitraryPanicAndKeepsConnectionAbort(t *testing.T) {
	for _, value := range []any{http.ErrAbortHandler, "synthetic-panic-secret-canary", map[string]string{"secret": "synthetic-map-panic-secret"}} {
		t.Run("abort", func(t *testing.T) {
			events := make(chan Event, 1)
			var serverErrors bytes.Buffer
			server := httptest.NewUnstartedServer(Handler("platform-api", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "partial")
				_ = http.NewResponseController(w).Flush()
				panic(value)
			}), func(event Event) { events <- event }))
			server.Config.ErrorLog = log.New(&serverErrors, "", 0)
			server.StartTLS()
			defer server.Close()
			response, err := server.Client().Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err == nil || string(raw) != "partial" {
				t.Fatalf("abort became successful EOF or appended an error body: %q %v", raw, err)
			}
			event := <-events
			if event.Status != 200 || event.Outcome != "aborted" || event.ResponseBytes != 7 || serverErrors.Len() != 0 {
				t.Fatalf("abort leaked diagnostic or lost partial status: %+v log=%q", event, serverErrors.String())
			}
		})
	}
}

func TestTraceDoesNotLogRequestOrAcceptClientRequestID(t *testing.T) {
	var event Event
	handler := Handler("platform-api", http.NotFoundHandler(), func(value Event) { event = value })
	request := httptest.NewRequest("SYNTHETIC-SECRET-METHOD", "/synthetic-secret-path?token=synthetic-secret-query", strings.NewReader("synthetic-secret-body"))
	request.Header.Set("Authorization", "Bearer synthetic-secret-token")
	request.Header.Set("X-Agentmoor-Request-ID", "synthetic-secret-client-id")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	raw, _ := json.Marshal(event)
	if bytes.Contains(raw, []byte("synthetic-secret")) || event.Method != "other" || event.Route != "unmatched" || event.Scope != nil || event.Actor != "unauthenticated" || event.Outcome != "not_found" || uuid.Validate(event.RequestID) != nil {
		t.Fatalf("untrusted request influenced telemetry: %s", raw)
	}
	response = httptest.NewRecorder()
	Handler("platform-api", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), func(Event) { panic("unavailable telemetry sink") }).ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
	if response.Code != 204 {
		t.Fatal("telemetry sink changed successful response")
	}
}
