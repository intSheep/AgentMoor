package snapshot

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/intSheep/AgentMoor/internal/asset"
)

// PartRange locates one object body in the original frozen tar. Duplicate
// digests share one range, allowing the uploader to send each body only once.
type PartRange struct {
	Offset int64
	Size   int64
}

type countedReader struct {
	r io.Reader
	n int64
}

func (r *countedReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

// PartitionTar derives a bounded representation from the verified frozen blob.
// It never opens the live workspace or re-encodes tar headers. Small framing
// bytes stay inline; file bodies are independently content-addressed so that
// changes to a path, mode or neighboring file do not invalidate those bodies.
func PartitionTar(ctx context.Context, blob Blob) (asset.TarParts, map[string]PartRange, error) {
	m := asset.TarParts{Format: asset.TarPartsFormat}
	ranges := make(map[string]PartRange)
	fail := func(err error) (asset.TarParts, map[string]PartRange, error) {
		return asset.TarParts{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	decoded, err := hex.DecodeString(blob.SHA256)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != blob.SHA256 || blob.Size < 0 || blob.Size > asset.MaxBytes {
		return fail(fmt.Errorf("invalid frozen tar identity"))
	}
	before, err := os.Lstat(blob.Path)
	if err != nil {
		return fail(err)
	}
	if !before.Mode().IsRegular() || before.Size() != blob.Size {
		return fail(fmt.Errorf("frozen tar size or type differs"))
	}
	f, err := os.Open(blob.Path)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !stable(before, opened) {
		return fail(fmt.Errorf("frozen tar changed before partitioning"))
	}
	whole := sha256.New()
	position := &countedReader{r: io.TeeReader(cancelReader{ctx, io.LimitReader(f, blob.Size+1)}, whole)}
	archive := tar.NewReader(position)
	var end, inlineTotal int64
	appendPart := func(p asset.TarPart) error {
		if len(m.Parts) >= asset.MaxTarParts {
			return fmt.Errorf("tar-parts part limit exceeded")
		}
		m.Parts = append(m.Parts, p)
		return nil
	}
	appendInline := func(offset, size int64) error {
		if offset < 0 || size < 0 || offset > blob.Size || size > blob.Size-offset || size > asset.MaxTarPartsInlineBytes-inlineTotal {
			return fmt.Errorf("tar-parts inline limit exceeded")
		}
		inlineTotal += size
		for size > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			n := min(size, int64(asset.MaxTarPartInlineBytes))
			raw := make([]byte, int(n))
			if _, err := f.ReadAt(raw, offset); err != nil {
				return err
			}
			if err := appendPart(asset.TarPart{Size: n, Inline: raw}); err != nil {
				return err
			}
			offset += n
			size -= n
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(err)
		}
		switch header.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
		case tar.TypeDir, tar.TypeSymlink:
			if header.Size != 0 {
				return fail(fmt.Errorf("non-file tar entry has a body"))
			}
		default:
			return fail(fmt.Errorf("unsupported tar entry in frozen workspace"))
		}
		if header.Size < 0 || position.n > blob.Size || header.Size > blob.Size-position.n {
			return fail(fmt.Errorf("invalid frozen tar body size"))
		}
		if err := appendInline(end, position.n-end); err != nil {
			return fail(err)
		}
		remaining := header.Size
		for remaining > 0 {
			start := position.n
			n := min(remaining, asset.MaxTarPartBytes)
			h := sha256.New()
			if _, err := io.CopyN(h, archive, n); err != nil {
				return fail(err)
			}
			if position.n-start != n {
				return fail(fmt.Errorf("unsupported synthetic tar body"))
			}
			digest := hex.EncodeToString(h.Sum(nil))
			if old, found := ranges[digest]; found && old.Size != n {
				return fail(fmt.Errorf("inconsistent frozen tar object size"))
			}
			if _, found := ranges[digest]; !found {
				ranges[digest] = PartRange{Offset: start, Size: n}
			}
			if err := appendPart(asset.TarPart{SHA256: digest, Size: n}); err != nil {
				return fail(err)
			}
			remaining -= n
		}
		end = position.n
	}
	// tar.Reader stops at its end marker. Keep any remaining raw framing bytes,
	// but never accept an unbounded trailer as inline metadata.
	if blob.Size-end > asset.MaxTarPartsInlineBytes-inlineTotal {
		return fail(fmt.Errorf("tar-parts inline limit exceeded"))
	}
	if _, err := io.Copy(io.Discard, position); err != nil {
		return fail(err)
	}
	if position.n != blob.Size || hex.EncodeToString(whole.Sum(nil)) != blob.SHA256 {
		return fail(fmt.Errorf("frozen tar checksum or size mismatch"))
	}
	if err := appendInline(end, blob.Size-end); err != nil {
		return fail(err)
	}
	after, err := os.Lstat(blob.Path)
	if err != nil {
		return fail(err)
	}
	if !stable(before, after) {
		return fail(fmt.Errorf("frozen tar changed while partitioning"))
	}
	change := asset.Change{Metadata: asset.Metadata{Key: asset.Key{Kind: "workspace", ID: "root"}, AdapterVersion: "partition", Format: "workspace-tar.v1", MediaType: "application/x-tar", Name: "root", Origin: "runtime", Completeness: "persisted-files", SHA256: blob.SHA256, Size: blob.Size}}
	if err := m.Validate(change); err != nil {
		return fail(err)
	}
	return m, ranges, nil
}
