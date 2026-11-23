package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"agentmoor/internal/asset"
)

type cursor struct {
	Version     int64  `json:"version"`
	Fingerprint string `json:"fingerprint"`
}

type pending struct {
	Change asset.Change `json:"change"`
	Blob   string       `json:"blob"`
}

type Journal struct {
	Scope       asset.Scope       `json:"scope"`
	Endpoint    string            `json:"endpoint"`
	State       string            `json:"state"`
	Error       string            `json:"error,omitempty"`
	LastSuccess time.Time         `json:"last_success"`
	Cursors     map[string]cursor `json:"cursors"`
	Pending     *pending          `json:"pending,omitempty"`
}

func acquire(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another worker owns this checkpoint: %w", err)
	}
	return f, nil
}

func load(c Config) (Journal, error) {
	j := Journal{Scope: c.Scope, Endpoint: c.Endpoint, Cursors: map[string]cursor{}}
	b, err := os.ReadFile(filepath.Join(c.StateDir, "journal.json"))
	if os.IsNotExist(err) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	if err = json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	if j.Scope != c.Scope || j.Endpoint != c.Endpoint || j.Cursors == nil {
		return j, fmt.Errorf("journal belongs to another runtime or platform")
	}
	if j.Pending != nil && (!strings.HasPrefix(j.Pending.Blob, "asset-") || filepath.Base(j.Pending.Blob) != j.Pending.Blob || j.Pending.Change.Validate() != nil) {
		return j, fmt.Errorf("invalid pending journal")
	}
	return j, nil
}

func LocalStatus(c Config) (Journal, error) {
	j, err := load(c)
	if err != nil || j.State != "running" {
		return j, err
	}
	f, err := os.Open(filepath.Join(c.StateDir, "worker.lock"))
	if err != nil {
		return j, err
	}
	defer f.Close()
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		// Reread after locking in case the worker completed between the reads.
		j, err = load(c)
		if err == nil && j.State == "running" {
			j.State = "interrupted"
		}
		return j, err
	}
	if err == unix.EWOULDBLOCK {
		return j, nil
	}
	return j, err
}

func (j *Journal) save(dir string) error {
	f, err := os.CreateTemp(dir, ".journal-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(j)
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
	if err = os.Rename(f.Name(), filepath.Join(dir, "journal.json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
