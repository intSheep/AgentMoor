//go:build integration

package platform

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The first failure leaves a real S3 object and a durable pending intent. The
// armed Exists then obtains a real positive S3 response before pausing, so the
// test can terminate the PostgreSQL lease owner at the last external boundary.
type objectCollectionLeaseFault struct {
	Objects
	key              string
	denyFirstRemove  atomic.Bool
	blockNextExists  atomic.Bool
	deniedRemoves    atomic.Int64
	forwardedRemoves atomic.Int64
	entered          chan struct{}
	release          chan struct{}
	releaseOnce      sync.Once
}

func (o *objectCollectionLeaseFault) Exists(ctx context.Context, key string) (bool, error) {
	exists, err := o.Objects.Exists(ctx, key)
	if err != nil || !exists || key != o.key || !o.blockNextExists.CompareAndSwap(true, false) {
		return exists, err
	}
	close(o.entered)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-o.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, errors.New("owned lease test timed out waiting to release S3 Exists")
	}
}

func (o *objectCollectionLeaseFault) Remove(ctx context.Context, key string) error {
	if key != o.key {
		return o.Objects.Remove(ctx, key)
	}
	if o.denyFirstRemove.CompareAndSwap(true, false) {
		o.deniedRemoves.Add(1)
		return errors.New("owned pre-Remove failure preserves the pending collection")
	}
	o.forwardedRemoves.Add(1)
	return o.Objects.Remove(ctx, key)
}

func (o *objectCollectionLeaseFault) unblock() {
	o.releaseOnce.Do(func() { close(o.release) })
}

func checkObjectCollectionLeaseFailure(t *testing.T, c *Catalog, objects *S3, dir, dsn string) *Catalog {
	t.Helper()
	t.Cleanup(func() { c.Close() })
	reader, err := OpenAuditReader(t.Context(), dir, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	s := scope("collection-pg-lease-after-exists")
	body := "synthetic S3 bytes must survive loss of the PostgreSQL writer lease"
	ch := change(body, 0)
	upload(t, c, s, ch, body)
	cutoff := time.Now().Add(-24 * time.Hour)
	if _, err := c.writer.ExecContext(t.Context(), "UPDATE operations SET updated=$1 WHERE scope=$2", cutoff.Add(-time.Hour).UnixNano(), s.Prefix()); err != nil {
		t.Fatal(err)
	}
	fault := &objectCollectionLeaseFault{Objects: objects, key: objectKey(s, ch.SHA256), entered: make(chan struct{}), release: make(chan struct{})}
	fault.denyFirstRemove.Store(true)
	c.objects = fault
	collector := auditHTTPContext(t.Context(), "synthetic-collection-lease-initiator", "gc")
	if n, err := c.Collect(collector, s, cutoff); err == nil || n != 0 || fault.deniedRemoves.Load() != 1 || fault.forwardedRemoves.Load() != 0 {
		t.Fatal("lease fixture did not preserve a genuine pending deletion", n, err)
	}
	planned := collectionServiceEvents(t, reader, s)
	if len(planned) != 1 || planned[0].Action != "object.collection_planned" || planned[0].CollectionProof != "" {
		t.Fatal("lease fixture has no unique unconfirmed collection plan", planned)
	}
	if exists, err := objects.Exists(t.Context(), fault.key); err != nil || !exists {
		t.Fatal("pre-Remove fixture did not retain actual S3 bytes", exists, err)
	}

	type collectionResult struct {
		removed int
		err     error
	}
	done := make(chan collectionResult, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	owner := c
	received := false
	// Registered before starting the collector, so any Fatal below releases
	// the barrier, cancels the call and waits a bounded interval for its exit.
	t.Cleanup(func() {
		fault.unblock()
		cancel()
		if received {
			return
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned lease collector did not stop during bounded cleanup")
		}
	})
	fault.blockNextExists.Store(true)
	go func() {
		n, err := owner.Collect(auditHTTPContext(ctx, "synthetic-collection-lease-blocked", "gc"), s, cutoff)
		done <- collectionResult{n, err}
	}()
	select {
	case <-fault.entered:
	case result := <-done:
		received = true
		t.Fatal("collection returned before the selected positive S3 Exists boundary", result.removed, result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("collection never reached the selected positive S3 Exists boundary")
	}
	// This helper verifies pg_stat_activity no longer contains the original
	// writer backend; merely acknowledging pg_terminate_backend is not enough.
	terminatePostgresWriter(t, owner)
	fault.unblock()
	var result collectionResult
	select {
	case result = <-done:
		received = true
	case <-time.After(5 * time.Second):
		t.Fatal("collection did not return after the writer backend was gone")
	}
	cancel()
	observe, stopObserve := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopObserve()
	exists, existsErr := objects.Exists(observe, fault.key)
	var state string
	stateErr := c.db.QueryRowContext(observe, "SELECT state FROM object_collection_intents WHERE operation_id=$1", planned[0].OperationID).Scan(&state)
	after := collectionServiceEvents(t, reader, s)
	// Keep the actual unsafe boundary visible in the intentionally failing
	// source: the old implementation forwards one real Remove after lease loss.
	t.Logf("COLLECTION_LEASE writer_backend_gone=true positive_exists_barrier=true denied_remove=%d forwarded_remove=%d object_remained=%t collect_error=%t removed_count=%d collection_events=%d intent_state=%s", fault.deniedRemoves.Load(), fault.forwardedRemoves.Load(), exists, result.err != nil, result.removed, len(after), state)
	if result.err == nil || result.removed != 0 || fault.forwardedRemoves.Load() != 0 || existsErr != nil || !exists ||
		stateErr != nil || state != "pending" || len(after) != 1 || after[0] != planned[0] {
		t.Fatal("lost PostgreSQL lease did not prevent a new external Remove after S3 Exists", result.removed, fault.forwardedRemoves.Load(), exists, existsErr, stateErr)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	recoveryObjects := &objectCollectionLeaseFault{Objects: objects, key: fault.key}
	c, err = OpenCatalog(t.Context(), dir, dsn, recoveryObjects)
	if err != nil {
		t.Fatal("could not reopen a healthy PostgreSQL owner", err)
	}
	recoveryContext, stopRecovery := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopRecovery()
	recovery := auditHTTPContext(recoveryContext, "synthetic-collection-lease-recovery", "gc")
	if n, err := c.Collect(recovery, s, cutoff); err != nil || n != 1 || recoveryObjects.forwardedRemoves.Load() != 1 {
		t.Fatal("healthy owner did not issue one observed Remove for retained bytes", n, recoveryObjects.forwardedRemoves.Load(), err)
	}
	if exists, err := objects.Exists(recoveryContext, fault.key); err != nil || exists {
		t.Fatal("healthy recovery did not really delete S3 bytes", exists, err)
	}
	events := collectionServiceEvents(t, reader, s)
	if len(events) != 2 || events[0] != planned[0] || events[1].Action != "object.collection_confirmed" ||
		events[1].OperationID != planned[0].OperationID || events[1].CollectionProof != "remove_ack" ||
		events[1].Actor != auditActor(recovery, "internal") || events[1].Actor == planned[0].Actor {
		t.Fatal("healthy lease recovery lost the original operation or actual step actor", events)
	}
	t.Logf("COLLECTION_LEASE recovery_forwarded_remove=%d recovery_proof=remove_ack stable_operation=true distinct_step_actor=true", recoveryObjects.forwardedRemoves.Load())
	c.objects = objects
	return c
}
