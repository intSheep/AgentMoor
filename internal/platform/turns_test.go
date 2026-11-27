package platform

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

// The physical blobs are exercised by the cluster contract. This fixture
// isolates queue transitions so the same checks can also run on PostgreSQL.
func checkTurnQueue(t *testing.T, c *Catalog, s asset.Scope) {
	t.Helper()
	ctx := t.Context()
	base := taskAddress{Scope: s}
	native := "ses_original"
	checkpoint := []task.Ref{{Key: asset.Key{Kind: "session", ID: native}, Version: 2}, {Key: asset.Key{Kind: "workspace", ID: "root"}, Version: 3}}
	// Seed a legacy successful checkpoint without inventing modern Finish
	// authority or historical link events. Queue transitions are isolated here.
	raw, err := json.Marshal(task.Record{ID: s.Runtime, State: task.Succeeded, Assets: checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.writer.ExecContext(ctx, "INSERT INTO tasks(scope,record,attempt) VALUES($1,$2,'')", base.Prefix(), string(raw))
	if err != nil {
		t.Fatal(err)
	}
	addr := taskAddress{s, uuid.NewString()}
	next := taskAddress{s, uuid.NewString()}
	q := task.Question{SessionID: native, Message: "What was the total?"}
	accepted, err := c.submitTurn(ctx, addr, q)
	if err != nil || accepted.Parent.Session != checkpoint[0] || accepted.Parent.Workspace != checkpoint[1] {
		t.Fatal(accepted, err)
	}
	if retry, err := c.submitTurn(ctx, addr, q); err != nil || retry.TurnID != accepted.TurnID {
		t.Fatal(retry, err)
	}
	if _, err := c.submitTurn(ctx, addr, task.Question{SessionID: native, Message: "changed"}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := c.submitTurn(ctx, next, q); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	another := s
	another.Tenant = "other"
	if _, err := c.submitTurn(ctx, taskAddress{another, addr.turn}, q); !errors.Is(err, ErrMissing) {
		t.Fatal(err)
	}
	calls := 0
	ensure := func(_ context.Context, got asset.Scope, id string) (bool, error) {
		calls++
		if got != s || id != addr.turn {
			t.Fatal(got, id)
		}
		if calls == 1 {
			return false, errors.New("transient API error")
		}
		return false, nil
	}
	if c.DispatchQueued(ctx, s, ensure) == nil {
		t.Fatal("lost dispatch failure")
	}
	if err := c.DispatchQueued(ctx, s, ensure); err != nil || calls != 2 {
		t.Fatal(calls, err)
	}
	attempt := uuid.NewString()
	if _, err := c.claimTask(ctx, addr, task.Claim{Attempt: attempt}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.finishTask(ctx, addr, task.Finish{Attempt: attempt, SessionID: "ses_wrong"}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := c.cancelTask(ctx, addr); err != nil {
		t.Fatal(err)
	}
	if _, err := c.finishTask(ctx, addr, task.Finish{Attempt: attempt, Error: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.submitTurn(ctx, next, task.Question{SessionID: "suspect", Message: "hi"}); !errors.Is(err, ErrMissing) {
		t.Fatal(err)
	}
	rebound, err := c.submitTurn(ctx, next, q)
	if err != nil || !reflect.DeepEqual(rebound.Parent, accepted.Parent) {
		t.Fatal(rebound, err)
	}
	if err := c.DispatchQueued(ctx, s, func(context.Context, asset.Scope, string) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	dead, err := c.readTask(ctx, next)
	if err != nil || dead.State != task.Failed {
		t.Fatal(dead, err)
	}
	// Two clients may submit different questions against the same checkpoint.
	type admission struct {
		addr taskAddress
		err  error
	}
	results := make(chan admission, 2)
	for range 2 {
		go func() {
			addr := taskAddress{s, uuid.NewString()}
			_, err := c.submitTurn(ctx, addr, q)
			results <- admission{addr, err}
		}()
	}
	var winner taskAddress
	conflicts := 0
	for range 2 {
		r := <-results
		switch {
		case r.err == nil:
			winner = r.addr
		case errors.Is(r.err, ErrConflict):
			conflicts++
		default:
			t.Fatal(r.err)
		}
	}
	if conflicts != 1 || winner.turn == "" {
		t.Fatal("concurrent questions did not serialize", conflicts)
	}
	if _, err := c.cancelTask(ctx, winner); err != nil {
		t.Fatal(err)
	}
}

func TestFollowupQueue(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	checkTurnQueue(t, c, scope("followup"))
}

func TestAcceptedTurnSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	objects := DiskObjects{Dir: t.TempDir()}
	c, err := OpenCatalog(t.Context(), dir, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	s := scope("restart")
	checkTurnQueue(t, c, s)
	a, q := taskAddress{s, uuid.NewString()}, task.Question{SessionID: "ses_original", Message: "another"}
	_, err = c.submitTurn(t.Context(), a, q)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err = OpenCatalog(t.Context(), dir, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	called := false
	err = c.DispatchQueued(t.Context(), s, func(_ context.Context, scope asset.Scope, id string) (bool, error) {
		called = id == a.turn && scope == s
		return false, nil
	})
	if err != nil || !called {
		t.Fatal(called, err)
	}
}
