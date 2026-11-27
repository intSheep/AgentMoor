package sandboxgateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"agentmoor/internal/platform"
)

func gatewayAuditPage(t *testing.T, f *fixture) platform.AuditPage {
	t.Helper()
	reader, err := platform.OpenAuditReader(t.Context(), f.directory, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	page, err := reader.Page(t.Context(), f.scope, 0, platform.AuditMaxLimit)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(page.Events)) != page.Total {
		t.Fatalf("small fixture audit was truncated: got %d of %d events", len(page.Events), page.Total)
	}
	return page
}

func gatewayAuditAction(t *testing.T, page platform.AuditPage, action string, count int) []platform.AuditEvent {
	t.Helper()
	var events []platform.AuditEvent
	for _, event := range page.Events {
		if event.Action == action {
			events = append(events, event)
		}
	}
	if len(events) != count {
		t.Fatalf("audit action %s: got %d events, want %d", action, len(events), count)
	}
	return events
}

func gatewayAuditActor(t *testing.T, event platform.AuditEvent, source, role, component string) {
	t.Helper()
	if event.Actor.Source != source || event.Actor.Role != role || event.Actor.CredentialSHA256 != "" || event.Component != component {
		t.Errorf("%s attribution = %+v component=%q, want %s/%s without credential digest and component %s", event.Action, event.Actor, event.Component, source, role, component)
	}
}

func gatewayAuditNoSensitiveFields(t *testing.T, f *fixture, page platform.AuditPage, canaries ...string) {
	t.Helper()
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	canaries = append(canaries, f.permit.Capabilities.Input, f.permit.Capabilities.Output, f.permit.Capabilities.Model, syntheticProviderKey)
	for _, canary := range canaries {
		if bytes.Contains(raw, []byte(canary)) {
			t.Error("audit retained sensitive request data or a capability")
		}
	}
}

func TestGatewayAuditDelegatedModelAndOutputActors(t *testing.T) {
	f := setup(t, nil, 0)
	initial := gatewayAuditPage(t, f)
	for _, action := range []string{"sandbox.begun", "sandbox.input_sealed", "sandbox.create_fenced", "sandbox.pod_bound"} {
		for _, event := range gatewayAuditAction(t, initial, action, 1) {
			gatewayAuditActor(t, event, "controller", "system", "sandbox-controller")
		}
	}

	// Exercise both initial allowance creation and an increment before output
	// sealing revokes model authority. Guest metadata cannot supply an actor.
	const prompt = "synthetic-sensitive-model-prompt-audit-canary"
	const suppliedActor = "synthetic-guest-supplied-actor-canary"
	var request map[string]any
	if err := json.Unmarshal(modelBody(), &request); err != nil {
		t.Fatal(err)
	}
	request["input"] = prompt
	request["client_metadata"] = map[string]any{"actor": suppliedActor, "role": "system"}
	model, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		response, err := f.request(http.MethodPost, "/v1/responses", f.permit.Capabilities.Model, model)
		closeResponse(t, response, err, http.StatusOK)
	}
	modelEvents := gatewayAuditAction(t, gatewayAuditPage(t, f), "model.reserved", 2)
	for i, event := range modelEvents {
		gatewayAuditActor(t, event, "guest-capability", "model", "model-gateway")
		if event.ModelRequests != int64(i+1) || event.ModelInputBytes <= 0 || event.ModelOutputBytes <= 0 || event.RunID != f.permit.Run.ID {
			t.Error("model reservation lost its counters or delegated run identity")
		}
	}
	if f.requests.Load() != 2 {
		t.Fatalf("upstream received %d requests, want 2", f.requests.Load())
	}
	f.seenMu.Lock()
	for _, seen := range f.seen {
		if seen["client_metadata"] != nil {
			t.Error("guest actor metadata reached upstream")
		}
	}
	f.seenMu.Unlock()

	// Output is opaque native data. Even JSON with actor-shaped members must
	// remain data, never become the principal of the two output transitions.
	output := []byte(`{"actor":{"source":"synthetic-output-actor-canary","role":"system","credential_sha256":"synthetic-output-credential-canary"},"native":"synthetic-original-output-canary"}`)
	response, err := f.request(http.MethodPut, "/output", f.permit.Capabilities.Output, output)
	closeResponse(t, response, err, http.StatusNoContent)
	sealed := gatewayAuditPage(t, f)
	for _, action := range []string{"sandbox.output_declared", "sandbox.output_sealed"} {
		for _, event := range gatewayAuditAction(t, sealed, action, 1) {
			gatewayAuditActor(t, event, "guest-capability", "output", "sandbox-controller")
			if event.RunID != f.permit.Run.ID || event.Digest != sha(output) {
				t.Error("output audit lost its delegated run or frozen digest")
			}
		}
	}
	if sealed.Total != initial.Total+4 {
		t.Errorf("two model reservations and output declaration/seal added %d events, want 4", sealed.Total-initial.Total)
	}

	// A lost acknowledgement replays the same bytes without adding evidence.
	response, err = f.request(http.MethodPut, "/output", f.permit.Capabilities.Output, output)
	closeResponse(t, response, err, http.StatusNoContent)
	if replay := gatewayAuditPage(t, f); replay.Total != sealed.Total {
		t.Error("identical output acknowledgement replay duplicated audit evidence")
	}

	// A later actual controller operation has its own principal; delegation
	// must not leak out of one gateway request's context into the Catalog.
	run, err := f.catalog.SandboxRun(t.Context(), f.permit.Run.Address())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.catalog.RecordSandboxTerminal(t.Context(), run.Address(), run.PodUID, 0); err != nil {
		t.Fatal(err)
	}
	// This unit fixture explicitly supplies the trusted controller observation;
	// terminal classification alone is no longer guest stop evidence.
	code := 0
	if _, err = f.catalog.RecordSandboxGuestStopped(t.Context(), run.Address(), platform.SandboxGuestStopProof{
		Kind: platform.SandboxGuestTerminated, PodUID: run.PodUID, ResourceVersion: "synthetic-controller-stop-observation",
		NodeName: "synthetic-controller-node", ContainerID: "containerd://" + strings.Repeat("a", 64),
		ContainerName: "guest", RestartCount: 0, ExitCode: &code,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.catalog.ConfirmSandboxDeleted(t.Context(), run.Address(), run.PodUID); err != nil {
		t.Fatal(err)
	}
	final := gatewayAuditPage(t, f)
	for _, action := range []string{"sandbox.terminated", "sandbox.deleted"} {
		for _, event := range gatewayAuditAction(t, final, action, 1) {
			gatewayAuditActor(t, event, "controller", "system", "sandbox-controller")
		}
	}
	gatewayAuditNoSensitiveFields(t, f, final, prompt, suppliedActor, "synthetic-output-actor-canary", "synthetic-output-credential-canary", "synthetic-original-output-canary")
}

func TestGatewayAuditDeniedCapabilitiesCannotCreateEvidence(t *testing.T) {
	f := setup(t, nil, 0)
	initial := gatewayAuditPage(t, f)
	spoof := []byte(`{"actor":{"source":"http","role":"execute","credential_sha256":"synthetic-spoofed-credential"},"input":"synthetic-spoofed-input"}`)
	bad := "sbx1." + strings.Repeat("A", 43)
	cases := []struct {
		name, method, path, token string
		body                      []byte
		status                    int
	}{
		{"input cannot reserve model", http.MethodPost, "/v1/responses", f.permit.Capabilities.Input, modelBody(), http.StatusUnauthorized},
		{"output cannot reserve model", http.MethodPost, "/v1/responses", f.permit.Capabilities.Output, modelBody(), http.StatusUnauthorized},
		{"input cannot declare output", http.MethodPut, "/output", f.permit.Capabilities.Input, spoof, http.StatusUnauthorized},
		{"model cannot declare output", http.MethodPut, "/output", f.permit.Capabilities.Model, spoof, http.StatusUnauthorized},
		{"unknown cannot reserve model", http.MethodPost, "/v1/responses", bad, modelBody(), http.StatusUnauthorized},
		{"unknown body cannot choose actor", http.MethodPut, "/output", bad, spoof, http.StatusUnauthorized},
		{"authorized model body cannot choose actor", http.MethodPost, "/v1/responses", f.permit.Capabilities.Model, []byte(`{"model":"gpt-5","stream":true,"input":"synthetic-spoofed-input","actor":{"source":"http","role":"execute"}}`), http.StatusBadRequest},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response, err := f.request(test.method, test.path, test.token, test.body)
			body := closeResponse(t, response, err, test.status)
			if bytes.Contains(body, []byte(test.token)) || bytes.Contains(body, []byte("synthetic-spoofed")) {
				t.Error("denied response retained credential or caller-supplied actor")
			}
			if current := gatewayAuditPage(t, f); current.Total != initial.Total {
				t.Errorf("denied request added %d audit events", current.Total-initial.Total)
			}
			if f.requests.Load() != 0 {
				t.Error("denied request reached upstream")
			}
		})
	}
	gatewayAuditNoSensitiveFields(t, f, gatewayAuditPage(t, f), "synthetic-spoofed-credential", "synthetic-spoofed-input")
}
