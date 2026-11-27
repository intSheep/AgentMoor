package adapter_test

import (
	"testing"

	"github.com/intSheep/AgentMoor/internal/adapter"
	"github.com/intSheep/AgentMoor/internal/adapters/claudecode"
	"github.com/intSheep/AgentMoor/internal/adapters/codex"
)

func TestRegistry(t *testing.T) {
	registry, err := adapter.NewRegistry(codex.New(), claudecode.New())
	if err != nil {
		t.Fatal(err)
	}
	all := registry.All()
	if len(all) != 2 || all[0].Descriptor().ID != "claude-code" || all[1].Descriptor().ID != "codex" {
		t.Fatalf("unexpected registry order: %v", all)
	}
	if _, err := registry.Lookup("unregistered"); err == nil {
		t.Fatal("unregistered adapter accepted")
	}
	if _, err := adapter.NewRegistry(codex.New(), codex.New()); err == nil {
		t.Fatal("duplicate registration accepted")
	}
}
