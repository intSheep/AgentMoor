package platform

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/intSheep/AgentMoor/internal/nativeguest"
	"github.com/intSheep/AgentMoor/internal/task"
)

// SandboxController runs only in the trusted control plane. Guest requests are
// served by a different capability-only listener, never by API.Handler.
type SandboxController interface {
	Start(context.Context, SandboxAddress) (SandboxRun, error)
	Reconcile(context.Context, SandboxAddress) (SandboxRun, error)
	Cancel(context.Context, SandboxAddress) (SandboxRun, error)
}

type SandboxTaskStatus struct {
	Run     SandboxRun          `json:"run"`
	Request nativeguest.Request `json:"request"`
}

func sandboxAttempt(w http.ResponseWriter, r *http.Request) (string, bool) {
	// One exact field: no duplicate, case-folded alias, unknown field or tail.
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return "", false
	}
	if token, err := d.Token(); err != nil || token != "attempt" {
		return "", false
	}
	var attempt string
	if err := d.Decode(&attempt); err != nil || !sandboxUUID(attempt) {
		return "", false
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return "", false
	}
	_, err := d.Token()
	return attempt, err == io.EOF
}

func (a *API) sandboxRoutes(handle func(string, string, apiHandler)) {
	for _, base := range []string{"/v1/tasks/{id}/sandbox", "/v1/tasks/{id}/turns/{turn}/sandbox"} {
		for _, operation := range []string{"", "/status", "/output", "/cancel"} {
			handle("POST "+base+operation, "execute", func(w http.ResponseWriter, r *http.Request, grant Grant) error {
				if r.PathValue("id") != grant.Scope.Runtime {
					return ErrMissing
				}
				turn := r.PathValue("turn")
				if turn != "" && !sandboxUUID(turn) {
					return errBadRequest
				}
				attempt, ok := sandboxAttempt(w, r)
				if !ok {
					return errBadRequest
				}
				address := SandboxAddress{Scope: grant.Scope, TurnID: turn, Attempt: attempt}
				// Cleanup/status still need to work after cancellation or expiry;
				// only starting requires a live claim. The output reader separately
				// enforces ready-to-publish and the successful ACK-recovery case.
				slot, err := a.Catalog.readTask(r.Context(), taskAddress{address.Scope, address.TurnID})
				if err != nil {
					return err
				}
				if slot.attempt != attempt || slot.ID != address.Scope.Runtime || slot.TurnID != turn {
					return ErrConflict
				}
				if a.SandboxProfiles == nil || a.SandboxControl == nil {
					return errUnsupported
				}
				profile, ok := a.SandboxProfiles(grant.Scope)
				if !ok {
					return errUnsupported
				}
				if slot.ExecutionPolicy != task.ExecutionUntrustedIsolated {
					return ErrConflict
				}
				var run SandboxRun
				var request nativeguest.Request
				if operation == "" {
					run, request, err = a.Catalog.PrepareSandboxInput(r.Context(), address, profile)
					if err == nil {
						run, err = a.SandboxControl.Start(r.Context(), address)
					}
				} else {
					run, err = a.Catalog.SandboxRun(r.Context(), address)
					if err == nil && operation == "/output" && run.ProfileFingerprint != profile.Fingerprint {
						err = ErrConflict
					}
					if err == nil {
						request, _, err = a.Catalog.sandboxRequest(r.Context(), address.Scope, run.Source, profile)
					}
					if err == nil && operation == "/cancel" {
						run, err = a.SandboxControl.Cancel(r.Context(), address)
					} else if err == nil && operation == "/status" {
						run, err = a.SandboxControl.Reconcile(r.Context(), address)
					}
				}
				if err != nil {
					return err
				}
				if operation != "/output" {
					return reply(w, SandboxTaskStatus{Run: run, Request: request})
				}
				blob, reader, err := a.Catalog.ReadSandboxOutput(r.Context(), address)
				if err != nil {
					return err
				}
				defer reader.Close()
				w.Header().Set("Content-Type", "application/x-tar")
				w.Header().Set("Content-Length", strconv.FormatInt(blob.Size, 10))
				w.Header().Set("X-Agentmoor-SHA256", blob.SHA256)
				w.Header().Set("X-Agentmoor-Size", strconv.FormatInt(blob.Size, 10))
				_, err = io.Copy(w, reader)
				if err != nil {
					// Binary headers may already have been sent. Abort this stream;
					// never append the generic API error body to an archive.
					panic(http.ErrAbortHandler)
				}
				return nil
			})
		}
	}
}
