package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/platform"
)

// auditCommand is an operator-only local database reader. It neither accepts
// platform tokens nor adds a tenant HTTP management surface.
func auditCommand(ctx context.Context, args []string, out io.Writer) error {
	f := commandFlags("audit")
	dir := f.String("data", "", "existing private platform directory (SQLite)")
	var scope asset.Scope
	f.StringVar(&scope.Tenant, "tenant", "", "exact tenant ID")
	f.StringVar(&scope.Project, "project", "", "exact project ID")
	f.StringVar(&scope.Runtime, "runtime", "", "exact runtime ID")
	f.StringVar(&scope.Adapter, "adapter", "", "exact adapter ID")
	after := f.Int64("after", 0, "exclusive global audit cursor")
	limit := f.Int("limit", 100, "maximum events (1-1000)")
	timeout := f.Duration("timeout", 10*time.Second, "bounded read timeout (at most one minute)")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	postgres := os.Getenv("AGENTMOOR_POSTGRES_DSN")
	if f.NArg() != 0 || !scope.Valid() || *after < 0 || *limit < 1 || *limit > 1000 || *timeout <= 0 || *timeout > time.Minute || (*dir == "" && postgres == "") {
		return errors.New("audit requires an existing local store, exact scope, nonnegative cursor and bounded limit/timeout")
	}
	readCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	location := *dir
	if location != "" {
		var err error
		location, err = filepath.Abs(location)
		if err != nil {
			return errors.New("invalid audit store directory")
		}
	}
	reader, err := platform.OpenAuditReader(readCtx, location, postgres)
	if err != nil {
		return err
	}
	defer reader.Close()
	page, err := reader.Page(readCtx, scope, *after, *limit)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(page)
}
