package sandboxgateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"agentmoor/internal/httptrace"
)

func TestGatewayTelemetryVerifiedDelegationAndNoCapabilityFingerprint(t *testing.T) {
	f := setup(t, nil, 0)
	events := make(chan httptrace.Event, 3)
	f.gateway.requestLog = func(event httptrace.Event) { events <- event }
	for _, token := range []string{"synthetic-invalid-capability", f.permit.Capabilities.Model} {
		response, err := f.request(http.MethodPost, "/v1/responses", token, modelBody())
		want := http.StatusOK
		if token == "synthetic-invalid-capability" {
			want = http.StatusUnauthorized
		}
		closeResponse(t, response, err, want)
	}
	denied, admitted := <-events, <-events
	if denied.Scope != nil || denied.Actor != "unauthenticated" || denied.RunID != "" || denied.CredentialSHA256 != "" || denied.Outcome != "denied" {
		t.Fatalf("unverified capability acquired attribution: %+v", denied)
	}
	if admitted.Scope == nil || *admitted.Scope != f.scope || admitted.Actor != "guest-capability" || admitted.RunID != f.permit.Run.ID || admitted.RequestedRole != "model" || admitted.CredentialSHA256 != "" || admitted.Outcome != "ok" || admitted.Route != "POST /v1/responses" {
		t.Fatalf("verified capability lost delegation: %+v", admitted)
	}
	raw, _ := json.Marshal([]httptrace.Event{denied, admitted})
	for _, value := range []string{f.permit.Capabilities.Model, syntheticProviderKey, "synthetic-invalid-capability", string(modelBody())} {
		digest := sha256.Sum256([]byte(value))
		if bytes.Contains(raw, []byte(value)) || bytes.Contains(raw, []byte(hex.EncodeToString(digest[:]))) {
			t.Fatal("telemetry retained request content or capability fingerprint")
		}
	}
}
