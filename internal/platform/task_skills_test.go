package platform

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

func TestTaskSkillPinsFreezeNativeInputsAcrossRestartsAndFollowups(t *testing.T) {
	dir := t.TempDir()
	objects := DiskObjects{Dir: t.TempDir()}
	c, err := OpenCatalog(t.Context(), dir, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.Close() }()
	s := scope("pinned-skills")
	base := taskAddress{Scope: s}
	publish := func(where asset.Scope, key asset.Key, content, session string, previous int64) task.Ref {
		t.Helper()
		ch := change(content, previous)
		ch.Key, ch.SessionID = key, session
		if key.Kind == "skill" {
			ch.Format, ch.MediaType = "skill-tar.v1", "application/x-tar"
		}
		ticket := upload(t, c, where, ch, content)
		version, err := c.Commit(t.Context(), where, ticket.ID)
		if err != nil {
			t.Fatal(err)
		}
		return task.Ref{Key: key, Version: version, SHA256: ch.SHA256, ContentPath: key.ContentPath(version)}
	}
	source := asset.Key{Kind: "skill", ID: "catalog/skills/report/SKILL.md"}
	v1 := publish(s, source, "original skill package", "", 0)
	v2 := publish(s, source, "updated skill package", "", 1)
	pin := task.SkillPin{Source: v1, Name: "report-rules"}
	pins := []task.SkillPin{pin}
	spec := task.Spec{Instruction: "report", Output: "report.json"}
	if _, err := c.submitTask(t.Context(), base, spec); err != nil {
		t.Fatal(err)
	}
	claim := task.Claim{Attempt: uuid.NewString(), Skills: pins}
	wrongHash := pin
	wrongHash.Source.SHA256 = strings.Repeat("f", 64)
	missing := pin
	missing.Source.Version = 99
	missing.Source.ContentPath = missing.Source.Key.ContentPath(99)
	foreign := pin
	foreign.Source.ID = "catalog/skills/foreign/SKILL.md"
	foreign.Source.ContentPath = foreign.Source.Key.ContentPath(1)
	other := s
	other.Tenant = "another-tenant"
	foreign.Source = publish(other, foreign.Source.Key, "foreign skill package", "", 0)
	for _, tc := range []struct {
		name string
		pin  task.SkillPin
		want error
	}{
		{"wrong digest", wrongHash, ErrConflict},
		{"missing version", missing, ErrNotReady},
		{"other scope", foreign, ErrNotReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.claimTask(t.Context(), base, task.Claim{Attempt: claim.Attempt, Skills: []task.SkillPin{tc.pin}}); !errors.Is(err, tc.want) {
				t.Fatalf("claim = %v, want %v", err, tc.want)
			}
			if row, err := c.readTask(t.Context(), base); err != nil || row.State != task.Queued || len(row.Skills) != 0 {
				t.Fatal("rejected pin changed task", row, err)
			}
		})
	}
	for range 2 {
		row, err := c.claimTask(t.Context(), base, claim)
		if err != nil || !slices.Equal(row.Skills, pins) || row.Skills[0].Source.Version != 1 {
			t.Fatal("claim did not freeze selected old version", row, err)
		}
	}
	for _, changed := range [][]task.SkillPin{nil, {{Source: v2, Name: pin.Name}}} {
		if _, err := c.claimTask(t.Context(), base, task.Claim{Attempt: claim.Attempt, Skills: changed}); !errors.Is(err, ErrConflict) {
			t.Fatal("same attempt changed pinned input", err)
		}
	}
	session := uuid.NewString()
	for _, key := range []asset.Key{{Kind: "artifact", ID: "out/report.json"}, {Kind: "workspace", ID: "root"}, {Kind: "session", ID: session}} {
		publish(s, key, "first result", session, 0)
	}
	if _, err := c.finishTask(t.Context(), base, task.Finish{Attempt: claim.Attempt, SessionID: session}); err != nil {
		t.Fatal(err)
	}
	cp, err := c.latestCheckpoint(t.Context(), s)
	if err != nil || !slices.Equal(cp.Skills, pins) {
		t.Fatal(cp, err)
	}
	a := taskAddress{s, uuid.NewString()}
	question := task.Question{SessionID: session, Message: "continue using the original rules"}
	row, err := c.submitTurn(t.Context(), a, question)
	if err != nil || !slices.Equal(row.Skills, pins) || !slices.Equal(row.Parent.Skills, pins) {
		t.Fatal(row, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = OpenCatalog(t.Context(), dir, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	row, err = c.submitTurn(t.Context(), a, question)
	if err != nil || !slices.Equal(row.Skills, pins) || !slices.Equal(row.Parent.Skills, pins) {
		t.Fatal("accepted pins did not survive restart", row, err)
	}
	attempt := uuid.NewString()
	for _, changed := range [][]task.SkillPin{nil, {{Source: v2, Name: pin.Name}}} {
		if _, err := c.claimTask(t.Context(), a, task.Claim{Attempt: attempt, Skills: changed}); !errors.Is(err, ErrConflict) {
			t.Fatal("replacement worker changed inherited pins", err)
		}
	}
	row, err = c.claimTask(t.Context(), a, task.Claim{Attempt: attempt, Skills: pins})
	if err != nil || !slices.Equal(row.Skills, pins) {
		t.Fatal(row, err)
	}
	if _, err := c.cancelTask(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := c.finishTask(t.Context(), a, task.Finish{Attempt: attempt, Error: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	next, err := c.submitTurn(t.Context(), taskAddress{s, uuid.NewString()}, question)
	if err != nil || !slices.Equal(next.Skills, pins) || next.Parent.Session != cp.Session {
		t.Fatal("failed turn changed last successful skills", next, err)
	}
}

func TestOnlyExecutionCredentialCanSelectPinnedSkills(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := scope("skill-rights")
	secret := bytes.Repeat([]byte{8}, 32)
	handler := (&API{Catalog: c, Secret: secret, Temp: t.TempDir(), TaskAdapters: map[string]bool{s.Adapter: true}}).Handler()
	request := func(method, path string, grant Grant, body any, want int) {
		t.Helper()
		grant.Scope, grant.Expires = s, time.Now().Add(time.Hour).Unix()
		grant.Read = true
		token, err := Issue(secret, grant, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, w.Code, want, w.Body.String())
		}
	}
	route := "/v1/tasks/" + s.Runtime
	// SaaS cannot select Skills in the immutable task specification.
	request("PUT", route, Grant{Submit: true}, map[string]any{"instruction": "run", "output": "report.json", "skills": []any{}}, 400)
	request("PUT", route, Grant{Submit: true}, task.Spec{Instruction: "run", Output: "report.json"}, 200)
	// A real pin traverses the HTTP contract, including a source ID that is
	// longer than the original tiny claim body limit.
	ch := change("skill package", 0)
	ch.Key = asset.Key{Kind: "skill", ID: "catalog/" + strings.Repeat("s", 1100)}
	ticket := upload(t, c, s, ch, "skill package")
	version, err := c.Commit(t.Context(), s, ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	claim := task.Claim{Attempt: uuid.NewString(), Skills: []task.SkillPin{{Name: "report-rules", Source: task.Ref{Key: ch.Key, Version: version, SHA256: ch.SHA256, ContentPath: ch.Key.ContentPath(version)}}}}
	request("POST", route+"/claim", Grant{Submit: true}, claim, 403)
	request("POST", route+"/claim", Grant{Read: true}, claim, 403)
	request("POST", route+"/claim", Grant{Execute: true}, claim, 200)
	row, err := c.readTask(t.Context(), taskAddress{Scope: s})
	if err != nil || !slices.Equal(row.Skills, claim.Skills) {
		t.Fatal("HTTP claim did not persist the selected version", row, err)
	}
}
