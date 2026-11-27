package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
)

func partKey(s asset.Scope, digest string) string { return s.Prefix() + "/parts/" + digest }

func (c *Catalog) operationParts(ctx context.Context, q querier, scope, id string) (asset.TarParts, bool, error) {
	var parts asset.TarParts
	var raw, digest string
	err := q.QueryRowContext(ctx, "SELECT manifest,digest FROM part_manifests WHERE scope=$1 AND op=$2", scope, id).Scan(&raw, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return parts, false, nil
	}
	if err != nil {
		return parts, false, err
	}
	if err = json.Unmarshal([]byte(raw), &parts); err != nil {
		return parts, true, err
	}
	actual, err := parts.Digest()
	if err == nil && actual != digest {
		err = errBadRequest
	}
	return parts, true, err
}

// PrepareParts freezes one representation for an operation. The durable
// candidate rows precede all object writes, including an interrupted Put.
func (c *Catalog) PrepareParts(ctx context.Context, s asset.Scope, id string, parts asset.TarParts) ([]asset.TarPart, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	scope := s.Prefix()
	o, err := getOp(ctx, c.writer, scope, id)
	if err != nil {
		return nil, err
	}
	if err = parts.Validate(o.change); err != nil {
		return nil, errors.Join(errBadRequest, err)
	}
	if o.state != "pending" && o.state != "committed" {
		return nil, ErrConflict
	}
	digest, _ := parts.Digest()
	existing, found, err := c.operationParts(ctx, c.writer, scope, id)
	if err != nil {
		return nil, err
	}
	if found {
		previous, _ := existing.Digest()
		if previous != digest {
			return nil, ErrConflict
		}
	} else {
		if o.state == "committed" {
			return nil, ErrConflict
		}
		raw, _ := json.Marshal(parts)
		tx, err := c.writer.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		_, err = tx.ExecContext(ctx, "INSERT INTO part_manifests(scope,op,digest,manifest) VALUES($1,$2,$3,$4)", scope, id, digest, string(raw))
		if err != nil {
			return nil, err
		}
		for _, p := range distinctParts(parts) {
			_, err = tx.ExecContext(ctx, "INSERT INTO operation_parts(scope,op,digest,size) VALUES($1,$2,$3,$4)", scope, id, p.SHA256, p.Size)
			if err != nil {
				return nil, err
			}
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
	}
	if o.state == "committed" {
		return []asset.TarPart{}, nil
	}
	if _, err = c.writer.ExecContext(ctx, "UPDATE operations SET updated=$1 WHERE scope=$2 AND op=$3", time.Now().UnixNano(), scope, id); err != nil {
		return nil, err
	}
	missing := []asset.TarPart{}
	for _, p := range distinctParts(parts) {
		exists, err := c.objects.Exists(ctx, partKey(s, p.SHA256))
		if err != nil {
			return nil, err
		}
		if !exists {
			missing = append(missing, p)
		}
	}
	return missing, nil
}

func distinctParts(parts asset.TarParts) []asset.TarPart {
	seen := make(map[string]bool)
	out := []asset.TarPart{}
	for _, p := range parts.Parts {
		if p.SHA256 != "" && !seen[p.SHA256] {
			seen[p.SHA256] = true
			out = append(out, asset.TarPart{SHA256: p.SHA256, Size: p.Size})
		}
	}
	return out
}

func (c *Catalog) declaredPart(ctx context.Context, q querier, s asset.Scope, id, digest string) (int64, error) {
	var size int64
	err := q.QueryRowContext(ctx, "SELECT size FROM operation_parts WHERE scope=$1 AND op=$2 AND digest=$3", s.Prefix(), id, digest).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrMissing
	}
	return size, err
}

// UploadPart accepts an already verified spool file, like Upload. Both lookup
// and write use the authenticated scope; a known hash grants no other access.
func (c *Catalog) UploadPart(ctx context.Context, s asset.Scope, id, file, digest string, size int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, err := getOp(ctx, c.writer, s.Prefix(), id)
	if err != nil {
		return err
	}
	declared, err := c.declaredPart(ctx, c.writer, s, id, digest)
	if err != nil {
		return err
	}
	if declared != size {
		return errBadRequest
	}
	if o.state == "committed" {
		return nil
	}
	if o.state != "pending" {
		return ErrConflict
	}
	if err = c.prepareObjectPut(ctx, s, objectWorkspacePart, digest); err != nil {
		return err
	}
	if err = c.objects.Put(ctx, partKey(s, digest), file, size); err != nil {
		return err
	}
	_, err = c.writer.ExecContext(ctx, "UPDATE operations SET updated=$1 WHERE scope=$2 AND op=$3", time.Now().UnixNano(), s.Prefix(), id)
	return err
}

func (c *Catalog) verifyParts(ctx context.Context, s asset.Scope, ch asset.Change, parts asset.TarParts) error {
	if err := parts.Validate(ch); err != nil {
		return errors.Join(errBadRequest, err)
	}
	for _, p := range distinctParts(parts) {
		exists, err := c.objects.Exists(ctx, partKey(s, p.SHA256))
		if err != nil {
			return err
		}
		if !exists {
			return ErrNotReady
		}
	}
	r := newPartsReader(ctx, c.objects, s, parts)
	defer r.Close()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return err
	}
	if n != ch.Size || hex.EncodeToString(h.Sum(nil)) != ch.SHA256 {
		return errBadRequest
	}
	return nil
}

// partsReader opens each body only when reached, so large revisions never
// buffer their tar or hold one S3 connection per part. Each body is bounded and
// rechecked before advancing; a corrupt or truncated object fails the stream.
type partsReader struct {
	ctx       context.Context
	objects   Objects
	scope     asset.Scope
	parts     []asset.TarPart
	next      int
	current   io.ReadCloser
	remaining int64
	hash      hash.Hash
	closed    bool
}

func newPartsReader(ctx context.Context, objects Objects, s asset.Scope, parts asset.TarParts) *partsReader {
	return &partsReader{ctx: ctx, objects: objects, scope: s, parts: parts.Parts}
}

func (r *partsReader) Close() error {
	r.closed = true
	if r.current != nil {
		err := r.current.Close()
		r.current = nil
		return err
	}
	return nil
}

func (r *partsReader) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	for !r.closed && r.next < len(r.parts) {
		if err := r.ctx.Err(); err != nil {
			r.Close()
			return 0, err
		}
		p := r.parts[r.next]
		if r.current == nil {
			if len(p.Inline) > 0 {
				r.current = io.NopCloser(bytes.NewReader(p.Inline))
			} else {
				var err error
				r.current, err = r.objects.Get(r.ctx, partKey(r.scope, p.SHA256))
				if err != nil {
					r.Close()
					return 0, err
				}
			}
			r.remaining = p.Size
			r.hash = sha256.New()
		}
		window := buf
		if int64(len(window)) > r.remaining+1 {
			window = window[:r.remaining+1]
		}
		n, err := r.current.Read(window)
		r.remaining -= int64(n)
		r.hash.Write(window[:n])
		if r.remaining < 0 {
			err = errBadRequest
		}
		if err == io.EOF {
			if r.remaining != 0 || (p.SHA256 != "" && hex.EncodeToString(r.hash.Sum(nil)) != p.SHA256) {
				err = errBadRequest
			} else {
				err = r.current.Close()
				r.current = nil
				r.next++
			}
		}
		if err != nil {
			r.Close()
			return n, err
		}
		if n > 0 {
			return n, nil
		}
	}
	return 0, io.EOF
}

// Candidate records intentionally survive deletion. A crash after a successful
// Put, or a failed Remove, therefore remains collectable on the next pass.
func (c *Catalog) collectParts(ctx context.Context, s asset.Scope, before time.Time) (int, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT DISTINCT p.digest FROM operation_parts p JOIN operations o ON o.scope=p.scope AND o.op=p.op
 WHERE p.scope=$1 AND o.state<>'pending' AND o.updated<$2
 AND NOT EXISTS (SELECT 1 FROM revision_parts r JOIN operation_parts live ON live.scope=r.scope AND live.op=r.op WHERE live.scope=p.scope AND live.digest=p.digest)
 AND NOT EXISTS (SELECT 1 FROM operation_parts active JOIN operations pending ON pending.scope=active.scope AND pending.op=active.op WHERE active.scope=p.scope AND active.digest=p.digest AND pending.state='pending')`, s.Prefix(), before.UnixNano())
	if err != nil {
		return 0, err
	}
	var hashes []string
	for rows.Next() {
		var digest string
		if err = rows.Scan(&digest); err != nil {
			rows.Close()
			return 0, err
		}
		hashes = append(hashes, digest)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, digest := range hashes {
		removed, err := c.collectObject(ctx, s, objectWorkspacePart, digest)
		n += removed
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
