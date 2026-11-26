//go:build integration

package platform

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"agentmoor/internal/task"
)

func TestPostgresAndS3(t *testing.T) {
	endpoint, dsn := os.Getenv("AGENTMOOR_TEST_S3"), os.Getenv("AGENTMOOR_TEST_PG")
	if endpoint == "" || dsn == "" {
		t.Fatal("use sh tests/services.sh")
	}
	objects, err := NewS3(endpoint, "agentmoor-test", "disposable-test", "disposable-test-password", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Minute); ; {
		err = objects.client.MakeBucket(t.Context(), objects.bucket, minio.MakeBucketOptions{})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
	}
	c, err := OpenCatalog(t.Context(), t.TempDir(), dsn, objects)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	checkTarPartsStorage(t, c, scope("integration-parts"))
	checkTarPartsCollection(t, c, scope("integration-parts-gc"))
	checkTurnQueue(t, c, scope("followup"))
	s := scope("integration")
	spec := task.Spec{Instruction: "summarize rows", Input: "one,two", Output: "summary.txt"}
	if _, err := c.submitTask(t.Context(), taskAddress{Scope: s}, spec); err != nil {
		t.Fatal(err)
	}
	cancelled, err := c.cancelTask(t.Context(), taskAddress{Scope: s})
	if err != nil || cancelled.State != task.Cancelled {
		t.Fatal(cancelled, err)
	}
	ch := change("persisted", 0)
	ticket := upload(t, c, s, ch, "persisted")
	if _, err = c.Commit(t.Context(), s, ticket.ID); err != nil {
		t.Fatal(err)
	}
	revision, body, err := c.Read(t.Context(), s, ch.Key, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(body)
	body.Close()
	if err != nil || string(b) != "persisted" || revision.Version != 1 {
		t.Fatal(string(b), revision, err)
	}
	abandoned := change("expired", 1)
	upload(t, c, s, abandoned, "expired")
	if n, err := c.Collect(t.Context(), s, time.Now().Add(time.Second)); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
