package platform

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/task"
)

func TestTaskReceiptPinsOnlyDeclaredArtifactsForItsTurn(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := scope("multiple-artifacts")
	session := uuid.NewString()
	base := taskAddress{Scope: s}
	spec := task.Spec{Mode: task.FileTurn, Instruction: "report", Output: "report.json"}
	if _, err := c.submitTask(t.Context(), base, spec); err != nil {
		t.Fatal(err)
	}
	// The trusted worker's explicit list prevents a prior partial publication
	// or a different turn from silently joining the successful delivery.
	for _, turn := range []string{"", uuid.NewString()} {
		a := taskAddress{Scope: s, turn: turn}
		if turn != "" {
			if _, err := c.submitTurn(t.Context(), a, task.Question{SessionID: session, Message: "revise"}); err != nil {
				t.Fatal(err)
			}
		}
		attempt := uuid.NewString()
		if _, err := c.claimTask(t.Context(), a, task.Claim{Attempt: attempt}); err != nil {
			t.Fatal(err)
		}
		publish := func(key asset.Key, content, native string, base int64) task.Ref {
			t.Helper()
			ch := change(content, base)
			ch.Key, ch.SessionID = task.TurnKey(turn, key), native
			ticket := upload(t, c, s, ch, content)
			version, err := c.Commit(t.Context(), s, ticket.ID)
			if err != nil {
				t.Fatal(err)
			}
			return task.Ref{Key: ch.Key, Version: version, SHA256: ch.SHA256, ContentPath: ch.Key.ContentPath(version)}
		}
		primary := publish(asset.Key{Kind: "artifact", ID: "out/report.json"}, "report", session, 0)
		second := publish(asset.Key{Kind: "artifact", ID: "out/details.csv"}, "details", session, 0)
		publish(asset.Key{Kind: "artifact", ID: "out/unlisted.txt"}, "partial stale file", session, 0)
		wrongSession := publish(asset.Key{Kind: "artifact", ID: "out/foreign.txt"}, "foreign", "another-session", 0)
		publish(asset.Key{Kind: "workspace", ID: "root"}, "workspace", session, 0)
		publish(asset.Key{Kind: "session", ID: session}, "session", session, 0)
		finish := task.Finish{Attempt: attempt, SessionID: session}
		for _, tc := range []struct {
			name string
			refs []task.Ref
			err  error
		}{
			{"missing required output", []task.Ref{second}, ErrNotReady},
			{"duplicate", []task.Ref{primary, primary}, errBadRequest},
			{"other native session", []task.Ref{primary, wrongSession}, ErrNotReady},
			{"cross-turn", []task.Ref{primary, {Key: asset.Key{Kind: "artifact", ID: "turns/" + uuid.NewString() + "/out/report.json"}, Version: 1}}, errBadRequest},
			{"digest mismatch", []task.Ref{{Key: primary.Key, Version: primary.Version, SHA256: "wrong", ContentPath: primary.ContentPath}}, ErrConflict},
			{"missing version", []task.Ref{{Key: primary.Key, Version: 99, SHA256: primary.SHA256, ContentPath: primary.Key.ContentPath(99)}}, ErrNotReady},
		} {
			t.Run(turn+"/"+tc.name, func(t *testing.T) {
				finish.Artifacts = tc.refs
				if _, err := c.finishTask(t.Context(), a, finish); !errors.Is(err, tc.err) {
					t.Fatalf("finish = %v, want %v", err, tc.err)
				}
			})
		}
		// Publish a newer head after the worker froze its receipt. Its finish
		// must still pin the exact already-verified bytes, never the new head.
		publish(asset.Key{Kind: "artifact", ID: "out/report.json"}, "newer head", session, 1)
		finish.Artifacts = []task.Ref{second, primary}
		got, err := c.finishTask(t.Context(), a, finish)
		if err != nil || got.State != task.Succeeded || len(got.Assets) != 4 || got.Assets[0] != primary || got.Assets[1] != second {
			t.Fatal(got, err)
		}
		finish.Artifacts = []task.Ref{wrongSession}
		replay, err := c.finishTask(t.Context(), a, finish)
		if err != nil || len(replay.Assets) != 4 || replay.Assets[0] != primary {
			t.Fatal("late acknowledgement changed a frozen receipt", replay, err)
		}
	}
}
