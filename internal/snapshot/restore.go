package snapshot

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/intSheep/AgentMoor/internal/asset"
)

// Restore verifies the entire payload before exposing a new target. The caller
// selects a trusted parent; an existing destination always remains untouched.
func Restore(ctx context.Context, r io.Reader, meta asset.Metadata, dest string, tree bool) (err error) {
	parent := filepath.Dir(dest)
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("restore conflict: %s already exists", dest)
	} else if !os.IsNotExist(err) {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".agentmoor-restore-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, RemoveDirectory(stage)) }()
	blob := filepath.Join(stage, "payload")
	f, err := os.OpenFile(blob, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), cancelReader{ctx, io.LimitReader(r, meta.Size+1)})
	if err != nil {
		return err
	}
	if n != meta.Size || hex.EncodeToString(h.Sum(nil)) != meta.SHA256 {
		return fmt.Errorf("snapshot checksum or size mismatch")
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if !tree {
		return publish(blob, dest)
	}
	if _, err = f.Seek(0, 0); err != nil {
		return err
	}
	out := filepath.Join(stage, "tree")
	if err = os.Mkdir(out, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(out)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = unpack(ctx, root, tar.NewReader(f), true); err != nil {
		return err
	}
	return publish(out, dest)
}

func unpack(ctx context.Context, root *os.Root, tr *tar.Reader, allowLinks bool) error {
	seen := map[string]bool{}
	remaining := asset.MaxBytes
	type perm struct {
		name string
		mode os.FileMode
	}
	var dirs []perm
	var links []struct{ name, target string }
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if (!asset.SafePath(h.Name) && h.Name != ".") || seen[h.Name] || len(seen) >= 1_000_000 || h.Mode < 0 || h.Mode > 0777 {
			return fmt.Errorf("invalid archive entry %q", h.Name)
		}
		seen[h.Name] = true
		mode := os.FileMode(h.Mode)
		switch h.Typeflag {
		case tar.TypeSymlink:
			if !allowLinks || !safeLink(h.Name, h.Linkname) {
				return fmt.Errorf("invalid link %q", h.Name)
			}
			links = append(links, struct{ name, target string }{h.Name, h.Linkname})
		case tar.TypeDir:
			if err = root.MkdirAll(h.Name, 0700); err != nil {
				return err
			}
			dirs = append(dirs, perm{h.Name, mode})
		case tar.TypeReg:
			if h.Size < 0 || h.Size > remaining {
				return fmt.Errorf("expanded snapshot exceeds size limit")
			}
			remaining -= h.Size
			if h.Name == "." {
				return fmt.Errorf("invalid file name")
			}
			if err = root.MkdirAll(filepath.Dir(h.Name), 0700); err != nil {
				return err
			}
			f, err := root.OpenFile(h.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, cancelReader{ctx, tr})
			if err == nil {
				err = f.Chmod(mode)
			}
			if err == nil {
				err = f.Sync()
			}
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported archive entry %q", h.Name)
		}
	}
	// Install links last. Archive entries can never redirect a subsequent write.
	for _, link := range links {
		if err := root.MkdirAll(filepath.Dir(link.name), 0700); err != nil {
			return err
		}
	}
	for _, link := range links {
		if err := root.Symlink(link.target, link.name); err != nil {
			return err
		}
	}
	// Child directories must remain accessible until all content is installed.
	slices.SortFunc(dirs, func(a, b perm) int { return len(b.name) - len(a.name) })
	for _, d := range dirs {
		if err := root.Chmod(d.name, d.mode); err != nil {
			return err
		}
	}
	return nil
}

// AddSession fills an unpublished, fresh history directory. Pinned revisions
// may share date directories, but colliding files and redirects are rejected.
func AddSession(ctx context.Context, root *os.Root, r io.Reader, meta asset.Metadata, temp string) error {
	b, err := spool(temp, func(w io.Writer) error {
		_, err := io.Copy(w, cancelReader{ctx, io.LimitReader(r, meta.Size+1)})
		return err
	})
	if err != nil {
		return err
	}
	defer b.Remove()
	if b.Size != meta.Size || b.SHA256 != meta.SHA256 {
		return fmt.Errorf("session checksum mismatch")
	}
	f, err := os.Open(b.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	if meta.MediaType == "application/x-tar" {
		return unpack(ctx, root, tar.NewReader(f), false)
	}
	if !asset.SafePath(meta.ID) || path.Base(meta.ID) != meta.ID {
		return fmt.Errorf("invalid native session ID")
	}
	out, err := root.OpenFile(meta.ID+".json", os.O_EXCL|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, cancelReader{ctx, f})
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// Require a canonical relative target. Leading ".." is allowed only within the
// archive; after an ordinary component no ".." can reinterpret a symlink.
func safeLink(name, target string) bool {
	if name == "." || target == "" || path.IsAbs(target) || path.Clean(target) != target || strings.ContainsAny(target, "\\:\x00\r\n") {
		return false
	}
	resolved := path.Join(path.Dir(name), target)
	return resolved == "." || asset.SafePath(resolved)
}

// BuildDirectory stages native importer output beside the target, on the same
// filesystem, then publishes it with the same no-replace rule as a snapshot.
func BuildDirectory(dest string, build func(string) error) error {
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("restore conflict: %s already exists", dest)
	} else if !os.IsNotExist(err) {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dest), ".agentmoor-native-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err = build(stage); err != nil {
		return err
	}
	return publish(stage, dest)
}
