package platform

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/snapshot"
)

type tarFile struct{ name, body string }

func tarFixture(t *testing.T, base int64, files ...tarFile) (asset.Change, asset.TarParts, []byte, map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, file := range files {
		if err := w.WriteHeader(&tar.Header{Name: file.name, Mode: 0600, Size: int64(len(file.body)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, file.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	ch := change(string(raw), base)
	ch.Key = asset.Key{Kind: "workspace", ID: "root"}
	ch.Format, ch.MediaType, ch.Name = "workspace-tar.v1", "application/x-tar", "root"
	file := filepath.Join(t.TempDir(), "frozen.tar")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	manifest, ranges, err := snapshot.PartitionTar(t.Context(), snapshot.Blob{Path: file, SHA256: ch.SHA256, Size: ch.Size})
	if err != nil {
		t.Fatal(err)
	}
	bodies := make(map[string][]byte)
	for digest, part := range ranges {
		bodies[digest] = raw[part.Offset : part.Offset+part.Size]
	}
	return ch, manifest, raw, bodies
}

func uploadParts(t *testing.T, c *Catalog, s asset.Scope, op string, missing []asset.TarPart, bodies map[string][]byte) {
	t.Helper()
	for _, part := range missing {
		file := filepath.Join(t.TempDir(), "body")
		if err := os.WriteFile(file, bodies[part.SHA256], 0600); err != nil {
			t.Fatal(err)
		}
		if err := c.UploadPart(t.Context(), s, op, file, part.SHA256, part.Size); err != nil {
			t.Fatal(err)
		}
	}
}

// Used unchanged for both SQLite/DiskObjects and PostgreSQL/S3.
func checkTarPartsStorage(t *testing.T, c *Catalog, s asset.Scope) {
	t.Helper()
	first, _, raw1, _ := tarFixture(t, 0, tarFile{"before.txt", "legacy complete tar"})
	full := upload(t, c, s, first, string(raw1))
	if v, err := c.Commit(t.Context(), s, full.ID); err != nil || v != 1 {
		t.Fatal(v, err)
	}
	second, parts2, raw2, bodies2 := tarFixture(t, 1, tarFile{"stable.txt", "reusable file body"}, tarFile{"duplicate.txt", "reusable file body"}, tarFile{"changed.txt", "version two"})
	ticket, err := c.Begin(t.Context(), s, second)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := c.PrepareParts(t.Context(), s, ticket.ID, parts2)
	if err != nil || len(missing) != 2 {
		t.Fatal("duplicate file bodies must upload once", missing, err)
	}
	if _, err = c.Commit(t.Context(), s, ticket.ID); !errors.Is(err, ErrNotReady) {
		t.Fatal("missing dependency committed", err)
	}
	uploadParts(t, c, s, ticket.ID, missing, bodies2)
	if v, err := c.Commit(t.Context(), s, ticket.ID); err != nil || v != 2 {
		t.Fatal(v, err)
	}
	third, parts3, raw3, bodies3 := tarFixture(t, 2, tarFile{"renamed.txt", "reusable file body"}, tarFile{"changed.txt", "version three"})
	next, err := c.Begin(t.Context(), s, third)
	if err != nil {
		t.Fatal(err)
	}
	missing, err = c.PrepareParts(t.Context(), s, next.ID, parts3)
	if err != nil || len(missing) != 1 || string(bodies3[missing[0].SHA256]) != "version three" {
		t.Fatal("renaming unchanged file re-uploaded its body", missing, err)
	}
	uploadParts(t, c, s, next.ID, missing, bodies3)
	if v, err := c.Commit(t.Context(), s, next.ID); err != nil || v != 3 {
		t.Fatal(v, err)
	}
	if replay, err := c.Begin(t.Context(), s, second); err != nil || replay.ID != ticket.ID || replay.Version != 2 {
		t.Fatal("operation replay changed identity", replay, err)
	}
	if v, err := c.Commit(t.Context(), s, ticket.ID); err != nil || v != 2 {
		t.Fatal(v, err)
	}
	for version, want := range map[int64][]byte{1: raw1, 2: raw2, 3: raw3, 0: raw3} {
		rev, stream, err := c.Read(t.Context(), s, first.Key, version)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := io.ReadAll(stream)
		stream.Close()
		h := sha256.Sum256(want)
		if err != nil || !bytes.Equal(actual, want) || rev.Format != "workspace-tar.v1" || rev.SHA256 != hex.EncodeToString(h[:]) || rev.Size != int64(len(want)) {
			t.Fatal("logical tar or metadata changed", version, rev, err)
		}
	}
	if exists, err := c.objects.Exists(t.Context(), objectKey(s, second.SHA256)); err != nil || exists {
		t.Fatal("parts commit stored a second complete tar", exists, err)
	}
	other := s
	other.Runtime += "-other"
	if _, err = c.PrepareParts(t.Context(), other, next.ID, parts3); !errors.Is(err, ErrMissing) {
		t.Fatal("other scope accessed manifest", err)
	}
	third.Base = 0
	isolated, err := c.Begin(t.Context(), other, third)
	if err != nil {
		t.Fatal(err)
	}
	if missing, err := c.PrepareParts(t.Context(), other, isolated.ID, parts3); err != nil || len(missing) != 2 {
		t.Fatal("object discovery crossed scope", missing, err)
	}
}

func TestTarPartsStorage(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	checkTarPartsStorage(t, c, scope("parts"))
}

func TestTarPartsRecoveryAndAtomicPublication(t *testing.T) {
	dir := t.TempDir()
	objects := DiskObjects{t.TempDir()}
	open := func() *Catalog {
		c, err := OpenCatalog(t.Context(), dir, "", objects)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	s := scope("recover-parts")
	ch, parts, raw, bodies := tarFixture(t, 0, tarFile{"one", "recoverable candidate"})
	ticket, err := c.Begin(t.Context(), s, ch)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := c.PrepareParts(t.Context(), s, ticket.ID, parts)
	if err != nil {
		t.Fatal(err)
	}
	uploadParts(t, c, s, ticket.ID, missing, bodies)
	if _, err = c.db.Exec("CREATE TRIGGER fail_parts_commit BEFORE INSERT ON revisions BEGIN SELECT RAISE(FAIL,'outage'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Commit(t.Context(), s, ticket.ID); err == nil {
		t.Fatal("expected failed transaction")
	}
	var references int
	if err = c.db.QueryRow("SELECT COUNT(*) FROM revision_parts").Scan(&references); err != nil || references != 0 {
		t.Fatal("representation escaped failed revision transaction", references, err)
	}
	if _, err = c.db.Exec("DROP TRIGGER fail_parts_commit"); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = open()
	defer c.Close()
	if retried, err := c.Begin(t.Context(), s, ch); err != nil || retried.ID != ticket.ID {
		t.Fatal(retried, err)
	}
	if missing, err := c.PrepareParts(t.Context(), s, ticket.ID, parts); err != nil || len(missing) != 0 {
		t.Fatal("uploaded candidates lost across restart", missing, err)
	}
	if v, err := c.Commit(t.Context(), s, ticket.ID); err != nil || v != 1 {
		t.Fatal(v, err)
	}
	_, stream, err := c.Read(t.Context(), s, ch.Key, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(stream)
	stream.Close()
	if err != nil || !bytes.Equal(raw, got) {
		t.Fatal("restart changed original tar", err)
	}
}

func TestTarPartsResumeIgnoresLegacyObjectPresence(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := scope("resume-parts")
	ch, parts, raw, bodies := tarFixture(t, 0, tarFile{"one", "body not uploaded yet"})
	ticket, err := c.Begin(t.Context(), s, ch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.PrepareParts(t.Context(), s, ticket.ID, parts); err != nil {
		t.Fatal(err)
	}
	legacy := ch
	legacy.ID = "other-workspace"
	full := upload(t, c, s, legacy, string(raw))
	if _, err = c.Commit(t.Context(), s, full.ID); err != nil {
		t.Fatal(err)
	}
	if resumed, err := c.Begin(t.Context(), s, ch); err != nil || !resumed.Upload || resumed.ID != ticket.ID {
		t.Fatal("legacy object presence bypassed unfinished frozen manifest", resumed, err)
	}
	missing, err := c.PrepareParts(t.Context(), s, ticket.ID, parts)
	if err != nil || len(missing) != 1 {
		t.Fatal(missing, err)
	}
	uploadParts(t, c, s, ticket.ID, missing, bodies)
	if v, err := c.Commit(t.Context(), s, ticket.ID); err != nil || v != 1 {
		t.Fatal(v, err)
	}
}

type lostPartPutObjects struct{ Objects }

func (o lostPartPutObjects) Put(ctx context.Context, key, src string, size int64) error {
	if err := o.Objects.Put(ctx, key, src, size); err != nil {
		return err
	}
	return errors.New("object stored but acknowledgement lost")
}

func TestTarPartsCandidateSurvivesInterruptedPut(t *testing.T) {
	dir := t.TempDir()
	objects := DiskObjects{t.TempDir()}
	c, err := OpenCatalog(t.Context(), dir, "", lostPartPutObjects{objects})
	if err != nil {
		t.Fatal(err)
	}
	s := scope("interrupted-parts")
	ch, parts, _, bodies := tarFixture(t, 0, tarFile{"body", "persisted before failure"})
	ticket, err := c.Begin(t.Context(), s, ch)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := c.PrepareParts(t.Context(), s, ticket.ID, parts)
	if err != nil || len(missing) != 1 {
		t.Fatal(missing, err)
	}
	p := missing[0]
	file := filepath.Join(t.TempDir(), "spool")
	if err = os.WriteFile(file, bodies[p.SHA256], 0600); err != nil {
		t.Fatal(err)
	}
	if err = c.UploadPart(t.Context(), s, ticket.ID, file, p.SHA256, p.Size); err == nil {
		t.Fatal("synthetic put acknowledgement loss was hidden")
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = OpenCatalog(t.Context(), dir, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if exists, err := objects.Exists(t.Context(), partKey(s, p.SHA256)); err != nil || !exists {
		t.Fatal("test must contain a real stored object after failure", exists, err)
	}
	if n, err := c.Collect(t.Context(), s, time.Now().Add(time.Second)); err != nil || n != 1 {
		t.Fatal("crash orphan candidate was not durable", n, err)
	}
}

type countedPartObjects struct {
	Objects
	gets, open, maxOpen int
}

type countedPartStream struct {
	io.ReadCloser
	owner *countedPartObjects
}

func (s countedPartStream) Close() error {
	s.owner.open--
	return s.ReadCloser.Close()
}

func (o *countedPartObjects) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	r, err := o.Objects.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	o.gets++
	o.open++
	o.maxOpen = max(o.maxOpen, o.open)
	return countedPartStream{r, o}, nil
}

func TestTarPartsReadIsLazyAndClosesStreams(t *testing.T) {
	objects := &countedPartObjects{Objects: DiskObjects{t.TempDir()}}
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", objects)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := scope("lazy-parts")
	ch, parts, raw, bodies := tarFixture(t, 0, tarFile{"one", "first body"}, tarFile{"two", "second body"})
	ticket, err := c.Begin(t.Context(), s, ch)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := c.PrepareParts(t.Context(), s, ticket.ID, parts)
	if err != nil {
		t.Fatal(err)
	}
	uploadParts(t, c, s, ticket.ID, missing, bodies)
	if _, err = c.Commit(t.Context(), s, ticket.ID); err != nil {
		t.Fatal(err)
	}
	objects.gets, objects.maxOpen = 0, 0
	_, r, err := c.Read(t.Context(), s, ch.Key, 1)
	if err != nil || objects.gets != 0 {
		t.Fatal("read eagerly opened body objects", objects.gets, err)
	}
	if _, err = io.CopyN(io.Discard, r, 513); err != nil {
		t.Fatal(err)
	}
	if objects.gets != 1 || objects.open != 1 {
		t.Fatal("partial read opened later body", objects.gets, objects.open)
	}
	r.Close()
	if objects.open != 0 {
		t.Fatal("partial read leaked object stream")
	}
	_, r, err = c.Read(t.Context(), s, ch.Key, 1)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(actual, raw) || objects.maxOpen != 1 || objects.open != 0 {
		t.Fatal("lazy stream bounds or bytes changed", objects.maxOpen, objects.open, err)
	}
}

type retryRemoveObjects struct {
	Objects
	fail string
}

func (o *retryRemoveObjects) Remove(ctx context.Context, key string) error {
	if key == o.fail {
		o.fail = ""
		return errors.New("synthetic object deletion outage")
	}
	return o.Objects.Remove(ctx, key)
}

func checkTarPartsCollection(t *testing.T, c *Catalog, s asset.Scope) {
	t.Helper()
	begin := func(base int64, files ...tarFile) (asset.Ticket, asset.TarParts) {
		ch, parts, _, bodies := tarFixture(t, base, files...)
		ticket, err := c.Begin(t.Context(), s, ch)
		if err != nil {
			t.Fatal(err)
		}
		missing, err := c.PrepareParts(t.Context(), s, ticket.ID, parts)
		if err != nil {
			t.Fatal(err)
		}
		uploadParts(t, c, s, ticket.ID, missing, bodies)
		return ticket, parts
	}
	first, history := begin(0, tarFile{"history", "historical body"})
	if _, err := c.Commit(t.Context(), s, first.ID); err != nil {
		t.Fatal(err)
	}
	second, current := begin(1, tarFile{"current", "current body"})
	if _, err := c.Commit(t.Context(), s, second.ID); err != nil {
		t.Fatal(err)
	}
	stale, candidates := begin(2, tarFile{"orphan", "orphan body"}, tarFile{"pending", "pending shared body"})
	live, protected := begin(2, tarFile{"pending-renamed", "pending shared body"})
	if live.ID == stale.ID {
		t.Fatal("fixtures must use distinct operations")
	}
	old := time.Now().Add(-48 * time.Hour).UnixNano()
	if _, err := c.writer.ExecContext(t.Context(), "UPDATE operations SET updated=$1 WHERE scope=$2 AND op<>$3", old, s.Prefix(), live.ID); err != nil {
		t.Fatal(err)
	}
	orphan := distinctParts(candidates)[0].SHA256
	original := c.objects
	c.objects = &retryRemoveObjects{Objects: original, fail: partKey(s, orphan)}
	defer func() { c.objects = original }()
	before := time.Now().Add(-24 * time.Hour)
	if n, err := c.Collect(t.Context(), s, before); err == nil || n != 0 {
		t.Fatal("deletion outage was hidden", n, err)
	}
	if n, err := c.Collect(t.Context(), s, before); err != nil || n != 1 {
		t.Fatal("orphan deletion was not retryable", n, err)
	}
	if exists, err := c.objects.Exists(t.Context(), partKey(s, orphan)); err != nil || exists {
		t.Fatal("real orphan was retained", exists, err)
	}
	for _, parts := range []asset.TarParts{history, current, protected} {
		for _, p := range distinctParts(parts) {
			if exists, err := c.objects.Exists(t.Context(), partKey(s, p.SHA256)); err != nil || !exists {
				t.Fatal("history or live pending dependency collected", p, err)
			}
		}
	}
	if n, err := c.Collect(t.Context(), s, before); err != nil || n != 0 {
		t.Fatal("repeat collection changed count", n, err)
	}
}

func TestTarPartsCollection(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	checkTarPartsCollection(t, c, scope("parts-gc"))
}

func TestTarPartsRejectCorruptionAndDrift(t *testing.T) {
	objects := DiskObjects{t.TempDir()}
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", objects)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := scope("corrupt-parts")
	ch, parts, _, bodies := tarFixture(t, 0, tarFile{"body", "expected body"})
	ticket, err := c.Begin(t.Context(), s, ch)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := c.PrepareParts(t.Context(), s, ticket.ID, parts)
	if err != nil {
		t.Fatal(err)
	}
	drift := parts
	drift.Parts = append([]asset.TarPart(nil), parts.Parts...)
	drift.Parts[0].Inline = bytes.Clone(parts.Parts[0].Inline)
	drift.Parts[0].Inline[0] ^= 1
	if _, err = c.PrepareParts(t.Context(), s, ticket.ID, drift); !errors.Is(err, ErrConflict) {
		t.Fatal("operation representation drift accepted", err)
	}
	uploadParts(t, c, s, ticket.ID, missing, bodies)
	key := partKey(s, missing[0].SHA256)
	for _, body := range [][]byte{[]byte("wrong body!!!"), []byte("short"), []byte("expected body-extra")} {
		if err = os.WriteFile(filepath.Join(objects.Dir, key), body, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = c.Commit(t.Context(), s, ticket.ID); !errors.Is(err, errBadRequest) {
			t.Fatal("corrupt CAS body committed", err)
		}
	}
	if err = os.WriteFile(filepath.Join(objects.Dir, key), bodies[missing[0].SHA256], 0600); err != nil {
		t.Fatal(err)
	}
	// A shape-valid manifest with incorrect inline framing must also fail the
	// logical checksum, even though all referenced object bodies are valid.
	badChange := ch
	badChange.SHA256 = hex.EncodeToString(bytes.Repeat([]byte{9}, sha256.Size))
	badTicket, err := c.Begin(t.Context(), s, badChange)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.PrepareParts(t.Context(), s, badTicket.ID, parts); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Commit(t.Context(), s, badTicket.ID); !errors.Is(err, errBadRequest) {
		t.Fatal("wrong logical tar checksum committed", err)
	}
	if revisions, err := c.List(t.Context(), s); err != nil || len(revisions) != 0 {
		t.Fatal(revisions, err)
	}
}
