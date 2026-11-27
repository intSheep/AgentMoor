package snapshot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// RemoveDirectory removes an owned private staging or restored directory.
// Archives can assign mode 000 to directories, so restore owner access before
// removing them. WalkDir does not follow links; os.Root confines each chmod.
// The caller must own the path and its parent throughout the operation.
func RemoveDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("owned cleanup target is not a directory")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return errors.Join(err, os.RemoveAll(dir))
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return errors.Join(err, os.RemoveAll(dir))
	}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return root.Chmod(name, 0700)
		}
		return nil
	})
	return errors.Join(err, root.Close(), os.RemoveAll(dir))
}
