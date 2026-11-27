package worker

import (
	"errors"
	"net"
	"testing"

	"github.com/intSheep/AgentMoor/internal/platform"
)

func TestTaskStartupRetriesOnlyNetworkFailures(t *testing.T) {
	calls := 0
	err := connectForTask(t.Context(), func() error {
		calls++
		if calls == 1 {
			return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatal(calls, err)
	}
	calls = 0
	err = connectForTask(t.Context(), func() error { calls++; return platform.ErrMissing })
	if !errors.Is(err, platform.ErrMissing) || calls != 1 {
		t.Fatal(calls, err)
	}
}
