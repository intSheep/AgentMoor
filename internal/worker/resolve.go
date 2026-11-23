package worker

import (
	"context"
	"fmt"
)

// Rebase records an operator's decision to publish the pending local bytes
// over one specific reviewed platform version. Commit still uses CAS.
func (e *Engine) Rebase(ctx context.Context, expected int64) error {
	g, err := e.authorize(ctx)
	if err != nil {
		return err
	}
	if !g.Read || !g.Write {
		return fmt.Errorf("rebase requires read and write permission")
	}
	c := e.Config
	lock, err := acquire(c.StateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	j, err := load(c)
	if err != nil {
		return err
	}
	if j.State != "conflict" || j.Pending == nil || expected < 1 {
		return fmt.Errorf("no pending conflict or invalid expected version")
	}
	rev, body, err := e.Client.Content(ctx, j.Pending.Change.Key, 0)
	if err != nil {
		return err
	}
	body.Close()
	if rev.Version != expected {
		return fmt.Errorf("platform head changed to %d", rev.Version)
	}
	j.Pending.Change.Base = expected
	j.State = "pending"
	j.Error = ""
	return j.save(c.StateDir)
}
