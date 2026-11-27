package adapter

import (
	"context"
	"os"

	"github.com/intSheep/AgentMoor/internal/snapshot"
)

// SessionSource reads an operator-assigned, single-scope native history root.
type SessionSource interface {
	Sessions(context.Context, *os.Root) ([]NativeSession, error)
}

type NativeSession struct {
	ID      string
	Format  string
	Entries []snapshot.Entry
	Raw     bool // Native single-file import, as opposed to a tree with sidecars.
}
