package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/intSheep/AgentMoor/internal/platform"
)

// Explicit synthetic controller attestation, not evidence of native execution
// or host resource cleanup. Real Kubernetes status is tested independently.
func fixtureGuestStopped(ctx context.Context, c *platform.Catalog, a platform.SandboxAddress, uid string, exit int) error {
	sum := sha256.Sum256([]byte(uid))
	_, err := c.RecordSandboxGuestStopped(ctx, a, platform.SandboxGuestStopProof{Kind: platform.SandboxGuestTerminated, PodUID: uid, ResourceVersion: "fixture-1", NodeName: "fixture-node", ContainerID: "containerd://" + hex.EncodeToString(sum[:]), ContainerName: "guest", ExitCode: &exit})
	return err
}
