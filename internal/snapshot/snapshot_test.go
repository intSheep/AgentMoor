package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/intSheep/AgentMoor/internal/asset"
)

func TestTreeRoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.Mkdir(filepath.Join(src, "empty"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "run"), []byte("hello"), 0751); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err = root.Symlink("../run", "empty/alias"); err != nil {
		t.Fatal(err)
	}
	capture := func() Blob {
		b, err := Tree(t.Context(), root, []Entry{{".", "."}}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b := capture()
	if b.SHA256 != capture().SHA256 {
		t.Fatal("nondeterministic archive")
	}
	m := asset.Metadata{Size: b.Size, SHA256: b.SHA256}
	dst := filepath.Join(t.TempDir(), "restored")
	restore := func() error {
		r, err := os.Open(b.Path)
		if err != nil {
			return err
		}
		defer r.Close()
		return Restore(t.Context(), r, m, dst, true)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if err := restore(); err == nil {
		t.Fatal("overwrote target")
	}
	data, err := os.ReadFile(filepath.Join(dst, "run"))
	if err != nil || string(data) != "hello" {
		t.Fatal(err, string(data))
	}
	a, _ := os.Stat(filepath.Join(src, "run"))
	v, _ := os.Stat(filepath.Join(dst, "run"))
	if a.Mode() != v.Mode() {
		t.Fatal(a.Mode(), v.Mode())
	}
	if _, err = os.Stat(filepath.Join(dst, "empty")); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "empty", "alias")); err != nil || target != "../run" {
		t.Fatal(target, err)
	}
}

func TestRejectsEscapesAndCorruption(t *testing.T) {
	for _, h := range []*tar.Header{{Name: "../escape", Typeflag: tar.TypeReg}, {Name: "/absolute", Typeflag: tar.TypeReg}, {Name: "alias", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}, {Name: "dev", Typeflag: tar.TypeChar}} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		hash := sha256.Sum256(b.Bytes())
		m := asset.Metadata{Size: int64(b.Len()), SHA256: hex.EncodeToString(hash[:])}
		dir := t.TempDir()
		dst := filepath.Join(dir, "restored")
		if err := Restore(context.Background(), &b, m, dst, true); err == nil {
			t.Fatal("accepted", h)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatal("partial target", entries, err)
		}
	}
	if err := Restore(t.Context(), bytes.NewReader([]byte("wrong")), asset.Metadata{Size: 5, SHA256: "bad"}, filepath.Join(t.TempDir(), "x"), false); err == nil {
		t.Fatal("bad checksum accepted")
	}
}
