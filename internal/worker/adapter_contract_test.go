package worker_test

import (
	"testing"

	"agentmoor/internal/worker/contracttest"
	"agentmoor/internal/worker/contracttest/codexcase"
	"agentmoor/internal/worker/contracttest/opencodecase"
)

func TestAdapterAssetContract(t *testing.T) {
	for _, c := range []contracttest.Case{codexcase.New(), opencodecase.New()} {
		contracttest.Run(t, c)
	}
}
