package worker

import (
	"context"
	"fmt"

	"github.com/intSheep/AgentMoor/internal/asset"
)

func (s *scan) publishFrozen(ctx context.Context, p *pending, file string) (int64, error) {
	switch p.Transfer {
	case "":
		return s.engine.Client.Publish(ctx, p.Change, file)
	case asset.TarPartsFormat:
		return s.engine.Client.PublishTarParts(ctx, p.Change, file)
	default:
		return 0, fmt.Errorf("unsupported pending workspace transfer")
	}
}
