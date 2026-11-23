package worker

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch coalesces event bursts. The timer always reconciles even after a lost
// notification, a failed upload, or a directory created between watches.
func (e *Engine) Watch(ctx context.Context, interval time.Duration, events bool, log io.Writer) error {
	if interval <= 0 {
		return fmt.Errorf("interval must be positive")
	}
	if err := e.Config.Validate(); err != nil {
		return err
	}
	var err error
	var watcher *fsnotify.Watcher
	var changes <-chan fsnotify.Event
	var failures <-chan error
	if events {
		watcher, err = fsnotify.NewWatcher()
		if err != nil {
			return err
		}
		defer watcher.Close()
		changes = watcher.Events
		failures = watcher.Errors
	}
	refresh := func() {
		g, err := e.authorize(ctx)
		if err != nil {
			fmt.Fprintln(log, "authorization:", err)
			return
		}
		if !g.Read || !g.Write {
			fmt.Fprintln(log, "sync requires read and write permission")
			return
		}
		if watcher != nil {
			for _, root := range []string{e.Config.Workspace, e.Config.Sessions} {
				if root == "" {
					continue
				}
				err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if d.IsDir() {
						return watcher.Add(p)
					}
					return nil
				})
				if err != nil {
					fmt.Fprintln(log, "watch:", err)
				}
			}
		}
		if err := e.Sync(ctx); err != nil {
			fmt.Fprintln(log, "sync:", err)
		}
	}
	refresh()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var pending <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			refresh()
		case _, ok := <-changes:
			if !ok {
				return os.ErrClosed
			}
			if pending == nil {
				timer = time.NewTimer(200 * time.Millisecond)
				pending = timer.C
			}
		case err, ok := <-failures:
			if !ok {
				return os.ErrClosed
			}
			fmt.Fprintln(log, "watch:", err)
			refresh()
		case <-pending:
			pending = nil
			refresh()
		}
	}
}
