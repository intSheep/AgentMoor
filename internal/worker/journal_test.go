package worker

import (
	"testing"

	"github.com/intSheep/AgentMoor/internal/asset"
)

func TestStatusDistinguishesInterruptedRun(t *testing.T) {
	c := Config{Scope: asset.Scope{Tenant: "a", Project: "p", Runtime: "r", Adapter: "codex"}, Endpoint: "http://localhost", StateDir: t.TempDir()}
	lock, err := acquire(c.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	j, err := load(c)
	if err != nil {
		t.Fatal(err)
	}
	j.State = "running"
	if err = j.save(c.StateDir); err != nil {
		t.Fatal(err)
	}
	current, err := LocalStatus(c)
	if err != nil || current.State != "running" {
		t.Fatal(current, err)
	}
	lock.Close()
	current, err = LocalStatus(c)
	if err != nil || current.State != "interrupted" {
		t.Fatal(current, err)
	}
	c.Scope.Tenant = "b"
	if _, err = LocalStatus(c); err == nil {
		t.Fatal("journal reused across tenants")
	}
}
