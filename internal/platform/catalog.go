// Package platform owns the authenticated metadata and object-storage boundary.
package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sys/unix"

	"agentmoor/internal/asset"
)

var ErrConflict = errors.New("version conflict")
var ErrNotReady = errors.New("object has not been uploaded")
var ErrMissing = errors.New("asset or operation not found")

type Catalog struct {
	db     *sql.DB
	writer interface {
		querier
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	}
	objects Objects
	mu      sync.Mutex // composes conditional publication and collection for this store owner
	release func()
}

const schema = `
CREATE TABLE IF NOT EXISTS tasks (scope TEXT PRIMARY KEY, record TEXT NOT NULL, attempt TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS turns (
 scope TEXT NOT NULL, id TEXT NOT NULL, seq BIGINT NOT NULL, state TEXT NOT NULL, record TEXT NOT NULL, attempt TEXT NOT NULL,
 PRIMARY KEY(scope,id), UNIQUE(scope,seq));
CREATE INDEX IF NOT EXISTS turn_states ON turns(scope,state,seq);
CREATE TABLE IF NOT EXISTS revisions (
 scope TEXT NOT NULL, kind TEXT NOT NULL, asset_id TEXT NOT NULL, version BIGINT NOT NULL,
 digest TEXT NOT NULL, metadata TEXT NOT NULL, created BIGINT NOT NULL,
 PRIMARY KEY(scope,kind,asset_id,version));
CREATE INDEX IF NOT EXISTS revision_blobs ON revisions(scope,digest);
CREATE TABLE IF NOT EXISTS operations (
 scope TEXT NOT NULL, op TEXT NOT NULL, kind TEXT NOT NULL, asset_id TEXT NOT NULL, digest TEXT NOT NULL,
 change_json TEXT NOT NULL, state TEXT NOT NULL, message TEXT NOT NULL, version BIGINT NOT NULL, updated BIGINT NOT NULL,
 PRIMARY KEY(scope,op));
CREATE INDEX IF NOT EXISTS operation_blobs ON operations(scope,digest,state);
`

// OpenCatalog takes an exclusive service lease. Many workers may use the HTTP
// API concurrently. A second service must not collect the first one's uploads.
func OpenCatalog(ctx context.Context, dir, postgres string, objects Objects) (*Catalog, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	driver, dsn := "sqlite3", filepath.Join(dir, "catalog.sqlite")+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL"
	if postgres != "" {
		driver, dsn = "postgres", postgres
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	c := &Catalog{db: db, writer: db, objects: objects}
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()
	if postgres != "" {
		conn, err := db.Conn(ctx)
		if err != nil {
			return nil, err
		}
		var locked bool
		if err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(193674738, 1)").Scan(&locked); err != nil || !locked {
			conn.Close()
			return nil, fmt.Errorf("PostgreSQL catalog is unavailable or already in use")
		}
		c.release = func() { conn.Close() }
		// Mutations must fail closed when the connection owning the lease dies.
		c.writer = conn
	} else {
		lock, err := os.OpenFile(filepath.Join(dir, "catalog.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			lock.Close()
			return nil, fmt.Errorf("catalog is already in use: %w", err)
		}
		c.release = func() { lock.Close() }
	}
	if _, err = c.writer.ExecContext(ctx, schema); err != nil {
		return nil, err
	}
	ok = true
	return c, nil
}

func (c *Catalog) Close() error {
	if c.release != nil {
		c.release()
	}
	return c.db.Close()
}

type operation struct {
	change  asset.Change
	state   string
	version int64
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getOp(ctx context.Context, q querier, scope, id string) (operation, error) {
	var o operation
	var raw string
	err := q.QueryRowContext(ctx, "SELECT change_json,state,version FROM operations WHERE scope=$1 AND op=$2", scope, id).Scan(&raw, &o.state, &o.version)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrMissing
	}
	if err != nil {
		return o, err
	}
	err = json.Unmarshal([]byte(raw), &o.change)
	return o, err
}

func head(ctx context.Context, q querier, scope string, k asset.Key) (asset.Metadata, int64, error) {
	var m asset.Metadata
	var v int64
	var raw string
	err := q.QueryRowContext(ctx, "SELECT version,metadata FROM revisions WHERE scope=$1 AND kind=$2 AND asset_id=$3 ORDER BY version DESC LIMIT 1", scope, k.Kind, k.ID).Scan(&v, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return m, 0, nil
	}
	if err != nil {
		return m, 0, err
	}
	err = json.Unmarshal([]byte(raw), &m)
	return m, v, err
}

func objectKey(s asset.Scope, hash string) string { return s.Prefix() + "/objects/" + hash }
func (c *Catalog) Begin(ctx context.Context, s asset.Scope, ch asset.Change) (asset.Ticket, error) {
	var t asset.Ticket
	if err := ch.Validate(); err != nil {
		return t, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.ID = asset.Fingerprint(ch)
	scope := s.Prefix()
	now := time.Now().UnixNano()
	raw, err := json.Marshal(ch)
	if err != nil {
		return t, err
	}
	_, err = c.writer.ExecContext(ctx, `INSERT INTO operations(scope,op,kind,asset_id,digest,change_json,state,message,version,updated) VALUES($1,$2,$3,$4,$5,$6,'pending','',0,$7) ON CONFLICT(scope,op) DO NOTHING`, scope, t.ID, ch.Kind, ch.ID, ch.SHA256, string(raw), now)
	if err != nil {
		return t, err
	}
	o, err := getOp(ctx, c.writer, scope, t.ID)
	if err != nil {
		return t, err
	}
	if o.state == "committed" {
		t.Version = o.version
		return t, nil
	}
	m, v, err := head(ctx, c.writer, scope, ch.Key)
	if err != nil {
		return t, err
	}
	if v != ch.Base && asset.Fingerprint(m) != asset.Fingerprint(ch.Metadata) {
		return t, c.conflict(ctx, scope, t.ID)
	}
	_, err = c.writer.ExecContext(ctx, "UPDATE operations SET state='pending',message='',updated=$1 WHERE scope=$2 AND op=$3", now, scope, t.ID)
	if err != nil {
		return t, err
	}
	exists, err := c.objects.Exists(ctx, objectKey(s, ch.SHA256))
	t.Upload = !exists
	return t, err
}

func (c *Catalog) conflict(ctx context.Context, scope, id string) error {
	_, err := c.writer.ExecContext(ctx, "UPDATE operations SET state='conflict',message='base version differs from current head',updated=$1 WHERE scope=$2 AND op=$3", time.Now().UnixNano(), scope, id)
	if err != nil {
		return err
	}
	return ErrConflict
}

func (c *Catalog) Upload(ctx context.Context, s asset.Scope, id, file, digest string, size int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, err := getOp(ctx, c.writer, s.Prefix(), id)
	if err != nil {
		return err
	}
	if o.change.SHA256 != digest || o.change.Size != size {
		return fmt.Errorf("upload checksum or size mismatch")
	}
	if o.state == "committed" {
		return nil
	}
	if o.state != "pending" {
		return ErrConflict
	}
	err = c.objects.Put(ctx, objectKey(s, digest), file, size)
	if err != nil {
		_, _ = c.writer.ExecContext(ctx, "UPDATE operations SET message='object upload failed',updated=$1 WHERE scope=$2 AND op=$3", time.Now().UnixNano(), s.Prefix(), id)
	}
	return err
}

func (c *Catalog) Commit(ctx context.Context, s asset.Scope, id string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	scope := s.Prefix()
	o, err := getOp(ctx, c.writer, scope, id)
	if err != nil {
		return 0, err
	}
	if o.state == "committed" {
		return o.version, nil
	}
	if o.state != "pending" {
		return 0, ErrConflict
	}
	ch := o.change
	exists, err := c.objects.Exists(ctx, objectKey(s, ch.SHA256))
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, ErrNotReady
	}
	current, v, err := head(ctx, c.writer, scope, ch.Key)
	if err != nil {
		return 0, err
	}
	changed := asset.Fingerprint(current) != asset.Fingerprint(ch.Metadata)
	if changed && v != ch.Base {
		return 0, c.conflict(ctx, scope, id)
	}
	tx, err := c.writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	if changed {
		v++
		raw, err := json.Marshal(ch.Metadata)
		if err != nil {
			return 0, err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO revisions(scope,kind,asset_id,version,digest,metadata,created) VALUES($1,$2,$3,$4,$5,$6,$7)", scope, ch.Kind, ch.ID, v, ch.SHA256, string(raw), now)
		if err != nil {
			return 0, err
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE operations SET state='committed',message='',version=$1,updated=$2 WHERE scope=$3 AND op=$4", v, now, scope, id)
	if err != nil {
		return 0, err
	}
	return v, tx.Commit()
}

func (c *Catalog) List(ctx context.Context, s asset.Scope) ([]asset.Revision, error) {
	return c.list(ctx, s, false)
}

func (c *Catalog) Heads(ctx context.Context, s asset.Scope) ([]asset.Revision, error) {
	return c.list(ctx, s, true)
}

func (c *Catalog) list(ctx context.Context, s asset.Scope, heads bool) ([]asset.Revision, error) {
	query := "SELECT metadata,version,created FROM revisions r WHERE scope=$1"
	if heads {
		query += " AND version=(SELECT MAX(h.version) FROM revisions h WHERE h.scope=r.scope AND h.kind=r.kind AND h.asset_id=r.asset_id)"
	}
	rows, err := c.db.QueryContext(ctx, query+" ORDER BY kind,asset_id,version", s.Prefix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []asset.Revision{}
	for rows.Next() {
		var r asset.Revision
		var raw string
		var ns int64
		if err = rows.Scan(&raw, &r.Version, &ns); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &r.Metadata); err != nil {
			return nil, err
		}
		r.Scope = s
		r.Created = time.Unix(0, ns).UTC()
		r.ContentPath = r.Key.ContentPath(r.Version)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (c *Catalog) Read(ctx context.Context, s asset.Scope, k asset.Key, version int64) (asset.Revision, io.ReadCloser, error) {
	var r asset.Revision
	var raw string
	var ns int64
	err := c.db.QueryRowContext(ctx, "SELECT metadata,version,created FROM revisions WHERE scope=$1 AND kind=$2 AND asset_id=$3 AND (version=$4 OR $4=0) ORDER BY version DESC LIMIT 1", s.Prefix(), k.Kind, k.ID, version).Scan(&raw, &r.Version, &ns)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil, ErrMissing
	}
	if err != nil {
		return r, nil, err
	}
	if err = json.Unmarshal([]byte(raw), &r.Metadata); err != nil {
		return r, nil, err
	}
	r.Scope = s
	r.Created = time.Unix(0, ns).UTC()
	r.ContentPath = r.Key.ContentPath(r.Version)
	stream, err := c.objects.Get(ctx, objectKey(s, r.SHA256))
	return r, stream, err
}

func (c *Catalog) Jobs(ctx context.Context, s asset.Scope) ([]asset.Job, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT op,kind,asset_id,state,message,updated FROM operations WHERE scope=$1 ORDER BY updated DESC", s.Prefix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []asset.Job{}
	for rows.Next() {
		var j asset.Job
		var ns int64
		if err = rows.Scan(&j.Operation, &j.Kind, &j.Key.ID, &j.State, &j.Error, &ns); err != nil {
			return nil, err
		}
		j.Updated = time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
		out = append(out, j)
	}
	return out, rows.Err()
}

// Collect expires inactive operations and deletes only blobs with no revision
// and no pending writer. Failed deletions leave the operation for the next pass.
func (c *Catalog) Collect(ctx context.Context, s asset.Scope, before time.Time) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	scope := s.Prefix()
	_, err := c.writer.ExecContext(ctx, "UPDATE operations SET state='expired' WHERE scope=$1 AND state='pending' AND updated<$2", scope, before.UnixNano())
	if err != nil {
		return 0, err
	}
	rows, err := c.db.QueryContext(ctx, `SELECT DISTINCT digest FROM operations o WHERE scope=$1 AND state IN ('expired','conflict') AND updated<$2 AND NOT EXISTS (SELECT 1 FROM revisions r WHERE r.scope=o.scope AND r.digest=o.digest) AND NOT EXISTS (SELECT 1 FROM operations p WHERE p.scope=o.scope AND p.digest=o.digest AND p.state='pending')`, scope, before.UnixNano())
	if err != nil {
		return 0, err
	}
	var hashes []string
	for rows.Next() {
		var h string
		if err = rows.Scan(&h); err != nil {
			rows.Close()
			return 0, err
		}
		hashes = append(hashes, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, h := range hashes {
		key := objectKey(s, h)
		exists, err := c.objects.Exists(ctx, key)
		if err != nil {
			return n, err
		}
		if exists {
			if err = c.objects.Remove(ctx, key); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
