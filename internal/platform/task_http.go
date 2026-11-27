package platform

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
)

type apiHandler = func(http.ResponseWriter, *http.Request, Grant) error
type taskHandler func(http.ResponseWriter, *http.Request, taskAddress) (task.Record, error)

func (a *API) taskRoutes(handle func(string, string, apiHandler)) {
	bind := func(pattern, right string, handler taskHandler) {
		handle(pattern, right, func(w http.ResponseWriter, r *http.Request, g Grant) error {
			if r.PathValue("id") != g.Scope.Runtime {
				return ErrMissing
			}
			turn := r.PathValue("turn")
			if turn != "" {
				id, err := uuid.Parse(turn)
				if err != nil || id == uuid.Nil || id.String() != turn {
					return errBadRequest
				}
			}
			record, err := handler(w, r, taskAddress{Scope: g.Scope, turn: turn})
			if err != nil {
				return err
			}
			return reply(w, record)
		})
	}
	bind("PUT /v1/tasks/{id}", "submit", func(w http.ResponseWriter, r *http.Request, s taskAddress) (task.Record, error) {
		var spec task.Spec
		if !a.TaskAdapters[s.Adapter] && !a.FileTaskAdapters[s.Adapter] {
			return task.Record{}, errUnsupported
		}
		if !decodeInput(w, r, &spec, 128<<10) || !spec.Valid() {
			return task.Record{}, errBadRequest
		}
		if (spec.Mode == task.FileTurn && !a.FileTaskAdapters[s.Adapter]) || (spec.Mode != task.FileTurn && !a.TaskAdapters[s.Adapter]) {
			return task.Record{}, errUnsupported
		}
		return a.Catalog.submitTask(r.Context(), s, spec)
	})
	bind("PUT /v1/tasks/{id}/turns/{turn}", "submit", func(w http.ResponseWriter, r *http.Request, s taskAddress) (task.Record, error) {
		if a.TurnEnabled == nil || !a.TurnEnabled(s.Scope) {
			return task.Record{}, errNoTurnProfile
		}
		var q task.Question
		if !decodeInput(w, r, &q, 64<<10) || strings.TrimSpace(q.Message) == "" || len(q.Message) > 8192 || !asset.SafePath(q.SessionID) || len(q.SessionID) > 128 {
			return task.Record{}, errBadRequest
		}
		return a.Catalog.submitTurn(r.Context(), s, q)
	})
	handle("GET /v1/tasks/{id}/checkpoint", "read", func(w http.ResponseWriter, r *http.Request, g Grant) error {
		if r.PathValue("id") != g.Scope.Runtime {
			return ErrMissing
		}
		cp, err := a.Catalog.latestCheckpoint(r.Context(), g.Scope)
		if err != nil {
			return err
		}
		return reply(w, cp)
	})
	for _, route := range []string{"/v1/tasks/{id}", "/v1/tasks/{id}/turns/{turn}"} {
		bind("GET "+route, "read", func(_ http.ResponseWriter, r *http.Request, s taskAddress) (task.Record, error) {
			slot, err := a.Catalog.readTask(r.Context(), s)
			return slot.Record, err
		})
		bind("POST "+route+"/cancel", "submit", func(_ http.ResponseWriter, r *http.Request, s taskAddress) (task.Record, error) {
			return a.Catalog.cancelTask(r.Context(), s)
		})
		bind("POST "+route+"/claim", "execute", func(w http.ResponseWriter, r *http.Request, s taskAddress) (task.Record, error) {
			var claim task.Claim
			if !decodeInput(w, r, &claim, 1<<20) {
				return task.Record{}, errBadRequest
			}
			if a.SandboxProfiles != nil {
				// Configuring isolation is a server policy, including allocations
				// whose profile is missing. Absence must never enable local work.
				_, assigned := a.SandboxProfiles(s.Scope)
				if !sandboxUUID(claim.Attempt) {
					return task.Record{}, errBadRequest
				}
				if !assigned || a.SandboxControl == nil || claim.ExecutionPolicy != task.ExecutionUntrustedIsolated {
					return task.Record{}, errUnsupported
				}
			} else if claim.ExecutionPolicy != "" && claim.ExecutionPolicy != task.ExecutionTrustedLocal {
				return task.Record{}, errUnsupported
			}
			return a.Catalog.claimTask(r.Context(), s, claim)
		})
		bind("POST "+route+"/finish", "execute", func(w http.ResponseWriter, r *http.Request, s taskAddress) (task.Record, error) {
			var finish task.Finish
			if !decodeInput(w, r, &finish, 1<<20) {
				return task.Record{}, errBadRequest
			}
			return a.Catalog.finishTask(r.Context(), s, finish)
		})
	}
}
