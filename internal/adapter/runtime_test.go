package adapter

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A launcher may exit when signalled but leave a child writing into the workspace.
func TestCancelCLIStopsDescendants(t *testing.T) {
	work := t.TempDir()
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- RunCLI(ctx, "/bin/sh", work, nil, io.Discard, 128, "-c", `(sleep 1; touch escaped) & echo ready > started; wait`)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(work, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("launcher did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	if err := <-done; err == nil {
		t.Fatal("cancelled launcher reported success")
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(filepath.Join(work, "escaped")); !os.IsNotExist(err) {
		t.Fatal("descendant survived cancellation", err)
	}
}

func TestPrivateHomeExcludesAmbientRuntimeState(t *testing.T) {
	t.Setenv("CODEX_HOME", "/other/allocation")
	t.Setenv("XDG_DATA_HOME", "/existing/data")
	t.Setenv("AGENTMOOR_TOKEN", "private")
	home := t.TempDir()
	var got bytes.Buffer
	if err := RunCLI(t.Context(), "/bin/sh", home, []string{"HOME=" + home}, &got, 1024, "-c", `printf '%s|%s|%s|%s' "$HOME" "$CODEX_HOME" "$XDG_DATA_HOME" "$AGENTMOOR_TOKEN"`); err != nil {
		t.Fatal(err)
	}
	if got.String() != home+"|||" {
		t.Fatalf("private environment leaked ambient state: %q", got.String())
	}
}
