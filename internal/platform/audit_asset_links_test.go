package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/task"
)

type auditLinksHTTP struct {
	c              *Catalog
	reader         *AuditReader
	scope          asset.Scope
	submit, worker *Client
	workerToken    string
	secret         []byte
	server         *httptest.Server
	drop           atomic.Bool
	finishCalls    atomic.Int64
}

func newAuditLinksHTTP(t *testing.T, c *Catalog, reader *AuditReader, s asset.Scope) *auditLinksHTTP {
	t.Helper()
	f := &auditLinksHTTP{c: c, reader: reader, scope: s, secret: []byte("synthetic-audit-link-signing-secret-never-log")}
	api := (&API{Catalog: c, Secret: f.secret, Temp: t.TempDir(), TaskAdapters: map[string]bool{s.Adapter: true}, FileTaskAdapters: map[string]bool{s.Adapter: true}, TurnEnabled: func(got asset.Scope) bool { return got == s }}).Handler()
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/finish") {
			api.ServeHTTP(w, r)
			return
		}
		f.finishCalls.Add(1)
		buffered := httptest.NewRecorder()
		api.ServeHTTP(buffered, r)
		if buffered.Code == http.StatusOK && f.drop.CompareAndSwap(true, false) {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close() // Commit succeeded; lose the actual TCP acknowledgement.
			return
		}
		for key, values := range buffered.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(buffered.Code)
		_, _ = io.Copy(w, buffered.Body)
	}))
	t.Cleanup(f.server.Close)
	f.submit, _ = f.client(t, s, Grant{Read: true, Submit: true})
	f.worker, f.workerToken = f.client(t, s, Grant{Read: true, Write: true, Execute: true})
	return f
}

func (f *auditLinksHTTP) client(t *testing.T, s asset.Scope, g Grant) (*Client, string) {
	t.Helper()
	g.Scope, g.Expires = s, time.Now().Add(time.Hour).Unix()
	token, err := Issue(f.secret, g, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(f.server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	transport := workerTransport.Clone()
	transport.DisableKeepAlives = true
	c.http.Transport = transport
	return c, token
}

func (f *auditLinksHTTP) route(turn string) string {
	route := "/v1/tasks/" + f.scope.Runtime
	if turn != "" {
		route += "/turns/" + turn
	}
	return route
}

func (f *auditLinksHTTP) json(t *testing.T, c *Client, method, route string, input, output any) {
	t.Helper()
	if err := c.JSON(t.Context(), method, route, input, output); err != nil {
		t.Fatal(err)
	}
}

func (f *auditLinksHTTP) publish(t *testing.T, c *Client, key asset.Key, body, session string, base int64) task.Ref {
	t.Helper()
	file := filepath.Join(t.TempDir(), "synthetic-original")
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	ch := change(body, base)
	ch.Key, ch.SessionID = key, session
	v, err := c.Publish(t.Context(), ch, file)
	if err != nil {
		t.Fatal(err)
	}
	return task.Ref{Key: key, Version: v, SHA256: ch.SHA256, ContentPath: key.ContentPath(v)}
}

func (f *auditLinksHTTP) begin(t *testing.T, turn, session string) string {
	t.Helper()
	var record task.Record
	if turn == "" {
		f.json(t, f.submit, "PUT", f.route(turn), task.Spec{Mode: task.FileTurn, Instruction: "synthetic private prompt do not log", Output: "report.json"}, &record)
	} else {
		f.json(t, f.submit, "PUT", f.route(turn), task.Question{SessionID: session, Message: "synthetic private follow-up"}, &record)
	}
	attempt := uuid.NewString()
	f.json(t, f.worker, "POST", f.route(turn)+"/claim", task.Claim{Attempt: attempt}, &record)
	return attempt
}

func (f *auditLinksHTTP) receipt(t *testing.T, turn, session, attempt, marker string) (task.Finish, []task.Ref) {
	t.Helper()
	publish := func(key asset.Key, body string, base int64) task.Ref {
		return f.publish(t, f.worker, task.TurnKey(turn, key), body, session, base)
	}
	primary := publish(asset.Key{Kind: "artifact", ID: "out/report.json"}, marker+" private report", 0)
	second := publish(asset.Key{Kind: "artifact", ID: "out/details.csv"}, marker+" private details", 0)
	publish(asset.Key{Kind: "artifact", ID: "out/unlisted.txt"}, marker+" never linked", 0)
	workspace := publish(asset.Key{Kind: "workspace", ID: "root"}, marker+" private workspace", 0)
	native := publish(asset.Key{Kind: "session", ID: session}, marker+" private native transcript", 0)
	publish(asset.Key{Kind: "artifact", ID: "out/report.json"}, marker+" newer unselected head", 1)
	return task.Finish{Attempt: attempt, SessionID: session, Artifacts: []task.Ref{second, primary}}, []task.Ref{primary, second, workspace, native}
}

func assetLinkEvents(events []AuditEvent) []AuditEvent {
	var links []AuditEvent
	for _, event := range events {
		if event.Action == "asset.linked" {
			links = append(links, event)
		}
	}
	return links
}

func requireLinks(t *testing.T, events []AuditEvent, address taskAddress, attempt, session, token string, refs []task.Ref) {
	t.Helper()
	var links []AuditEvent
	for _, event := range assetLinkEvents(events) {
		if event.TurnID == address.turn {
			links = append(links, event)
		}
	}
	if len(links) != len(refs) {
		t.Fatalf("linked %d assets, want %d", len(links), len(refs))
	}
	for i, event := range links {
		ref := refs[i]
		if event.Scope != address.Scope || event.TaskID != address.Runtime || event.TurnID != address.turn || event.AttemptSHA256 != auditHash("attempt", attempt) || event.SessionIDSHA256 != auditHash("session-id", session) || event.Component != "task-store" || event.State != task.Succeeded ||
			event.AssetKind != ref.Kind || event.AssetIDSHA256 != auditHash("asset-id/"+ref.Kind, ref.ID) || event.Version != ref.Version || event.Digest != ref.SHA256 {
			t.Fatal("authoritative link does not match the validated successful receipt", event)
		}
		if token != "" && (event.Actor.Source != "http" || event.Actor.Role != "execute" || event.Actor.CredentialSHA256 != auditHash("credential", token)) {
			t.Fatal("link actor was not the authenticated Finish executor")
		}
	}
}

func TestAuditAssetLinksHTTPInitialTwoTurnsPinnedRefsAndLostACK(t *testing.T) {
	base := newAuditFixture(t)
	s := scope("audit-links-http")
	f := newAuditLinksHTTP(t, base.c, base.r, s)
	session := "ses_private_native_identifier"
	forged := uuid.NewString()
	fake := asset.Key{Kind: "artifact", ID: "turns/" + forged + "/out/forged-ownership.txt"}
	f.publish(t, f.worker, fake, "ordinary revision must not infer a task turn", session, 0)
	if links := assetLinkEvents(auditEvents(t, base.r, s)); len(links) != 0 {
		t.Fatal("ordinary revision invented successful asset links")
	}
	var previous []AuditEvent
	var receipts []task.Record
	var attempts []string
	for index, turn := range []string{"", uuid.NewString(), uuid.NewString()} {
		attempt := f.begin(t, turn, session)
		attempts = append(attempts, attempt)
		finish, refs := f.receipt(t, turn, session, attempt, fmt.Sprint(index))
		var record task.Record
		if index == 2 {
			calls := f.finishCalls.Load()
			f.drop.Store(true)
			if err := f.worker.JSON(t.Context(), "POST", f.route(turn)+"/finish", finish, &record); err == nil {
				t.Fatal("actual lost TCP acknowledgement was accepted")
			}
			if f.finishCalls.Load() != calls+1 {
				t.Fatal("transport unexpectedly repeated the Finish POST")
			}
		} else {
			f.json(t, f.worker, "POST", f.route(turn)+"/finish", finish, &record)
		}
		events := auditEvents(t, base.r, s)
		requireLinks(t, events, taskAddress{s, turn}, attempt, session, f.workerToken, refs)
		if len(previous) > 0 && !reflect.DeepEqual(events[:len(previous)], previous) {
			t.Fatal("later success rewrote old audit history")
		}
		for _, event := range events {
			if event.Action == "revision.committed" && event.TurnID != "" {
				t.Fatal("revision path was treated as authoritative turn ownership")
			}
			if event.AssetIDSHA256 == auditHash("asset-id/artifact", fake.ID) && event.Action == "asset.linked" {
				t.Fatal("forged turn-path revision acquired a successful link")
			}
		}
		f.json(t, f.worker, "POST", f.route(turn)+"/finish", finish, &record)
		if record.State != task.Succeeded || !reflect.DeepEqual(record.Assets, refs) {
			t.Fatal("receipt did not pin old published artifact version", record)
		}
		if !reflect.DeepEqual(events, auditEvents(t, base.r, s)) {
			t.Fatal("Finish acknowledgement replay duplicated links")
		}
		var cp task.Checkpoint
		f.json(t, f.submit, "GET", f.route("")+"/checkpoint", nil, &cp)
		if cp.SessionID != session || cp.Session != refs[3] || cp.Workspace != refs[2] {
			t.Fatal("checkpoint and audited successful refs diverged")
		}
		receipts = append(receipts, record)
		previous = events
	}
	if len(assetLinkEvents(previous)) != 12 {
		t.Fatal("initial and two follow-ups did not each link all four assets")
	}
	raw, _ := json.Marshal(previous)
	for _, private := range append(attempts, session, f.workerToken, f.submit.token, string(f.secret), "synthetic private prompt do not log", "private report", "private native transcript", fake.ID, "out/report.json", "out/details.csv") {
		if bytes.Contains(raw, []byte(private)) {
			t.Fatal("audit contains raw sensitive content", private)
		}
	}
	if len(receipts) != 3 {
		t.Fatal("fixture did not finish all three turns")
	}
}

func TestAuditAssetLinksMaximumReceiptAndOneOver(t *testing.T) {
	base := newAuditFixture(t)
	s := scope("audit-links-max")
	f := newAuditLinksHTTP(t, base.c, base.r, s)
	session := "ses_private_maximum"
	attempt := f.begin(t, "", session)
	refs := make([]task.Ref, 0, task.MaxArtifacts)
	for index := range task.MaxArtifacts {
		name := fmt.Sprintf("out/part-%03d.csv", index)
		if index == 0 {
			name = "out/report.json"
		}
		refs = append(refs, f.publish(t, f.worker, asset.Key{Kind: "artifact", ID: name}, fmt.Sprint("synthetic maximum ", index), session, 0))
	}
	f.publish(t, f.worker, asset.Key{Kind: "workspace", ID: "root"}, "synthetic workspace", session, 0)
	f.publish(t, f.worker, asset.Key{Kind: "session", ID: session}, "synthetic session", session, 0)
	before := auditEvents(t, base.r, s)
	finish := task.Finish{Attempt: attempt, SessionID: session, Artifacts: append(append([]task.Ref{}, refs...), refs[0])}
	var record task.Record
	if err := f.worker.JSON(t.Context(), "POST", f.route("")+"/finish", finish, &record); err == nil {
		t.Fatal("one-over receipt accepted")
	}
	if !reflect.DeepEqual(before, auditEvents(t, base.r, s)) {
		t.Fatal("oversized receipt changed audit")
	}
	finish.Artifacts = refs
	f.json(t, f.worker, "POST", f.route("")+"/finish", finish, &record)
	if len(record.Assets) != task.MaxArtifacts+2 || len(assetLinkEvents(auditEvents(t, base.r, s))) != task.MaxArtifacts+2 {
		t.Fatal("maximum receipt did not atomically link every asset")
	}
}

func TestAuditAssetLinksRejectInvalidRefsAndNeverLinkFailedOrCancelled(t *testing.T) {
	for _, terminal := range []string{"failed", "cancelled"} {
		t.Run(terminal, func(t *testing.T) {
			base := newAuditFixture(t)
			s := scope("audit-links-negative-" + terminal)
			f := newAuditLinksHTTP(t, base.c, base.r, s)
			session := "ses_private_negative"
			attempt := f.begin(t, "", session)
			finish, refs := f.receipt(t, "", session, attempt, "negative")
			foreign := s
			foreign.Tenant = "foreign-owner"
			other, _ := f.client(t, foreign, Grant{Read: true, Write: true, Execute: true})
			alien := f.publish(t, other, asset.Key{Kind: "artifact", ID: "out/foreign.csv"}, "foreign only", session, 0)
			badDigest := refs[0]
			badDigest.SHA256 = strings.Repeat("f", 64)
			cases := []task.Finish{finish, finish, finish, finish}
			cases[0].Artifacts = []task.Ref{refs[0], alien}
			cases[1].Artifacts = []task.Ref{badDigest, refs[1]}
			cases[2].Artifacts = []task.Ref{refs[0], refs[0]}
			cases[3].Attempt = uuid.NewString()
			before := auditEvents(t, base.r, s)
			for _, bad := range cases {
				var record task.Record
				if err := f.worker.JSON(t.Context(), "POST", f.route("")+"/finish", bad, &record); err == nil {
					t.Fatal("invalid authority/ref was linked")
				}
				if !reflect.DeepEqual(before, auditEvents(t, base.r, s)) {
					t.Fatal("rejected Finish changed audit")
				}
			}
			var record task.Record
			if terminal == "cancelled" {
				f.json(t, f.submit, "POST", f.route("")+"/cancel", nil, &record)
			}
			f.json(t, f.worker, "POST", f.route("")+"/finish", task.Finish{Attempt: attempt, Error: "synthetic failure do not audit"}, &record)
			if len(assetLinkEvents(auditEvents(t, base.r, s))) != 0 {
				t.Fatal("failed/cancelled Finish produced successful links")
			}
		})
	}
}

// Trigger state sees rows already inserted by the current transaction. Thus
// rejecting link N proves rollback after success plus N-1 links were written.
func rejectNthAssetLink(t *testing.T, c *Catalog, s asset.Scope, baseline, n int, postgres bool) func() {
	t.Helper()
	var query, drop string
	threshold := baseline + n - 1
	if postgres {
		query = fmt.Sprintf("CREATE FUNCTION reject_nth_owned_asset_link() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.scope='%s' AND NEW.event::jsonb->>'action'='asset.linked' AND (SELECT count(*) FROM audit_events WHERE scope=NEW.scope AND event::jsonb->>'action'='asset.linked')=%d THEN RAISE EXCEPTION 'synthetic nth asset link failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_nth_owned_asset_link BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_nth_owned_asset_link()", s.Prefix(), threshold)
		drop = "DROP TRIGGER IF EXISTS reject_nth_owned_asset_link ON audit_events; DROP FUNCTION IF EXISTS reject_nth_owned_asset_link()"
	} else {
		query = fmt.Sprintf("CREATE TRIGGER reject_nth_owned_asset_link BEFORE INSERT ON audit_events WHEN NEW.scope='%s' AND json_extract(NEW.event,'$.action')='asset.linked' AND (SELECT count(*) FROM audit_events WHERE scope=NEW.scope AND json_extract(event,'$.action')='asset.linked')=%d BEGIN SELECT RAISE(ABORT,'synthetic nth asset link failure'); END", s.Prefix(), threshold)
		drop = "DROP TRIGGER IF EXISTS reject_nth_owned_asset_link"
	}
	if _, err := c.writer.ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	var removed sync.Once
	remove := func() {
		removed.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := c.db.ExecContext(ctx, drop); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(remove)
	return remove
}

func checkAuditAssetLinksRollback(t *testing.T, c *Catalog, reader *AuditReader, s asset.Scope, n int, postgres bool) {
	t.Helper()
	f := newAuditLinksHTTP(t, c, reader, s)
	session := "ses_rollback_private"
	attempt := f.begin(t, "", session)
	finish, _ := f.receipt(t, "", session, attempt, "base")
	var record task.Record
	f.json(t, f.worker, "POST", f.route("")+"/finish", finish, &record)
	var oldCP task.Checkpoint
	f.json(t, f.submit, "GET", f.route("")+"/checkpoint", nil, &oldCP)
	turn := uuid.NewString()
	attempt = f.begin(t, turn, session)
	finish, refs := f.receipt(t, turn, session, attempt, "turn")
	before := auditEvents(t, reader, s)
	oldTask, err := c.readStoredTask(t.Context(), taskAddress{s, turn})
	if err != nil {
		t.Fatal(err)
	}
	remove := rejectNthAssetLink(t, c, s, len(assetLinkEvents(before)), n, postgres)
	if err := f.worker.JSON(t.Context(), "POST", f.route(turn)+"/finish", finish, &record); err == nil {
		t.Fatal("Finish survived an actual nth-link insert failure")
	}
	after, err := c.readStoredTask(t.Context(), taskAddress{s, turn})
	if err != nil || !reflect.DeepEqual(oldTask, after) {
		t.Fatal("failed link committed task/checkpoint state", err)
	}
	if !reflect.DeepEqual(before, auditEvents(t, reader, s)) {
		t.Fatal("partial success/link audit survived rollback")
	}
	var cp task.Checkpoint
	f.json(t, f.submit, "GET", f.route("")+"/checkpoint", nil, &cp)
	if !reflect.DeepEqual(cp, oldCP) {
		t.Fatal("failed link advanced the old checkpoint")
	}
	remove()
	f.json(t, f.worker, "POST", f.route(turn)+"/finish", finish, &record)
	requireLinks(t, auditEvents(t, reader, s), taskAddress{s, turn}, attempt, session, f.workerToken, refs)
	committed := auditEvents(t, reader, s)
	f.json(t, f.worker, "POST", f.route(turn)+"/finish", finish, &record)
	if !reflect.DeepEqual(committed, auditEvents(t, reader, s)) {
		t.Fatal("successful Finish replay duplicated links after rollback/retry")
	}
}

func TestAuditAssetLinksNthFailureRollsBackTaskLinksAndCheckpoint(t *testing.T) {
	for _, n := range []int{1, 2, 4} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			f := newAuditFixture(t)
			checkAuditAssetLinksRollback(t, f.c, f.r, scope(fmt.Sprint("audit-link-rollback-", n)), n, false)
		})
	}
}

func TestAuditAssetLinksRequireValidatedFinishAndStrictFields(t *testing.T) {
	f := newAuditFixture(t)
	s := scope("audit-link-no-bypass")
	_, err := f.c.changeTask(t.Context(), taskAddress{Scope: s}, true, func(slot *taskSlot) error {
		slot.Record = task.Record{ID: s.Runtime, State: task.Succeeded}
		return nil
	})
	if !errors.Is(err, ErrAudit) {
		t.Fatal("unvalidated success bypassed authoritative linking", err)
	}
	if _, err = f.c.readStoredTask(t.Context(), taskAddress{Scope: s}); !errors.Is(err, ErrMissing) {
		t.Fatal("unvalidated success persisted")
	}
	if len(auditEvents(t, f.r, s)) != 0 {
		t.Fatal("unvalidated success persisted audit")
	}
	valid := AuditEvent{Schema: AuditSchema, Cursor: 1, TimeUnixNano: 1, Scope: s, TaskID: s.Runtime, Actor: AuditActor{Source: "internal", Role: "system"}, Component: "task-store", Action: "asset.linked", AttemptSHA256: strings.Repeat("a", 64), SessionIDSHA256: strings.Repeat("b", 64), AssetKind: "session", AssetIDSHA256: strings.Repeat("c", 64), Digest: strings.Repeat("d", 64), Version: 1, State: task.Succeeded}
	if !valid.valid() {
		t.Fatal("valid asset link schema rejected")
	}
	for _, mutate := range []func(*AuditEvent){func(e *AuditEvent) { e.AssetKind = "skill" }, func(e *AuditEvent) { e.AttemptSHA256 = "" }, func(e *AuditEvent) { e.SessionIDSHA256 = "" }, func(e *AuditEvent) { e.State = task.Failed }, func(e *AuditEvent) { e.Component = "revision-store" }, func(e *AuditEvent) { e.Version = 0 }, func(e *AuditEvent) { e.Action = "revision.committed" }, func(e *AuditEvent) { e.Action = "task.succeeded" }} {
		changed := valid
		mutate(&changed)
		if changed.valid() {
			t.Fatal("invalid link or session hash on another action accepted")
		}
	}
}
