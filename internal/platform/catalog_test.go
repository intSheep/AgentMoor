package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/task"
)

func TestCatalogSQLitePathUsesEncodedFileURI(t *testing.T) {
	for _, part := range []string{"private ? readonly # store", "private # store with spaces"} {
		t.Run(part, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), part)
			c, err := OpenCatalog(t.Context(), dir, "", DiskObjects{Dir: filepath.Join(dir, "objects")})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			s := scope("encoded-catalog")
			if _, err = c.submitTask(t.Context(), taskAddress{Scope: s}, task.Spec{Instruction: "synthetic", Output: "report.txt"}); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(filepath.Join(dir, "catalog.sqlite")); err != nil {
				t.Fatal("catalog was created outside its literal directory", err)
			}
			r, err := OpenAuditReader(t.Context(), dir, "")
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			page, err := r.Page(t.Context(), s, 0, 0)
			if err != nil || page.Total != 1 {
				t.Fatal("literal-path read did not see same catalog", page, err)
			}
		})
	}
}

func TestCatalogRefusesLegacyQuestionMarkFilename(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "legacy ? private # store")
	legacy, _, _ := strings.Cut(filepath.Join(dir, "catalog.sqlite"), "?")
	value := []byte("synthetic old store bytes; must never be moved or replaced")
	if err := os.WriteFile(legacy, value, 0600); err != nil {
		t.Fatal(err)
	}
	if c, err := OpenCatalog(t.Context(), dir, "", DiskObjects{Dir: filepath.Join(dir, "objects")}); err == nil {
		c.Close()
		t.Fatal("silently created a new catalog beside possible old data")
	}
	got, err := os.ReadFile(legacy)
	if err != nil || !bytes.Equal(got, value) {
		t.Fatal("legacy bytes changed", string(got), err)
	}
	if _, err = os.Lstat(filepath.Join(dir, "catalog.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new catalog was created", err)
	}
	if _, err = os.Lstat(filepath.Join(dir, "catalog.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("writer lease was acquired before migration refusal", err)
	}
}

func TestCatalogRefusesRelativeLegacyFileURI(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	legacy := filepath.Join(base, "legacy")
	c, err := OpenCatalog(t.Context(), legacy, "", DiskObjects{Dir: filepath.Join(legacy, "objects")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.submitTask(t.Context(), taskAddress{Scope: scope("legacy-uri")}, task.Spec{Instruction: "old synthetic data", Output: "report.txt"}); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(legacy, "catalog.sqlite")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"file:legacy", "./file:legacy", "child/../file:legacy"} {
		if c, err = OpenCatalog(t.Context(), dir, "", DiskObjects{Dir: filepath.Join(base, "unused-objects")}); err == nil {
			c.Close()
			t.Fatal("legacy URI was silently interpreted as a new literal directory", dir)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("legacy URI database bytes changed", err)
	}
	if _, err = os.Lstat(filepath.Join(base, "file:legacy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a second literal store was created", err)
	}
	// An explicit absolute filesystem path has no old URI interpretation.
	literal := filepath.Join(base, "file:literal")
	c, err = OpenCatalog(t.Context(), literal, "", DiskObjects{Dir: filepath.Join(literal, "objects")})
	if err != nil {
		t.Fatal("absolute literal directory was rejected", err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditReaderRejectsNULDirectoryInsteadOfOpeningPrefix(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenCatalog(t.Context(), dir, "", DiskObjects{Dir: filepath.Join(dir, "objects")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	prefix := filepath.Join(dir, "catalog.sqlite") + "\x00ignored"
	if reader, err := OpenAuditReader(t.Context(), prefix, ""); err == nil {
		reader.Close()
		t.Fatal("NUL-truncated prefix database was opened")
	}
	if other, err := OpenCatalog(t.Context(), prefix, "", DiskObjects{Dir: filepath.Join(dir, "unused")}); err == nil {
		other.Close()
		t.Fatal("NUL-truncated writer was opened")
	}
}

func scope(tenant string) asset.Scope {
	return asset.Scope{Tenant: tenant, Project: "demo", Runtime: "logical-1", Adapter: "codex"}
}

func change(value string, base int64) asset.Change {
	sum := sha256.Sum256([]byte(value))
	return asset.Change{Metadata: asset.Metadata{Key: asset.Key{Kind: "artifact", ID: "out/report.txt"}, AdapterVersion: "1", Format: "file.v1", MediaType: "text/plain", Name: "report.txt", Origin: "runtime", Completeness: "persisted-files", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(value))}, Base: base}
}

func upload(t *testing.T, c *Catalog, s asset.Scope, ch asset.Change, value string) asset.Ticket {
	t.Helper()
	ticket, err := c.Begin(t.Context(), s, ch)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Upload {
		name := filepath.Join(t.TempDir(), "bytes")
		if err = os.WriteFile(name, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if err = c.Upload(t.Context(), s, ticket.ID, name, ch.SHA256, ch.Size); err != nil {
			t.Fatal(err)
		}
	}
	return ticket
}

func TestPublicationAndRecovery(t *testing.T) {
	dir := t.TempDir()
	objects := DiskObjects{filepath.Join(dir, "objects")}
	open := func() *Catalog {
		c, err := OpenCatalog(t.Context(), dir, "", objects)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	s := scope("a")
	other := scope("b")
	first := change("one", 0)
	ticket := upload(t, c, s, first, "one")
	if _, err := c.Commit(t.Context(), other, ticket.ID); !errors.Is(err, ErrMissing) {
		t.Fatal("scope crossed", err)
	}
	if _, err := c.db.Exec("CREATE TRIGGER fail_commit BEFORE INSERT ON revisions BEGIN SELECT RAISE(FAIL,'simulated outage'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Commit(t.Context(), s, ticket.ID); err == nil {
		t.Fatal("expected failed metadata transaction")
	}
	if items, err := c.List(t.Context(), s); err != nil || len(items) != 0 {
		t.Fatal(items, err)
	}
	if _, err := c.db.Exec("DROP TRIGGER fail_commit"); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = open()
	defer c.Close()
	retried, err := c.Begin(t.Context(), s, first)
	if err != nil || retried.Upload || retried.ID != ticket.ID {
		t.Fatal(retried, err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			v, err := c.Commit(t.Context(), s, ticket.ID)
			if err != nil || v != 1 {
				t.Errorf("duplicate commit: %d %v", v, err)
			}
		})
	}
	wg.Wait()
	second := upload(t, c, s, change("two", 1), "two")
	if v, err := c.Commit(t.Context(), s, second.ID); err != nil || v != 2 {
		t.Fatal(v, err)
	}
	if _, err := c.Begin(t.Context(), s, change("unpublished", 1)); !errors.Is(err, ErrConflict) {
		t.Fatal("stale base accepted", err)
	}
	back := upload(t, c, s, change("one", 2), "one")
	if v, err := c.Commit(t.Context(), s, back.ID); err != nil || v != 3 {
		t.Fatal(v, err)
	}
	replay, err := c.Begin(t.Context(), s, first)
	if err != nil || replay.Version != 1 {
		t.Fatal(replay, err)
	}
	for v, want := range map[int64]string{1: "one", 2: "two", 3: "one", 0: "one"} {
		_, body, err := c.Read(t.Context(), s, first.Key, v)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(body)
		body.Close()
		if err != nil || string(b) != want {
			t.Fatal(v, string(b), err)
		}
	}
	if _, _, err := c.Read(t.Context(), other, first.Key, 0); !errors.Is(err, ErrMissing) {
		t.Fatal(err)
	}
	orphan := change("orphan", 3)
	upload(t, c, s, orphan, "orphan")
	if n, err := c.Collect(t.Context(), s, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if n, err := c.Collect(t.Context(), s, time.Now().Add(time.Second)); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if exists, err := objects.Exists(context.Background(), objectKey(s, first.SHA256)); err != nil || !exists {
		t.Fatal("referenced blob collected", err)
	}
}
