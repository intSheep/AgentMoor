package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentmoor/internal/httptrace"
)

type telemetryErrorObjects struct {
	Objects
	err error
}

func (o telemetryErrorObjects) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, o.err
}

func TestAPITelemetryScopesOnlyAuthenticatedCredentialAndUsesFixedRoute(t *testing.T) {
	catalog, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	secret := bytes.Repeat([]byte{53}, 32)
	s := scope("telemetry")
	token, err := Issue(secret, Grant{Scope: s, Read: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var events []httptrace.Event
	handler := (&API{Catalog: catalog, Secret: secret, RequestLog: func(event httptrace.Event) { events = append(events, event) }}).Handler()
	for _, credential := range []string{"synthetic-invalid-token", token} {
		request := httptest.NewRequest("POST", "/v1/changes/synthetic-sensitive-op/commit?synthetic-secret-query=yes", strings.NewReader("synthetic-secret-body"))
		request.Header.Set("Authorization", "Bearer "+credential)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	if len(events) != 2 || events[0].Status != 401 || events[0].Scope != nil || events[0].CredentialSHA256 != "" || events[1].Status != 403 || events[1].Scope == nil || *events[1].Scope != s || events[1].RequestedRole != "write" || events[1].CredentialSHA256 != auditHash("credential", token) {
		t.Fatalf("incorrect denied credential attribution: %+v", events)
	}
	for _, event := range events {
		if event.Route != "POST /v1/changes/{op}/commit" || event.Outcome != "denied" {
			t.Fatalf("raw operation ID or wrong outcome in route: %+v", event)
		}
	}
	raw, _ := json.Marshal(events)
	for _, forbidden := range []string{token, "synthetic-invalid-token", "synthetic-sensitive-op", "synthetic-secret"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatal("request secret or raw URL leaked into telemetry")
		}
	}
	// An actual object-store error can contain a credential or private path.
	// Neither the public error nor the legacy default logger may retain it.
	ch := change("synthetic content", 0)
	ticket := upload(t, catalog, s, ch, "synthetic content")
	if _, err := catalog.Commit(t.Context(), s, ticket.ID); err != nil {
		t.Fatal(err)
	}
	var unsafeLog bytes.Buffer
	previousLog := log.Writer()
	log.SetOutput(&unsafeLog)
	defer log.SetOutput(previousLog)
	for _, objectError := range []error{errors.New("synthetic-secret-storage-error"), fmt.Errorf("synthetic-secret-wrapped-error: %w", ErrMissing)} {
		catalog.objects = telemetryErrorObjects{Objects: catalog.objects, err: objectError}
		request := httptest.NewRequest(http.MethodGet, ch.Key.ContentPath(1), nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		raw, _ := json.Marshal(events[len(events)-1])
		if bytes.Contains(response.Body.Bytes(), []byte("synthetic-secret")) || bytes.Contains(raw, []byte("synthetic-secret")) || strings.Contains(unsafeLog.String(), "synthetic-secret") {
			t.Fatal("object-store error leaked through response or diagnostics")
		}
	}
	// The real storage failure still yields a fixed error and structured class.
	_ = catalog.Close()
	request := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	last := events[len(events)-1]
	if response.Code != 500 || last.Outcome != "service_error" || response.Body.String() != "platform operation failed\n" {
		t.Fatalf("unsafe storage error result: %d %q %+v", response.Code, response.Body.String(), last)
	}
}
