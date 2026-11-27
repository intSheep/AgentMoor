package nativeguest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/snapshot"
)

func treeTestHeader(t *testing.T, name string, kind byte, size int64) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := tar.NewWriter(&out)
	if err := writer.WriteHeader(&tar.Header{Name: strings.TrimSuffix(name, "/"), Mode: 0600, Typeflag: tar.TypeReg, Size: 0, Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0)}); err != nil {
		t.Fatal(err)
	}
	raw := bytes.Clone(out.Bytes()[:512])
	clear(raw[:100])
	copy(raw[:100], name)
	raw[156] = kind
	copy(raw[124:136], []byte(fmt.Sprintf("%011o\x00", size)))
	treeTestChecksum(raw)
	return raw
}
func treeTestChecksum(raw []byte) {
	for i := 148; i < 156; i++ {
		raw[i] = ' '
	}
	sum := 0
	for _, b := range raw {
		sum += int(b)
	}
	copy(raw[148:156], []byte(fmtOctal(sum)))
}
func treeTestArchive(t *testing.T, records ...struct {
	name string
	kind byte
	size int64
	data []byte
}) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, r := range records {
		out.Write(treeTestHeader(t, r.name, r.kind, r.size))
		out.Write(r.data)
		out.Write(make([]byte, (512-len(r.data)%512)%512))
	}
	out.Write(make([]byte, 1024))
	return out.Bytes()
}
func treeTestPAX(key, value string) string {
	n := len(key) + len(value) + 3
	for {
		record := fmt.Sprintf("%d %s=%s\n", n, key, value)
		if len(record) == n {
			return record
		}
		n = len(record)
	}
}
func treeTestValidate(t *testing.T, raw []byte, limit int64) error {
	t.Helper()
	file := filepath.Join(t.TempDir(), "source.tar")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return ValidateTree(t.Context(), file, limit)
}

type treeRecord = struct {
	name string
	kind byte
	size int64
	data []byte
}

func TestNativeGuestTreePreservesPAXFilesAndLinks(t *testing.T) {
	longName := "nested/" + strings.Repeat("long-native-file-", 10) + ".jsonl"
	data := []byte("original native transcript\n")
	var out bytes.Buffer
	writer := tar.NewWriter(&out)
	for _, header := range []*tar.Header{
		{Name: "nested", Typeflag: tar.TypeDir, Mode: 0750, Format: tar.FormatPAX, ModTime: time.Unix(0, 0)},
		{Name: longName, Typeflag: tar.TypeReg, Mode: 0640, Size: int64(len(data)), Format: tar.FormatPAX, ModTime: time.Unix(0, 0)},
		{Name: "native-link", Typeflag: tar.TypeSymlink, Linkname: longName, Mode: 0777, Format: tar.FormatPAX, ModTime: time.Unix(0, 0)},
	} {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write(data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	raw := out.Bytes()
	if !bytes.Contains(raw, []byte(" path=")) || !bytes.Contains(raw, []byte(" linkpath=")) {
		t.Fatal("positive fixture needs actual PAX path/link records")
	}
	source := filepath.Join(t.TempDir(), "source.tar")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTree(t.Context(), source, MaxBundleBytes); err != nil {
		t.Fatal("ordinary native PAX rejected", err)
	}
	after, _ := os.ReadFile(source)
	if !bytes.Equal(after, raw) {
		t.Fatal("validation rewrote original native bytes")
	}
	sum := sha256.Sum256(raw)
	dest := filepath.Join(t.TempDir(), "restored")
	if err := snapshot.Restore(t.Context(), bytes.NewReader(raw), asset.Metadata{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(raw))}, dest, true); err != nil {
		t.Fatal("accepted original PAX failed safe restore", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, longName))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("native file changed", err)
	}
	link, err := os.Readlink(filepath.Join(dest, "native-link"))
	if err != nil || link != longName {
		t.Fatal("native relative link changed", link, err)
	}
	if err := treeTestValidate(t, append(bytes.Clone(raw), make([]byte, 1024)...), MaxBundleBytes); err != nil {
		t.Fatal("normal native zero record padding rejected", err)
	}
}

func TestNativeGuestTreeRejectsDangerousExtensionsAndBodies(t *testing.T) {
	body := []byte("x")
	good := treeTestArchive(t, treeRecord{"file", tar.TypeReg, 1, body})
	if err := treeTestValidate(t, good, MaxBundleBytes); err != nil {
		t.Fatal("positive raw fixture", err)
	}
	cases := map[string][]byte{
		"hardlink":          treeTestArchive(t, treeRecord{"file", tar.TypeLink, 0, nil}),
		"gnu-sparse":        treeTestArchive(t, treeRecord{"file", tar.TypeGNUSparse, 0, nil}),
		"gnu-longname":      treeTestArchive(t, treeRecord{"file", tar.TypeGNULongName, 0, nil}),
		"global-pax":        treeTestArchive(t, treeRecord{"file", tar.TypeXGlobalHeader, 0, nil}),
		"reg-a-directory":   treeTestArchive(t, treeRecord{"smuggled/", tar.TypeRegA, 1, body}),
		"directory-body":    treeTestArchive(t, treeRecord{"directory", tar.TypeDir, 1, body}),
		"link-body":         treeTestArchive(t, treeRecord{"link", tar.TypeSymlink, 1, body}),
		"oversized-logical": treeTestArchive(t, treeRecord{"file", tar.TypeReg, MaxBundleBytes + 1, body}),
		"missing-body":      treeTestArchive(t, treeRecord{"file", tar.TypeReg, 4096, body}),
		"duplicate":         treeTestArchive(t, treeRecord{"file", tar.TypeReg, 1, body}, treeRecord{"file", tar.TypeReg, 1, body}),
		"path-escape":       treeTestArchive(t, treeRecord{"../escape", tar.TypeReg, 1, body}),
		"concatenated":      append(bytes.Clone(good), good...),
		"trailing-data":     append(bytes.Clone(good), []byte("nonzero")...),
		"truncated":         good[:len(good)-1],
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if err := treeTestValidate(t, raw, MaxBundleBytes); err == nil {
				t.Fatal("unsafe native archive accepted")
			}
		})
	}
}
func TestNativeGuestTreeRejectsSparseAndMalformedPAX(t *testing.T) {
	validPath := treeTestPAX("path", "native/session.json")
	good := treeTestArchive(t, treeRecord{"PaxHeaders/file", tar.TypeXHeader, int64(len(validPath)), []byte(validPath)}, treeRecord{"file", tar.TypeReg, 1, []byte("x")})
	if err := treeTestValidate(t, good, MaxBundleBytes); err != nil {
		t.Fatal("bounded ordinary PAX positive control", err)
	}
	for name, pax := range map[string]string{
		"gnu-sparse-map":   treeTestPAX("GNU.sparse.map", "0,1073741824"),
		"gnu-sparse-major": treeTestPAX("GNU.sparse.major", "1"),
		"sparse-alias":     treeTestPAX("SCHILY.sparse", "1"),
		"size-expansion":   treeTestPAX("size", fmt.Sprint(MaxBundleBytes+1)),
		"negative-size":    treeTestPAX("size", "-1"),
		"malformed-length": "999 path=bad\n",
		"malformed-member": "12 noequals\n",
		"oversized-pax":    strings.Repeat(" ", maxGuestPAXBytes+1),
		"too-many-records": strings.Repeat(treeTestPAX("mtime", "0"), 129),
	} {
		t.Run(name, func(t *testing.T) {
			raw := treeTestArchive(t, treeRecord{"PaxHeaders/file", tar.TypeXHeader, int64(len(pax)), []byte(pax)}, treeRecord{"file", tar.TypeReg, 1, []byte("x")})
			if err := treeTestValidate(t, raw, MaxBundleBytes); err == nil {
				t.Fatal("dangerous PAX accepted")
			}
		})
	}
	for name, pax := range map[string]string{"path-only": validPath, "size-override": treeTestPAX("size", "1")} {
		t.Run("dangling-"+name, func(t *testing.T) {
			raw := treeTestArchive(t, treeRecord{"PaxHeaders/file", tar.TypeXHeader, int64(len(pax)), []byte(pax)})
			if err := treeTestValidate(t, raw, MaxBundleBytes); err == nil {
				t.Fatal("dangling extension was silently ignored")
			}
		})
	}
}
func TestNativeGuestTreeRejectsEscapingLinksAndMetadata(t *testing.T) {
	for name, edit := range map[string]func([]byte){
		"absolute-link":    func(h []byte) { h[156] = tar.TypeSymlink; copy(h[157:257], []byte("/etc/passwd")) },
		"relative-escape":  func(h []byte) { h[156] = tar.TypeSymlink; copy(h[157:257], []byte("../outside")) },
		"windows-link":     func(h []byte) { h[156] = tar.TypeSymlink; copy(h[157:257], []byte("C:\\outside")) },
		"setuid-mode":      func(h []byte) { copy(h[100:108], []byte("0004755\x00")) },
		"gnu-base256-size": func(h []byte) { h[124] = 0x80 },
		"bad-checksum":     func(h []byte) { h[148] = '8' },
	} {
		t.Run(name, func(t *testing.T) {
			raw := treeTestHeader(t, "link", tar.TypeReg, 0)
			edit(raw)
			if name != "bad-checksum" {
				treeTestChecksum(raw)
			}
			raw = append(raw, make([]byte, 1024)...)
			if err := treeTestValidate(t, raw, MaxBundleBytes); err == nil {
				t.Fatal("unsafe native metadata accepted")
			}
		})
	}
}
func TestNativeGuestTreeEntryQuotaAndCancelledRead(t *testing.T) {
	var out bytes.Buffer
	writer := tar.NewWriter(&out)
	for i := 0; i <= MaxTreeEntries; i++ {
		if err := writer.WriteHeader(&tar.Header{Name: fmt.Sprintf("file-%d", i), Typeflag: tar.TypeReg, Mode: 0600, Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if int64(out.Len()) > MaxBundleBytes {
		t.Fatal("entry quota fixture must fit wire budget")
	}
	if err := treeTestValidate(t, out.Bytes(), MaxBundleBytes); err == nil {
		t.Fatal("excessive filesystem entry count accepted")
	}
	small := bundleTree(t, "file", []byte("x"))
	file := filepath.Join(t.TempDir(), "source.tar")
	os.WriteFile(file, small, 0600)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ValidateTree(ctx, file, MaxBundleBytes); err == nil {
		t.Fatal("cancelled tree read accepted")
	}
	if err := ValidateTree(t.Context(), file, 0); err == nil {
		t.Fatal("invalid expanded quota accepted")
	}
	if err := ValidateTree(t.Context(), file, int64(len(small)-1)); err == nil {
		t.Fatal("wire quota ignored")
	}
}
