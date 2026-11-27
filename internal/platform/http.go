package platform

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/httptrace"
)

type API struct {
	Catalog          *Catalog
	Secret           []byte
	AuthPolicy       string
	TaskAdapters     map[string]bool
	FileTaskAdapters map[string]bool
	TurnEnabled      func(asset.Scope) bool
	Temp             string
	SandboxProfiles  func(asset.Scope) (SandboxExecutionProfile, bool)
	SandboxControl   SandboxController
	RequestLog       httptrace.Sink
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	handle := func(pattern string, perm string, f func(http.ResponseWriter, *http.Request, Grant) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			httptrace.Route(r.Context(), pattern)
			w.Header().Set("Cache-Control", "no-store")
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			var g Grant
			var err error
			if a.AuthPolicy != "" {
				g, err = AuthenticateAuthPolicy(a.AuthPolicy, token, time.Now())
			} else {
				g, err = authenticate(a.Secret, token, time.Now())
			}
			if ok && errors.Is(err, ErrAuthPolicyUnavailable) {
				http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
				return
			}
			if !ok || err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			r = r.WithContext(auditHTTPContext(r.Context(), token, perm))
			actor := auditActor(r.Context(), "internal")
			httptrace.Platform(r.Context(), g.Scope, actor.CredentialSHA256, actor.Role)
			if (perm == "read" && !g.Read) || (perm == "write" && !g.Write) || (perm == "gc" && !g.Collect) || (perm == "submit" && !g.Submit) || (perm == "execute" && !g.Execute) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if err = f(w, r, g); err != nil {
				status := 500
				message := "platform operation failed"
				switch {
				case errors.Is(err, ErrMissing):
					status = 404
					message = ErrMissing.Error()
				case errors.Is(err, ErrConflict):
					status = 409
					message = ErrConflict.Error()
				case errors.Is(err, ErrNotReady):
					status = http.StatusTooEarly
					message = ErrNotReady.Error()
				case errors.Is(err, errUnsupported), errors.Is(err, errNoTurnProfile):
					status = http.StatusUnprocessableEntity
					message = "operation is not configured for this allocation"
				case errors.Is(err, errBadRequest):
					status = 400
					message = "invalid request or checksum"
				}
				http.Error(w, message, status)
			}
		})
	}
	a.taskRoutes(handle)
	a.sandboxRoutes(handle)
	a.partRoutes(handle)
	handle("GET /v1/context", "", func(w http.ResponseWriter, r *http.Request, g Grant) error { return reply(w, g) })
	handle("GET /v1/assets", "read", func(w http.ResponseWriter, r *http.Request, g Grant) error {
		items, err := a.Catalog.List(r.Context(), g.Scope)
		if err != nil {
			return err
		}
		return reply(w, items)
	})
	handle("GET /v1/heads", "read", func(w http.ResponseWriter, r *http.Request, g Grant) error {
		items, err := a.Catalog.Heads(r.Context(), g.Scope)
		if err != nil {
			return err
		}
		return reply(w, items)
	})
	handle("GET /v1/status", "read", func(w http.ResponseWriter, r *http.Request, g Grant) error {
		items, err := a.Catalog.Jobs(r.Context(), g.Scope)
		if err != nil {
			return err
		}
		return reply(w, items)
	})
	handle("POST /v1/changes", "write", func(w http.ResponseWriter, r *http.Request, g Grant) error {
		var change asset.Change
		if !decodeInput(w, r, &change, 64<<10) || change.Validate() != nil {
			return errBadRequest
		}
		t, err := a.Catalog.Begin(r.Context(), g.Scope, change)
		if err != nil {
			return err
		}
		return reply(w, t)
	})
	handle("PUT /v1/changes/{op}/content", "write", func(w http.ResponseWriter, r *http.Request, g Grant) error {
		id := r.PathValue("op")
		op, err := getOp(r.Context(), a.Catalog.db, g.Scope.Prefix(), id)
		if err != nil {
			return err
		}
		if op.state != "pending" && op.state != "committed" {
			return ErrConflict
		}
		if r.ContentLength >= 0 && r.ContentLength != op.change.Size {
			return errBadRequest
		}
		tmp, err := os.CreateTemp(a.Temp, ".upload-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(tmp, h), http.MaxBytesReader(w, r.Body, op.change.Size))
		if err != nil || n != op.change.Size || hex.EncodeToString(h.Sum(nil)) != op.change.SHA256 {
			return errBadRequest
		}
		if err = a.Catalog.Upload(r.Context(), g.Scope, id, tmp.Name(), op.change.SHA256, n); err != nil {
			return err
		}
		w.WriteHeader(204)
		return nil
	})
	handle("POST /v1/changes/{op}/commit", "write", func(w http.ResponseWriter, r *http.Request, g Grant) error {
		v, err := a.Catalog.Commit(r.Context(), g.Scope, r.PathValue("op"))
		if err != nil {
			return err
		}
		return reply(w, asset.Ticket{ID: r.PathValue("op"), Version: v})
	})
	handle("GET /v1/content", "read", func(w http.ResponseWriter, r *http.Request, g Grant) error {
		q := r.URL.Query()
		k := asset.Key{Kind: q.Get("kind"), ID: q.Get("id")}
		version, err := strconv.ParseInt(q.Get("version"), 10, 64)
		if !k.Valid() || err != nil || version < 0 {
			return errBadRequest
		}
		rev, stream, err := a.Catalog.Read(r.Context(), g.Scope, k, version)
		if err != nil {
			return err
		}
		defer stream.Close()
		data, err := json.Marshal(rev)
		if err != nil {
			return err
		}
		w.Header().Set("X-Agentmoor-Revision", base64.RawURLEncoding.EncodeToString(data))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(rev.Size, 10))
		_, err = io.Copy(w, stream)
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		return nil
	})
	handle("POST /v1/gc", "gc", func(w http.ResponseWriter, r *http.Request, g Grant) error {
		n, err := a.Catalog.Collect(r.Context(), g.Scope, time.Now().Add(-24*time.Hour))
		if err != nil {
			return err
		}
		return reply(w, map[string]int{"removed": n})
	})
	return httptrace.Handler("platform-api", mux, a.RequestLog)
}

var errNoTurnProfile = errors.New("follow-up execution is not configured for this allocation")
var errUnsupported = errors.New("adapter does not implement text-turn.v1")

var errBadRequest = errors.New("invalid request")

func reply(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}

func decodeInput(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	return dec.Decode(dst) == nil && dec.Decode(&struct{}{}) == io.EOF
}
