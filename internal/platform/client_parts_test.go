package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTarPartsUploadFailureCancelsOtherActiveRequests(t *testing.T) {
	catalog, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	secret := bytes.Repeat([]byte{3}, 32)
	api := (&API{Catalog: catalog, Secret: secret, Temp: t.TempDir()}).Handler()
	allStarted := make(chan struct{})
	cancelled := make(chan struct{}, 7)
	var active atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || !strings.Contains(r.URL.Path, "/parts/") {
			api.ServeHTTP(w, r)
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		position := active.Add(1)
		if position == 8 {
			close(allStarted)
		}
		select {
		case <-allStarted:
		case <-r.Context().Done():
			return
		}
		if position == 1 {
			http.Error(w, "owned upload failure", http.StatusServiceUnavailable)
			return
		}
		<-r.Context().Done()
		cancelled <- struct{}{}
	}))
	defer server.Close()
	scope := scope("upload-cancellation")
	token, err := Issue(secret, Grant{Scope: scope, Read: true, Write: true, Expires: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]tarFile, 10)
	for i := range files {
		files[i] = tarFile{name: fmt.Sprintf("file-%d", i), body: fmt.Sprintf("independent body %d", i)}
	}
	change, _, raw, _ := tarFixture(t, 0, files...)
	file := filepath.Join(t.TempDir(), "frozen.tar")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	_, err = client.PublishTarParts(ctx, change, file)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || active.Load() != 8 {
		t.Fatalf("upload did not return the first failure with a live caller: active=%d err=%v ctx=%v", active.Load(), err, ctx.Err())
	}
	for i := 0; i < 7; i++ {
		select {
		case <-cancelled:
		case <-ctx.Done():
			t.Fatal("parallel upload request remained active after another upload failed")
		}
	}
	if heads, err := catalog.Heads(t.Context(), scope); err != nil || len(heads) != 0 {
		t.Fatal("partially uploaded workspace became visible", heads, err)
	}
}
