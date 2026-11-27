package platform

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
)

func TestHTTPAuthority(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	key := bytes.Repeat([]byte{7}, 32)
	api := (&API{Catalog: c, Secret: key, Temp: t.TempDir()}).Handler()
	grant := func(s asset.Scope, r, w bool, at time.Time) string {
		v, err := Issue(key, Grant{Scope: s, Read: r, Write: w, Expires: at.Add(time.Hour).Unix()}, at)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	req := func(method, path, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		return w
	}
	now := time.Now()
	a := grant(scope("a"), true, true, now)
	b := grant(scope("b"), true, true, now)
	ro := grant(scope("a"), true, false, now)
	wo := grant(scope("a"), false, true, now)
	raw, _ := json.Marshal(change("one", 0))
	w := req("POST", "/v1/changes", a, string(raw))
	var ticket asset.Ticket
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ticket) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	path := "/v1/changes/" + ticket.ID
	for _, tc := range []struct {
		method, path, token, body string
		code                      int
	}{
		{"GET", "/v1/assets", "", "", 401}, {"GET", "/v1/context", a + "x", "", 401}, {"GET", "/v1/assets", grant(scope("a"), true, false, now.Add(-2*time.Hour)), "", 401},
		{"POST", "/v1/changes", ro, string(raw), 403}, {"POST", "/v1/changes", a, strings.Replace(string(raw), "{", "{\"tenant\":\"b\",", 1), 400}, {"GET", "/v1/assets", wo, "", 403},
		{"PUT", path + "/content", b, "one", 404}, {"POST", path + "/commit", b, "", 404}, {"PUT", path + "/content", a, "bad", 400}, {"POST", path + "/commit", a, "", 425},
		{"PUT", path + "/content", a, "one", 204}, {"POST", path + "/commit", a, "", 200}, {"GET", "/v1/content?kind=artifact&id=out/report.txt&version=1", b, "", 404}, {"POST", "/v1/gc", a, "", 403},
	} {
		w := req(tc.method, tc.path, tc.token, tc.body)
		if w.Code != tc.code {
			t.Errorf("%s %s: got %d want %d (%s)", tc.method, tc.path, w.Code, tc.code, w.Body.String())
		}
	}
	if w := req("GET", "/v1/assets", b, ""); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatal(w.Body.String())
	}
	for _, dimension := range []string{"project", "runtime", "adapter"} {
		t.Run(dimension, func(t *testing.T) {
			other := scope("a")
			switch dimension {
			case "project":
				other.Project = "another-project"
			case "runtime":
				other.Runtime = "another-runtime"
			case "adapter":
				other.Adapter = "another-adapter"
			}
			token := grant(other, true, true, now)
			for _, p := range []string{"/v1/assets", "/v1/heads", "/v1/status"} {
				if w := req("GET", p, token, ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
					t.Fatalf("scope exposed %s: %d %s", p, w.Code, w.Body.String())
				}
			}
			if w := req("POST", path+"/commit", token, ""); w.Code != 404 {
				t.Fatalf("foreign operation visible: %d", w.Code)
			}
		})
	}

}
