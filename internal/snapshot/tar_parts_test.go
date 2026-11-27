package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/intSheep/AgentMoor/internal/asset"
)

func capturePartsTree(t *testing.T, dir string) Blob {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	b, err := Tree(t.Context(), root, []Entry{{Source: ".", Name: "."}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func reassembleParts(t *testing.T, b Blob, m asset.TarParts, ranges map[string]PartRange) []byte {
	t.Helper()
	f, err := os.Open(b.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var joined bytes.Buffer
	for _, p := range m.Parts {
		if len(p.Inline) > 0 {
			joined.Write(p.Inline)
			continue
		}
		r, found := ranges[p.SHA256]
		if !found || r.Size != p.Size || r.Offset < 0 || r.Offset > b.Size-r.Size {
			t.Fatal("missing or invalid frozen body range", p, r)
		}
		raw := make([]byte, int(r.Size))
		if _, err := f.ReadAt(raw, r.Offset); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(raw)
		if hex.EncodeToString(h[:]) != p.SHA256 {
			t.Fatal("body range differs from its content address")
		}
		joined.Write(raw)
	}
	return joined.Bytes()
}

func TestPartitionTarPreservesFrozenBytesAndRestoresRichTree(t *testing.T) {
	dir := t.TempDir()
	longName := strings.Repeat("long-", 32) + ".txt"
	large := bytes.Repeat([]byte{1, 3, 5, 7}, int(asset.MaxTarPartBytes/4)+19)
	files := map[string][]byte{"empty": {}, "odd-511": bytes.Repeat([]byte{'a'}, 511), "even-512": bytes.Repeat([]byte{'b'}, 512), "odd-513": bytes.Repeat([]byte{'c'}, 513), "large": large, longName: []byte("same body"), "duplicate": []byte("same body")}
	for name, raw := range files {
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0751); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "empty-dir"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(longName, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	b := capturePartsTree(t, dir)
	want, err := os.ReadFile(b.Path)
	if err != nil {
		t.Fatal(err)
	}
	// The live workspace is no longer the source of the transfer.
	if err := os.WriteFile(filepath.Join(dir, "large"), []byte("later workspace content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, longName)); err != nil {
		t.Fatal(err)
	}
	m, ranges, err := PartitionTar(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	joined := reassembleParts(t, b, m, ranges)
	if !bytes.Equal(joined, want) {
		t.Fatal("partitioning changed the original tar framing or payload")
	}
	var paxPath, paxLink bool
	reader := tar.NewReader(bytes.NewReader(joined))
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		paxPath = paxPath || h.PAXRecords["path"] == longName
		paxLink = paxLink || h.PAXRecords["linkpath"] == longName
	}
	if !paxPath || !paxLink {
		t.Fatal("fixture did not exercise PAX path and link headers")
	}
	dst := filepath.Join(t.TempDir(), "restored")
	if err := Restore(t.Context(), bytes.NewReader(joined), asset.Metadata{SHA256: b.SHA256, Size: b.Size}, dst, true); err != nil {
		t.Fatal(err)
	}
	for name, expected := range files {
		actual, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatal("restored original content differs", name, err)
		}
	}
	if info, err := os.Stat(filepath.Join(dst, "empty-dir")); err != nil || !info.IsDir() || info.Mode().Perm() != 0750 {
		t.Fatal("directory metadata changed", info, err)
	}
	if info, err := os.Stat(filepath.Join(dst, "large")); err != nil || info.Mode().Perm() != 0751 {
		t.Fatal("file permissions changed", info, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != longName {
		t.Fatal("symlink changed", target, err)
	}
	duplicateHash := sha256.Sum256([]byte("same body"))
	if _, found := ranges[hex.EncodeToString(duplicateHash[:])]; !found {
		t.Fatal("duplicate bodies were not content addressed")
	}
	count := 0
	fullBodyChunk, finalBodyChunk := false, false
	for _, p := range m.Parts {
		if p.Size > asset.MaxTarPartBytes {
			t.Fatal("oversized file body part")
		}
		if p.SHA256 != "" {
			fullBodyChunk = fullBodyChunk || p.Size == asset.MaxTarPartBytes
			finalBodyChunk = finalBodyChunk || p.Size == int64(len(large))-asset.MaxTarPartBytes
		}
		if p.SHA256 == hex.EncodeToString(duplicateHash[:]) {
			count++
		}
	}
	if count != 2 {
		t.Fatal("duplicate file body references lost", count)
	}
	if !fullBodyChunk || !finalBodyChunk {
		t.Fatal("large file body did not split at the object size limit")
	}
}

func TestPartitionTarReusesBodiesAcrossRenameModeAndDifferentSizeChanges(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, raw []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0640); err != nil {
			t.Fatal(err)
		}
	}
	write("old-name", []byte("stable bytes"))
	write("neighbor", bytes.Repeat([]byte{'x'}, 513))
	capture := func() (asset.TarParts, map[string]int64) {
		t.Helper()
		b := capturePartsTree(t, dir)
		m, ranges, err := PartitionTar(t.Context(), b)
		if err != nil {
			t.Fatal(err)
		}
		if raw, err := os.ReadFile(b.Path); err != nil || !bytes.Equal(raw, reassembleParts(t, b, m, ranges)) {
			t.Fatal("tar roundtrip changed bytes", err)
		}
		objects := make(map[string]int64)
		for hash, r := range ranges {
			objects[hash] = r.Size
		}
		return m, objects
	}
	first, oldObjects := capture()
	if err := os.Rename(filepath.Join(dir, "old-name"), filepath.Join(dir, "renamed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "renamed"), 0751); err != nil {
		t.Fatal(err)
	}
	second, renamedObjects := capture()
	if !reflect.DeepEqual(oldObjects, renamedObjects) || reflect.DeepEqual(first, second) {
		t.Fatal("rename/mode change invalidated unchanged bodies or lost metadata")
	}
	write("neighbor", bytes.Repeat([]byte{'y'}, 91))
	_, changedObjects := capture()
	stableHash := sha256.Sum256([]byte("stable bytes"))
	stable := hex.EncodeToString(stableHash[:])
	if changedObjects[stable] != int64(len("stable bytes")) || len(changedObjects) != 2 {
		t.Fatal("different-size neighbor invalidated stable body")
	}
	if err := os.Remove(filepath.Join(dir, "neighbor")); err != nil {
		t.Fatal(err)
	}
	_, deletedObjects := capture()
	if len(deletedObjects) != 1 || deletedObjects[stable] != int64(len("stable bytes")) {
		t.Fatal("deletion lost or retained the wrong body")
	}
}

func TestPartitionTarRejectsInvalidFrozenInput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("frozen"), 0600); err != nil {
		t.Fatal(err)
	}
	b := capturePartsTree(t, dir)
	for _, test := range []struct {
		name string
		edit func(*Blob)
	}{
		{"wrong hash", func(b *Blob) { b.SHA256 = strings.Repeat("0", 64) }},
		{"bad hash", func(b *Blob) { b.SHA256 = "bad" }},
		{"wrong size", func(b *Blob) { b.Size-- }},
		{"negative size", func(b *Blob) { b.Size = -1 }},
		{"oversized", func(b *Blob) { b.Size = asset.MaxBytes + 1 }},
		{"directory", func(b *Blob) { b.Path = t.TempDir() }},
		{"symlink", func(b *Blob) {
			link := filepath.Join(t.TempDir(), "redirect")
			if err := os.Symlink(b.Path, link); err != nil {
				t.Fatal(err)
			}
			b.Path = link
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := b
			test.edit(&bad)
			if m, ranges, err := PartitionTar(t.Context(), bad); err == nil || len(m.Parts) != 0 || ranges != nil {
				t.Fatal("invalid frozen input accepted", err)
			}
		})
	}
	ctx, stop := context.WithCancel(t.Context())
	stop()
	if _, _, err := PartitionTar(ctx, b); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	for _, raw := range [][]byte{[]byte("not tar"), make([]byte, asset.MaxTarPartsInlineBytes+1)} {
		path := filepath.Join(t.TempDir(), "asset-input")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(raw)
		if _, _, err := PartitionTar(t.Context(), Blob{Path: path, SHA256: hex.EncodeToString(h[:]), Size: int64(len(raw))}); err == nil {
			t.Fatal("invalid tar or unbounded inline trailer accepted")
		}
	}
}
