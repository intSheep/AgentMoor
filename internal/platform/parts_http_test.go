package platform

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentmoor/internal/asset"
)

func TestTarPartsHTTP(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	secret := bytes.Repeat([]byte{3}, 32)
	api := (&API{Catalog: c, Secret: secret, Temp: t.TempDir()}).Handler()
	issue := func(s asset.Scope, read, write bool) string {
		token, err := Issue(secret, Grant{Scope: s, Read: read, Write: write, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	s := scope("parts-http")
	token, other, readOnly := issue(s, true, true), issue(scope("another-tenant"), true, true), issue(s, true, false)
	req := func(method, path, auth string, body []byte, encoding string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+auth)
		if encoding != "" {
			r.Header.Set("Content-Encoding", encoding)
		}
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		return w
	}
	ch, manifest, raw, bodies := tarFixture(t, 0, tarFile{"body", "trusted scoped body"})
	changeJSON, _ := json.Marshal(ch)
	w := req("POST", "/v1/changes", token, changeJSON, "")
	var ticket asset.Ticket
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ticket) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	path := "/v1/changes/" + ticket.ID
	manifestJSON, _ := json.Marshal(manifest)
	compressed := func(body []byte) []byte {
		var b bytes.Buffer
		g := gzip.NewWriter(&b)
		g.Write(body)
		g.Close()
		return b.Bytes()
	}
	for _, tc := range []struct {
		auth, encoding string
		body           []byte
		code           int
	}{
		{readOnly, "", manifestJSON, 403},
		{other, "", manifestJSON, 404},
		{token, "deflate", manifestJSON, 400},
		{token, "gzip", []byte("invalid gzip"), 400},
		{token, "gzip", compressed(bytes.Repeat([]byte(" "), asset.MaxTarPartsManifestBytes+1)), 400},
		{token, "", append(manifestJSON, []byte("{}")...), 400},
		{token, "", []byte(strings.Replace(string(manifestJSON), "{", "{\"unknown\":true,", 1)), 400},
		{token, "gzip", compressed(manifestJSON), 200},
		{token, "", manifestJSON, 200},
	} {
		w := req("POST", path+"/parts", tc.auth, tc.body, tc.encoding)
		if w.Code != tc.code {
			t.Fatalf("manifest auth/encoding: got %d want %d: %s", w.Code, tc.code, w.Body.String())
		}
	}
	w = req("POST", path+"/parts", token, manifestJSON, "")
	var missing struct {
		Missing []asset.TarPart `json:"missing"`
	}
	if json.Unmarshal(w.Body.Bytes(), &missing) != nil || len(missing.Missing) != 1 || len(missing.Missing[0].Inline) != 0 {
		t.Fatal(w.Body.String())
	}
	p := missing.Missing[0]
	partPath := path + "/parts/" + p.SHA256
	body := bodies[p.SHA256]
	for _, tc := range []struct {
		path, auth string
		body       []byte
		code       int
	}{
		{partPath, readOnly, body, 403},
		{partPath, other, body, 404},
		{path + "/parts/" + strings.Repeat("0", 64), token, body, 404},
		{partPath, token, []byte("short"), 400},
		{partPath, token, bytes.Repeat([]byte("x"), len(body)), 400},
		{partPath, token, body, 204},
		{partPath, token, body, 204},
	} {
		w := req("PUT", tc.path, tc.auth, tc.body, "")
		if w.Code != tc.code {
			t.Fatal(tc.path, w.Code, tc.code, w.Body.String())
		}
	}
	if w = req("POST", path+"/commit", token, nil, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = req("POST", path+"/parts", token, manifestJSON, "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"missing":[]}` {
		t.Fatal("committed replay must not request uploads", w.Code, w.Body.String())
	}
	w = req("GET", ch.Key.ContentPath(1), token, nil, "")
	header, err := base64.RawURLEncoding.DecodeString(w.Header().Get("X-Agentmoor-Revision"))
	var revision asset.Revision
	if w.Code != 200 || err != nil || json.Unmarshal(header, &revision) != nil || revision.SHA256 != ch.SHA256 || revision.Format != ch.Format || !bytes.Equal(w.Body.Bytes(), raw) {
		t.Fatal("HTTP exposed a representation instead of original tar", w.Code, revision, err)
	}
	for _, key := range []asset.Key{{Kind: "skill", ID: "catalog/tool"}, {Kind: "session", ID: "root"}} {
		invalid := ch
		invalid.Key = key
		j, _ := json.Marshal(invalid)
		w := req("POST", "/v1/changes", token, j, "")
		var operation asset.Ticket
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &operation) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		w = req("POST", fmt.Sprintf("/v1/changes/%s/parts", operation.ID), token, manifestJSON, "")
		if w.Code != 400 {
			t.Fatal("non-workspace accepted tar-parts", key, w.Code, w.Body.String())
		}
	}
}
