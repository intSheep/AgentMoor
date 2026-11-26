package asset

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	TarPartsFormat                 = "tar-parts.v1"
	MaxTarPartBytes          int64 = 4 << 20
	MaxTarPartInlineBytes          = 64 << 10
	MaxTarPartsInlineBytes         = 8 << 20
	MaxTarPartsManifestBytes       = 16 << 20
	MaxTarParts                    = 65_536
)

// TarParts is a storage representation of an unchanged workspace-tar.v1
// payload. Parts concatenate to the original tar bytes, not a new asset format.
type TarParts struct {
	Format string    `json:"format"`
	Parts  []TarPart `json:"parts"`
}

// TarPart contains either small raw archive framing or a scoped object digest.
// Size always describes the decoded bytes, including for an inline part.
type TarPart struct {
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size"`
	Inline []byte `json:"inline,omitempty"`
}

// Validate checks bounds and shape before any object lookup or allocation.
// The caller must also hash the concatenated bytes before committing a revision.
func (m TarParts) Validate(ch Change) error {
	if err := ch.Validate(); err != nil {
		return err
	}
	if ch.Kind != "workspace" || ch.Format != "workspace-tar.v1" || ch.MediaType != "application/x-tar" || m.Format != TarPartsFormat || len(m.Parts) == 0 || len(m.Parts) > MaxTarParts {
		return fmt.Errorf("invalid tar-parts representation")
	}
	var total, inline int64
	objects := make(map[string]int64)
	for _, p := range m.Parts {
		if p.Size <= 0 || p.Size > ch.Size-total {
			return fmt.Errorf("tar-parts size differs from the logical asset")
		}
		total += p.Size
		if len(p.Inline) > 0 {
			if p.SHA256 != "" || len(p.Inline) > MaxTarPartInlineBytes || int64(len(p.Inline)) != p.Size {
				return fmt.Errorf("invalid inline tar part")
			}
			inline += p.Size
			if inline > MaxTarPartsInlineBytes {
				return fmt.Errorf("tar-parts inline limit exceeded")
			}
			continue
		}
		if !checksum.MatchString(p.SHA256) || p.Size > MaxTarPartBytes {
			return fmt.Errorf("invalid tar-parts object reference")
		}
		if size, found := objects[p.SHA256]; found && size != p.Size {
			return fmt.Errorf("inconsistent tar-parts object size")
		}
		objects[p.SHA256] = p.Size
	}
	if total != ch.Size {
		return fmt.Errorf("tar-parts size differs from the logical asset")
	}
	_, err := m.Digest()
	return err
}

// Digest identifies the canonical uncompressed manifest, independent of HTTP
// compression. Validate must be called with the logical change before use.
func (m TarParts) Digest() (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	if len(raw) > MaxTarPartsManifestBytes {
		return "", fmt.Errorf("tar-parts manifest limit exceeded")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
