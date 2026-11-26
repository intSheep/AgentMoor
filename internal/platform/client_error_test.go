package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestClientHTTPErrorPreservesStatusMessagesAndSentinelIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/status/"))
		if err != nil {
			t.Errorf("invalid test status path: %s", r.URL.Path)
			http.Error(w, "invalid fixture path", http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, "  synthetic failure \n")
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "synthetic-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{400, 401, 403, 404, 409, 422, 425, 429, 500, 501, 502, 503, 504} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var value any
			err := client.JSON(t.Context(), http.MethodGet, "/status/"+strconv.Itoa(status), nil, &value)
			if err == nil {
				t.Fatal("non-success HTTP response was accepted")
			}
			want := fmt.Sprintf("platform HTTP %d: synthetic failure", status)
			switch status {
			case 404:
				want = "asset or operation not found"
			case 409:
				want = "version conflict: synthetic failure"
			}
			if err.Error() != want {
				t.Fatalf("error text changed: got %q, want %q", err, want)
			}
			wrapped := fmt.Errorf("worker publication: %w", err)
			var responseError *HTTPError
			if !errors.As(wrapped, &responseError) || responseError.StatusCode != status || responseError.Message != "synthetic failure" {
				t.Fatalf("HTTP classification unavailable through wrapping: %#v, err %v", responseError, wrapped)
			}
			if errors.Is(wrapped, ErrMissing) != (status == 404) || errors.Is(wrapped, ErrConflict) != (status == 409) {
				t.Fatalf("sentinel identity changed for HTTP %d: %v", status, wrapped)
			}
		})
	}
}

func TestClientSuccessfulHTTPWithMalformedJSONRemainsSyntaxError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "not JSON\n")
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "synthetic-token")
	if err != nil {
		t.Fatal(err)
	}
	var value any
	err = client.JSON(t.Context(), http.MethodGet, "/value", nil, &value)
	var syntaxError *json.SyntaxError
	var responseError *HTTPError
	if !errors.As(err, &syntaxError) || errors.As(err, &responseError) || errors.Is(err, ErrMissing) || errors.Is(err, ErrConflict) {
		t.Fatalf("successful HTTP response's format error was reclassified: %T %v", err, err)
	}
	if syntaxError.Offset != 2 || err.Error() != "invalid character 'o' in literal null (expecting 'u')" {
		t.Fatalf("JSON syntax error changed: %#v, err %v", syntaxError, err)
	}
}
