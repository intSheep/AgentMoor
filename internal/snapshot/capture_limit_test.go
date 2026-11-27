package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"agentmoor/internal/asset"
)

func TestCapturesRespectCallerLimitsAndKeepDefaultBytes(t *testing.T) {
	dir := t.TempDir()
	content := []byte("original\x00binary\xffbytes\n")
	if err := os.WriteFile(filepath.Join(dir, "original"), content, 0751); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, tree := range []bool{false, true} {
		name := "file"
		if tree {
			name = "tree"
		}
		t.Run(name, func(t *testing.T) {
			entries := []Entry{{Source: ".", Name: "."}}
			capture := func(temp string, limit int64, legacy bool) (Blob, error) {
				if tree {
					if legacy {
						return Tree(t.Context(), root, entries, temp)
					}
					return TreeWithLimit(t.Context(), root, entries, temp, limit)
				}
				if legacy {
					return File(t.Context(), root, "original", temp)
				}
				return FileWithLimit(t.Context(), root, "original", temp, limit)
			}
			original, err := capture(t.TempDir(), 0, true)
			if err != nil {
				t.Fatal(err)
			}
			defer original.Remove()
			exact, err := capture(t.TempDir(), original.Size, false)
			if err != nil || exact.Size != original.Size || exact.SHA256 != original.SHA256 {
				t.Fatal("caller limit changed successful original bytes", original, exact, err)
			}
			defer exact.Remove()
			before, _ := os.ReadFile(original.Path)
			after, _ := os.ReadFile(exact.Path)
			if !bytes.Equal(before, after) {
				t.Fatal("bounded capture rewrote the original file or tar representation")
			}
			for _, limit := range []int64{original.Size - 1, 0, -1, asset.MaxBytes + 1} {
				temp := t.TempDir()
				failed, err := capture(temp, limit, false)
				if err == nil || failed.Path != "" {
					t.Fatal("oversize or invalid budget produced a captured blob", limit, failed, err)
				}
				files, err := os.ReadDir(temp)
				if err != nil || len(files) != 0 {
					t.Fatal("failed capture left temporary content", limit, files, err)
				}
			}
		})
	}
}

func TestCaptureLimitStopsBeforeWritingPastBudget(t *testing.T) {
	temp := t.TempDir()
	denied := errors.New("writer denied excess bytes")
	_, err := spoolWithLimit(temp, 5, func(w io.Writer) error {
		if n, err := w.Write([]byte("abc")); err != nil || n != 3 {
			t.Fatal("positive write within budget failed", n, err)
		}
		if n, err := w.Write([]byte("def")); err == nil || n != 0 {
			t.Fatal("writer passed bytes beyond the remaining budget", n, err)
		}
		files, err := os.ReadDir(temp)
		if err != nil || len(files) != 1 {
			t.Fatal(files, err)
		}
		info, err := files[0].Info()
		if err != nil || info.Size() != 3 {
			t.Fatal("temporary capture exceeded its successful write count", info, err)
		}
		return denied
	})
	if !errors.Is(err, denied) {
		t.Fatal("capture lost its caller error", err)
	}
	files, err := os.ReadDir(temp)
	if err != nil || len(files) != 0 {
		t.Fatal("partial capture was not removed", files, err)
	}
}

func TestBoundedCapturesRespectCancellationBeforeCreatingFiles(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "original"), []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tree := range []bool{false, true} {
		temp := t.TempDir()
		var err error
		if tree {
			_, err = TreeWithLimit(ctx, root, nil, temp, 1024)
		} else {
			_, err = FileWithLimit(ctx, root, "original", temp, 1024)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatal("bounded capture lost cancellation", err)
		}
		files, readErr := os.ReadDir(temp)
		if readErr != nil || len(files) != 0 {
			t.Fatal("cancelled capture created temporary files", files, readErr)
		}
	}
}
