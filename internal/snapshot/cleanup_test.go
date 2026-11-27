package snapshot

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intSheep/AgentMoor/internal/asset"
)

func TestRemoveDirectoryRestoresAccessWithoutFollowingSymlinks(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("trusted external bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0700) })
	workspace := filepath.Join(t.TempDir(), "owned-workspace")
	if err := os.MkdirAll(filepath.Join(workspace, "locked"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "external-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(workspace, "locked"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace, 0); err != nil {
		t.Fatal(err)
	}
	if err := RemoveDirectory(workspace); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outside)
	if err != nil || info.Mode().Perm() != 0500 {
		t.Fatal("owned cleanup changed an external directory", err)
	}
	if raw, err := os.ReadFile(filepath.Join(outside, "sentinel")); err != nil || string(raw) != "trusted external bytes" {
		t.Fatal("owned cleanup altered external bytes", err)
	}
	if _, err := os.Lstat(workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned workspace was not removed", err)
	}
	if err := RemoveDirectory(workspace); err != nil {
		t.Fatal("cleanup of an absent owned path must be idempotent", err)
	}
	rootLink := filepath.Join(t.TempDir(), "unexpected-root-link")
	if err := os.Symlink(outside, rootLink); err != nil {
		t.Fatal(err)
	}
	if err := RemoveDirectory(rootLink); err == nil {
		t.Fatal("cleanup accepted a symlink as its owned root")
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0500 {
		t.Fatal("root symlink rejection changed the external target", err)
	}
}

type collisionDuringRestore struct {
	io.Reader
	once func()
}

func (r *collisionDuringRestore) Read(p []byte) (int, error) {
	if r.once != nil {
		r.once()
		r.once = nil
	}
	return r.Reader.Read(p)
}

func TestRestoreFailureCleansUnreadableStageAndPreservesCollidingTarget(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, entry := range []struct {
		name string
		kind byte
	}{{".", tar.TypeDir}, {"locked", tar.TypeDir}, {"locked/private", tar.TypeReg}} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Typeflag: entry.kind, Mode: 0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	dest := filepath.Join(parent, "restored")
	// Keep the negative control safe when run against the old leaking code.
	t.Cleanup(func() {
		entries, _ := os.ReadDir(parent)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".agentmoor-restore-") {
				_ = RemoveDirectory(filepath.Join(parent, entry.Name()))
			}
		}
	})
	reader := &collisionDuringRestore{Reader: bytes.NewReader(raw.Bytes()), once: func() {
		if err := os.Mkdir(dest, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, "sentinel"), []byte("previous target"), 0600); err != nil {
			t.Fatal(err)
		}
	}}
	digest := sha256.Sum256(raw.Bytes())
	meta := asset.Metadata{Size: int64(raw.Len()), SHA256: hex.EncodeToString(digest[:])}
	if err := Restore(t.Context(), reader, meta, dest, true); err == nil {
		t.Fatal("restore overwrote a concurrently created target")
	}
	if sentinel, err := os.ReadFile(filepath.Join(dest, "sentinel")); err != nil || string(sentinel) != "previous target" {
		t.Fatal("failed restore changed the colliding target", err)
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 1 || entries[0].Name() != "restored" {
		t.Fatal("failed restore retained an unreadable staging directory", entries, err)
	}
}
