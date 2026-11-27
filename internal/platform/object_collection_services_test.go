//go:build integration

package platform

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentmoor/internal/asset"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// This transport leaves the production SDK retry budget intact. Every matching
// DELETE loses its response until that call exhausts the real SDK retries.
// In "after" mode it first receives and closes an actual S3 204 response.
type collectionS3Fault struct {
	base         http.RoundTripper
	path, mode   string
	attempts     atomic.Int64
	forwarded    atomic.Int64
	discardedACK atomic.Int64
}

func (f *collectionS3Fault) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodDelete || r.URL.Path != f.path {
		return f.base.RoundTrip(r)
	}
	f.attempts.Add(1)
	if f.mode == "before" {
		return nil, io.ErrUnexpectedEOF
	}
	f.forwarded.Add(1)
	response, err := f.base.RoundTrip(r)
	if err != nil || response.StatusCode != http.StatusNoContent {
		return response, err
	}
	_, err = io.Copy(io.Discard, response.Body)
	err = errors.Join(err, response.Body.Close())
	if err != nil {
		return nil, err
	}
	f.discardedACK.Add(1)
	return nil, io.ErrUnexpectedEOF
}

func collectionServiceEvents(t *testing.T, reader *AuditReader, s asset.Scope) []AuditEvent {
	t.Helper()
	var events []AuditEvent
	for _, event := range auditEvents(t, reader, s) {
		if strings.HasPrefix(event.Action, "object.collection_") {
			events = append(events, event)
		}
	}
	return events
}

func logCollectionServiceEvents(t *testing.T, scenario string, events []AuditEvent) {
	t.Helper()
	raw, err := json.Marshal(struct {
		Scenario string       `json:"scenario"`
		Events   []AuditEvent `json:"events"`
	}{scenario, events})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("COLLECTION_EVENT_SAMPLE %s", raw)
}

func checkObjectCollectionAuditServices(t *testing.T, c *Catalog, objects *S3, dir, dsn string) *Catalog {
	t.Helper()
	t.Cleanup(func() { c.Close() })
	reader, err := OpenAuditReader(t.Context(), dir, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	cutoff := time.Now().Add(-24 * time.Hour)
	age := func(s asset.Scope) {
		if _, err := c.writer.ExecContext(t.Context(), "UPDATE operations SET updated=$1 WHERE scope=$2", cutoff.Add(-time.Hour).UnixNano(), s.Prefix()); err != nil {
			t.Fatal(err)
		}
	}
	reopen := func() {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		c, err = OpenCatalog(t.Context(), dir, dsn, objects)
		if err != nil {
			t.Fatal(err)
		}
	}
	collector := auditHTTPContext(t.Context(), "synthetic-collection-initiator", "gc")
	recovery := auditHTTPContext(t.Context(), "synthetic-collection-recovery", "gc")
	for _, mode := range []string{"before", "after"} {
		s := scope("collection-s3-" + mode)
		ch := change("synthetic object service fault "+mode, 0)
		upload(t, c, s, ch, "synthetic object service fault "+mode)
		age(s)
		transport, err := minio.DefaultTransport(false)
		if err != nil {
			t.Fatal(err)
		}
		fault := &collectionS3Fault{base: transport, path: "/" + objects.bucket + "/" + objectKey(s, ch.SHA256), mode: mode}
		client, err := minio.New(os.Getenv("AGENTMOOR_TEST_S3"), &minio.Options{
			Creds: credentials.NewStaticV4("disposable-test", "disposable-test-password", ""), Transport: fault})
		if err != nil {
			t.Fatal(err)
		}
		c.objects = &S3{client: client, bucket: objects.bucket}
		if n, err := c.Collect(collector, s, cutoff); err == nil || n != 0 {
			t.Fatal("S3 fault was reported as confirmed deletion", mode, n, err)
		}
		transport.CloseIdleConnections()
		exists, err := objects.Exists(t.Context(), objectKey(s, ch.SHA256))
		if err != nil || exists != (mode == "before") || fault.attempts.Load() < 2 {
			t.Fatal("real S3 state or unchanged SDK retries were not observed", mode, exists, fault.attempts.Load(), err)
		}
		if mode == "before" && fault.forwarded.Load() != 0 || mode == "after" && (fault.discardedACK.Load() == 0 || fault.discardedACK.Load() != fault.forwarded.Load()) {
			t.Fatal("S3 fault did not surround actual HTTP acknowledgements", mode, fault.forwarded.Load(), fault.discardedACK.Load())
		}
		planned := collectionServiceEvents(t, reader, s)
		if len(planned) != 1 || planned[0].Action != "object.collection_planned" || planned[0].CollectionProof != "" {
			t.Fatal("uncertain external result emitted confirmation", planned)
		}
		reopen()
		want, proof := 1, "remove_ack"
		if mode == "after" {
			want, proof = 0, "absence"
		}
		if n, err := c.Collect(recovery, s, cutoff); err != nil || n != want {
			t.Fatal("real S3 restart did not finish its original intent", mode, n, err)
		}
		events := collectionServiceEvents(t, reader, s)
		if len(events) != 2 || events[1].Action != "object.collection_confirmed" || events[1].OperationID != planned[0].OperationID ||
			events[1].CollectionProof != proof || events[1].Actor == events[0].Actor {
			t.Fatal("restart changed the intent, proof or actual step actor", events)
		}
		if n, err := c.Collect(recovery, s, cutoff); err != nil || n != 0 || len(collectionServiceEvents(t, reader, s)) != 2 {
			t.Fatal("repeated S3 collection duplicated an event", n, err)
		}
		t.Logf("COLLECTION_S3 mode=%s sdk_delete_attempts=%d forwarded=%d discarded_http_204=%d recovery_proof=%s stable_operation=true", mode, fault.attempts.Load(), fault.forwarded.Load(), fault.discardedACK.Load(), proof)
		logCollectionServiceEvents(t, "s3-"+mode, events)
	}
	for _, action := range []string{"object.collection_planned", "object.collection_confirmed"} {
		suffix := strings.TrimPrefix(action, "object.collection_")
		s := scope("collection-pg-reject-" + suffix)
		body := "synthetic PostgreSQL audit rejection " + suffix
		ch := change(body, 0)
		upload(t, c, s, ch, body)
		age(s)
		_, err := c.writer.ExecContext(t.Context(), `CREATE FUNCTION reject_collection_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.event::jsonb->>'action'='`+action+`' THEN RAISE EXCEPTION 'synthetic collection audit rejection'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_collection_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_collection_audit()`)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := c.Collect(collector, s, cutoff); err == nil || n != 0 {
			t.Fatal("PostgreSQL audit rejection was hidden", action, n, err)
		}
		exists, err := objects.Exists(t.Context(), objectKey(s, ch.SHA256))
		if err != nil || exists != (suffix == "planned") {
			t.Fatal("PostgreSQL rollback was confused with external deletion", action, exists, err)
		}
		events := collectionServiceEvents(t, reader, s)
		if len(events) != map[string]int{"planned": 0, "confirmed": 1}[suffix] {
			t.Fatal("failed transaction emitted a collection event", action, events)
		}
		if _, err = c.writer.ExecContext(t.Context(), "DROP TRIGGER reject_collection_audit ON audit_events; DROP FUNCTION reject_collection_audit()"); err != nil {
			t.Fatal(err)
		}
		reopen()
		want, proof := 1, "remove_ack"
		if suffix == "confirmed" {
			want, proof = 0, "absence"
		}
		if n, err := c.Collect(recovery, s, cutoff); err != nil || n != want {
			t.Fatal("PostgreSQL restart did not recover rejected audit", action, n, err)
		}
		events = collectionServiceEvents(t, reader, s)
		if len(events) != 2 || events[1].CollectionProof != proof || events[0].OperationID != events[1].OperationID {
			t.Fatal("rejected PostgreSQL audit lost the durable collection", events)
		}
		t.Logf("COLLECTION_PG rejected=%s object_remained=%t recovery_proof=%s stable_operation=true", action, exists, proof)
		logCollectionServiceEvents(t, "pg-reject-"+suffix, events)
	}
	return checkObjectCollectionLeaseFailure(t, c, objects, dir, dsn)
}
