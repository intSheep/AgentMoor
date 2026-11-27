package platform

import (
	"context"
	"database/sql"
	"strings"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

// appendAssetLinks runs only on the first successful Finish, in the same
// leased transaction as the task and success event. The address and native
// Session ID come from that authenticated operation, never asset-path parsing.
func appendAssetLinks(ctx context.Context, tx *sql.Tx, address taskAddress, slot taskSlot) error {
	if slot.State != task.Succeeded || slot.linkedSessionID == "" || uuid.Validate(slot.attempt) != nil ||
		len(slot.Assets) < 3 || len(slot.Assets) > task.MaxArtifacts+2 {
		return ErrAudit
	}
	sessionKey := task.TurnKey(address.turn, asset.Key{Kind: "session", ID: slot.linkedSessionID})
	workspaceKey := task.TurnKey(address.turn, asset.Key{Kind: "workspace", ID: "root"})
	primaryKey := task.TurnKey(address.turn, asset.Key{Kind: "artifact", ID: "out/" + slot.Spec.Output})
	artifactPrefix := task.TurnKey(address.turn, asset.Key{Kind: "artifact", ID: "out"}).ID + "/"
	seen := make(map[asset.Key]bool, len(slot.Assets))
	for _, ref := range slot.Assets {
		if !ref.Key.Valid() || seen[ref.Key] || ref.Version < 1 || !sandboxDigest(ref.SHA256) || ref.ContentPath != ref.Key.ContentPath(ref.Version) {
			return ErrAudit
		}
		seen[ref.Key] = true
		switch ref.Kind {
		case "workspace":
			if ref.Key != workspaceKey {
				return ErrAudit
			}
		case "session":
			if ref.Key != sessionKey {
				return ErrAudit
			}
		case "artifact":
			if !strings.HasPrefix(ref.ID, artifactPrefix) {
				return ErrAudit
			}
		default:
			return ErrAudit
		}
	}
	if !seen[sessionKey] || !seen[workspaceKey] || !seen[primaryKey] {
		return ErrAudit
	}
	for _, ref := range slot.Assets {
		event := AuditEvent{Scope: address.Scope, Actor: auditActor(ctx, "internal"), Action: "asset.linked",
			TaskID: address.Runtime, TurnID: address.turn, AttemptSHA256: auditHash("attempt", slot.attempt),
			SessionIDSHA256: auditHash("session-id", slot.linkedSessionID), State: task.Succeeded,
			AssetKind: ref.Kind, AssetIDSHA256: auditHash("asset-id/"+ref.Kind, ref.ID), Version: ref.Version, Digest: ref.SHA256}
		if err := appendAudit(ctx, tx, event); err != nil {
			return err
		}
	}
	return nil
}
