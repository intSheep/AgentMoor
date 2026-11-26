package platform

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"

	"agentmoor/internal/asset"
)

func (a *API) partRoutes(handle func(string, string, func(http.ResponseWriter, *http.Request, Grant) error)) {
	handle("POST /v1/changes/{op}/parts", "write", func(w http.ResponseWriter, r *http.Request, g Grant) error {
		// Bound both wire bytes and decompressed JSON, including gzip bombs.
		r.Body = http.MaxBytesReader(w, r.Body, asset.MaxTarPartsManifestBytes)
		if encoding := r.Header.Get("Content-Encoding"); encoding != "" {
			if encoding != "gzip" {
				return errBadRequest
			}
			decoded, err := gzip.NewReader(r.Body)
			if err != nil {
				return errBadRequest
			}
			defer decoded.Close()
			r.Body = decoded
		}
		var parts asset.TarParts
		if !decodeInput(w, r, &parts, asset.MaxTarPartsManifestBytes) {
			return errBadRequest
		}
		missing, err := a.Catalog.PrepareParts(r.Context(), g.Scope, r.PathValue("op"), parts)
		if err != nil {
			return err
		}
		return reply(w, struct {
			Missing []asset.TarPart `json:"missing"`
		}{Missing: missing})
	})
	handle("PUT /v1/changes/{op}/parts/{sha}", "write", func(w http.ResponseWriter, r *http.Request, g Grant) error {
		id, digest := r.PathValue("op"), r.PathValue("sha")
		op, err := getOp(r.Context(), a.Catalog.db, g.Scope.Prefix(), id)
		if err != nil {
			return err
		}
		if op.state != "pending" && op.state != "committed" {
			return ErrConflict
		}
		size, err := a.Catalog.declaredPart(r.Context(), a.Catalog.db, g.Scope, id, digest)
		if err != nil {
			return err
		}
		if r.ContentLength >= 0 && r.ContentLength != size {
			return errBadRequest
		}
		tmp, err := os.CreateTemp(a.Temp, ".upload-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(tmp, h), http.MaxBytesReader(w, r.Body, size))
		if err != nil || n != size || hex.EncodeToString(h.Sum(nil)) != digest {
			return errBadRequest
		}
		if err = a.Catalog.UploadPart(r.Context(), g.Scope, id, tmp.Name(), digest, n); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
}
