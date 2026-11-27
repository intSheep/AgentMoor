// Package snapshot captures stable regular files and deterministic directory archives.
package snapshot

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
)

type Entry struct{ Source, Name string }
type Blob struct {
	Path, SHA256 string
	Size         int64
}

func (b Blob) Remove() { _ = os.Remove(b.Path) }

type limitedWriter struct {
	w    io.Writer
	left int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		return 0, fmt.Errorf("asset exceeds size limit")
	}
	n, err := l.w.Write(p)
	l.left -= int64(n)
	return n, err
}

type cancelReader struct {
	ctx context.Context
	r   io.Reader
}

func (r cancelReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func spool(temp string, write func(io.Writer) error) (result Blob, err error) {
	return spoolWithLimit(temp, asset.MaxBytes, write)
}

func spoolWithLimit(temp string, maxBytes int64, write func(io.Writer) error) (result Blob, err error) {
	if maxBytes < 1 || maxBytes > asset.MaxBytes {
		return result, fmt.Errorf("invalid snapshot size limit")
	}
	f, err := os.CreateTemp(temp, "asset-*")
	if err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	w := &limitedWriter{io.MultiWriter(f, h), maxBytes}
	err = write(w)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return result, err
	}
	return Blob{f.Name(), hex.EncodeToString(h.Sum(nil)), maxBytes - w.left}, nil
}

func stable(a, b fs.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime() == b.ModTime()
}

func copyFile(ctx context.Context, root *os.Root, src string, before fs.FileInfo, w io.Writer) error {
	if !before.Mode().IsRegular() {
		return fmt.Errorf("unsupported file type at %q", src)
	}
	f, err := root.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !stable(before, opened) {
		return fmt.Errorf("source changed: %s", src)
	}
	n, err := io.Copy(w, cancelReader{ctx, io.LimitReader(f, before.Size()+1)})
	if err != nil {
		return err
	}
	after, err := root.Lstat(src)
	if err != nil {
		return err
	}
	if n != before.Size() || !stable(before, after) {
		return fmt.Errorf("source changed: %s", src)
	}
	return nil
}

func File(ctx context.Context, root *os.Root, src, temp string) (Blob, error) {
	return FileWithLimit(ctx, root, src, temp, asset.MaxBytes)
}

// FileWithLimit preserves the regular file's original bytes while bounding
// temporary capture writes. File retains the existing platform-wide limit.
func FileWithLimit(ctx context.Context, root *os.Root, src, temp string, maxBytes int64) (Blob, error) {
	if err := ctx.Err(); err != nil {
		return Blob{}, err
	}
	info, err := root.Lstat(src)
	if err != nil {
		return Blob{}, err
	}
	if info.Size() > maxBytes {
		return Blob{}, fmt.Errorf("asset exceeds size limit")
	}
	return spoolWithLimit(temp, maxBytes, func(w io.Writer) error { return copyFile(ctx, root, src, info, w) })
}

// Tree archives the contents of each source under its portable name. A "." name
// maps the source directory to the root of the restoration.
func Tree(ctx context.Context, root *os.Root, entries []Entry, temp string) (Blob, error) {
	return TreeWithLimit(ctx, root, entries, temp, asset.MaxBytes)
}

// TreeWithLimit bounds the complete captured tar, including headers, padding
// and end markers. Tree retains the existing platform-wide limit.
func TreeWithLimit(ctx context.Context, root *os.Root, entries []Entry, temp string, maxBytes int64) (Blob, error) {
	if err := ctx.Err(); err != nil {
		return Blob{}, err
	}
	return spoolWithLimit(temp, maxBytes, func(w io.Writer) error {
		tw := tar.NewWriter(w)
		type observed struct {
			name string
			info fs.FileInfo
		}
		var seen []observed
		for _, e := range entries {
			if (e.Source != "." && !asset.SafePath(e.Source)) || (e.Name != "." && !asset.SafePath(e.Name)) {
				return fmt.Errorf("invalid snapshot entry")
			}
			err := fs.WalkDir(root.FS(), e.Source, func(src string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if err = ctx.Err(); err != nil {
					return err
				}
				info, err := root.Lstat(src)
				if err != nil {
					return err
				}
				link := info.Mode().Type() == fs.ModeSymlink
				if !info.IsDir() && !info.Mode().IsRegular() && !link {
					return fmt.Errorf("unsupported file type at %q", src)
				}
				name := e.Name
				if src != e.Source {
					rel := src
					if e.Source != "." {
						rel = src[len(e.Source)+1:]
					}
					name = path.Join(e.Name, rel)
				}
				if name != "." && !asset.SafePath(name) {
					return fmt.Errorf("nonportable path %q", name)
				}
				seen = append(seen, observed{src, info})
				if len(seen) > 1_000_000 {
					return fmt.Errorf("snapshot entry limit exceeded")
				}
				header := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
				if info.IsDir() {
					header.Typeflag = tar.TypeDir
				} else if link {
					target, err := root.Readlink(src)
					if err != nil {
						return err
					}
					if !safeLink(name, target) {
						return fmt.Errorf("link outside snapshot: %q", name)
					}
					header.Typeflag, header.Linkname = tar.TypeSymlink, target
				} else {
					header.Size = info.Size()
				}
				if err = tw.WriteHeader(header); err != nil {
					return err
				}
				if info.IsDir() || link {
					return nil
				}
				return copyFile(ctx, root, src, info, tw)
			})
			if err != nil {
				return err
			}
		}
		for _, o := range seen {
			if err := ctx.Err(); err != nil {
				return err
			}
			info, err := root.Lstat(o.name)
			if err != nil {
				return err
			}
			if !stable(o.info, info) {
				return fmt.Errorf("source changed: %s", o.name)
			}
		}
		return tw.Close()
	})
}
