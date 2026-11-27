package nativeguest

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func treeExpansionArchive(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for _, header := range headers {
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestNativeGuestTreeCountsImplicitParentDirectories(t *testing.T) {
	// Each empty file causes 63 distinct parent directories. These 1100 tiny
	// headers fit below 1 MiB, but restoration would create over 70,000 nodes.
	// Only validation is performed: no expanded directories are ever created.
	var headers []*tar.Header
	for i := range 1100 {
		name := fmt.Sprintf("tree-%04d/", i) + strings.Repeat("d/", 62) + "file"
		headers = append(headers, &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Format: tar.FormatUSTAR})
	}
	raw := treeExpansionArchive(t, headers)
	if len(raw) >= 1<<20 || len(headers) >= MaxTreeEntries || 1+len(headers)*64 <= MaxTreeEntries {
		t.Fatal("implicit-directory fixture no longer exercises small-wire expansion")
	}
	if err := treeTestValidate(t, raw, MaxBundleBytes); err == nil {
		t.Fatal("small-wire implicit-directory expansion was accepted")
	}
}

func TestNativeGuestTreeCountsSharedAndExplicitParentsOnce(t *testing.T) {
	parent := "shared/" + strings.Repeat("d/", 62)
	var headers []*tar.Header
	for i := range 1100 {
		headers = append(headers, &tar.Header{Name: parent + fmt.Sprintf("file-%04d", i), Typeflag: tar.TypeReg, Mode: 0600, Format: tar.FormatUSTAR})
	}
	// Explicit directory declarations may follow their children. They promote
	// already-counted implicit directories rather than consuming another node.
	headers = append(headers, &tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: 0700, Format: tar.FormatUSTAR})
	name := "shared"
	for range 63 {
		headers = append(headers, &tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0700, Format: tar.FormatUSTAR})
		name += "/d"
	}
	raw := treeExpansionArchive(t, headers)
	file := filepath.Join(t.TempDir(), "shared.tar")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTree(t.Context(), file, MaxBundleBytes); err != nil {
		t.Fatal("shared or later explicit parents were counted more than once", err)
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatal("tree validation rewrote original archive bytes", err)
	}
}

func TestNativeGuestTreeBoundsPAXPathsAndSymlinkTargetsInBytes(t *testing.T) {
	boundary := strings.Repeat("a/", (MaxTreePathBytes-2)/2) + "ab"
	if len(boundary) != MaxTreePathBytes {
		t.Fatal("path boundary fixture changed")
	}
	positive := treeExpansionArchive(t, []*tar.Header{
		{Name: boundary, Typeflag: tar.TypeReg, Mode: 0600, Format: tar.FormatPAX},
		{Name: "native-link", Typeflag: tar.TypeSymlink, Linkname: boundary, Mode: 0777, Format: tar.FormatPAX},
	})
	if !bytes.Contains(positive, []byte(" path=")) || !bytes.Contains(positive, []byte(" linkpath=")) {
		t.Fatal("boundary fixture must use actual PAX path and link records")
	}
	if err := treeTestValidate(t, positive, MaxBundleBytes); err != nil {
		t.Fatal("bounded ordinary PAX paths were refused", err)
	}
	for name, header := range map[string]*tar.Header{
		"path-one-over":   {Name: boundary + "a", Typeflag: tar.TypeReg, Mode: 0600, Format: tar.FormatPAX},
		"target-one-over": {Name: "native-link", Typeflag: tar.TypeSymlink, Linkname: boundary + "a", Mode: 0777, Format: tar.FormatPAX},
		"utf8-byte-bound": {Name: strings.Repeat("界", MaxTreePathBytes/3+1), Typeflag: tar.TypeReg, Mode: 0600, Format: tar.FormatPAX},
	} {
		t.Run(name, func(t *testing.T) {
			if err := treeTestValidate(t, treeExpansionArchive(t, []*tar.Header{header}), MaxBundleBytes); err == nil {
				t.Fatal("overlong filesystem path or symlink target accepted")
			}
		})
	}
}

func TestNativeGuestTreeRejectsImplicitDirectoryTypeCollisions(t *testing.T) {
	file := func(name string) *tar.Header { return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600} }
	link := func(name string) *tar.Header {
		return &tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: "target", Mode: 0777}
	}
	for name, headers := range map[string][]*tar.Header{
		"file-becomes-parent":       {file("a"), file("a/b")},
		"link-becomes-parent":       {link("a"), file("a/b")},
		"implicit-dir-becomes-file": {file("a/b"), file("a")},
		"implicit-dir-becomes-link": {file("a/b"), link("a")},
	} {
		t.Run(name, func(t *testing.T) {
			if err := treeTestValidate(t, treeExpansionArchive(t, headers), MaxBundleBytes); err == nil {
				t.Fatal("filesystem parent collision reached restoration")
			}
		})
	}
}
