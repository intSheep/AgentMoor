package worker_test

import (
	"testing"

	"github.com/intSheep/AgentMoor/internal/worker/contracttest"
	"github.com/intSheep/AgentMoor/internal/worker/contracttest/codexcase"
	"github.com/intSheep/AgentMoor/internal/worker/contracttest/opencodecase"
)

func TestAdapterAssetContract(t *testing.T) {
	for _, c := range []contracttest.Case{codexcase.New(), opencodecase.New()} {
		contracttest.Run(t, c)
	}
}
