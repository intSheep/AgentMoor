package platform

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/nativeguest"
	"agentmoor/internal/task"
	"github.com/google/uuid"
)

func handoffProfile(s asset.Scope) SandboxExecutionProfile {
	return SandboxExecutionProfile{Fingerprint: sandboxTestProfile, AdapterID: s.Adapter,
		AdapterVersion: "fixture.1", RuntimeVersion: "synthetic handoff fixture 1"}
}

func handoffTree(t *testing.T, name, value string) []byte {
	t.Helper()
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(value)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, value); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func handoffPublish(t *testing.T, c *Catalog, s asset.Scope, key asset.Key, raw []byte, format, media, session string, base int64) task.Ref {
	t.Helper()
	ch := change(string(raw), base)
	ch.Key, ch.Format, ch.MediaType, ch.SessionID = key, format, media, session
	ticket := upload(t, c, s, ch, string(raw))
	revision, err := c.Commit(t.Context(), s, ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	return task.Ref{Key: key, Version: revision, SHA256: ch.SHA256, ContentPath: key.ContentPath(revision)}
}

func handoffClaim(t *testing.T, c *Catalog, s asset.Scope, pins []task.SkillPin) SandboxAddress {
	t.Helper()
	a := SandboxAddress{Scope: s, Attempt: uuid.NewString()}
	if _, err := c.submitTask(t.Context(), taskAddress{Scope: s}, task.Spec{Instruction: "synthetic handoff only", Input: "original input", Output: "report.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.claimTask(t.Context(), taskAddress{Scope: s}, task.Claim{Attempt: a.Attempt, Skills: pins, ExecutionPolicy: task.ExecutionUntrustedIsolated}); err != nil {
		t.Fatal(err)
	}
	return a
}

// Explicit metadata provider fixture: no Kubernetes Pod or native CLI runs.
// Success must nevertheless have the same sealed, terminal/deleted identity
// that the server now requires from every isolated execution.
func handoffReadyForFinish(t *testing.T, c *Catalog, a SandboxAddress, p SandboxExecutionProfile, existing *SandboxCreatePermit) SandboxRun {
	t.Helper()
	var permit SandboxCreatePermit
	if existing != nil {
		permit = *existing
	} else {
		r, _, err := c.PrepareSandboxInput(t.Context(), a, p)
		if err != nil {
			t.Fatal(err)
		}
		permit, err = c.FenceSandboxCreate(t.Context(), a, r.Deadline)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.SealSandboxPodTemplate(t.Context(), a, "handoff-fixture", sandboxTestProfile); err != nil {
		t.Fatal(err)
	}
	uid := uuid.NewString()
	if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
		t.Fatal(err)
	}
	const body = "sealed synthetic handoff result; trusted worker validates result bytes separately"
	if _, err := c.SealSandboxOutput(t.Context(), permit.Capabilities.Output, sandboxTestBlob(body), strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordSandboxTerminal(t.Context(), a, uid, 0); err != nil {
		t.Fatal(err)
	}
	r, err := c.ConfirmSandboxDeleted(t.Context(), a, uid)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func handoffFinishContext(ctx context.Context, s asset.Scope) context.Context {
	return sandboxFinishContext(ctx, func(got asset.Scope) (SandboxExecutionProfile, bool) { return handoffProfile(s), got == s })
}

func checkSandboxPreparedInput(t *testing.T, c *Catalog, s asset.Scope) {
	t.Helper()
	// This protocol fixture never invokes or impersonates a vendor CLI.
	profile := handoffProfile(s)
	skillV1 := handoffTree(t, "SKILL.md", "---\nname: rules\n---\noriginal synthetic package\n")
	skillKey := asset.Key{Kind: "skill", ID: "rules"}
	pin := task.SkillPin{Name: "rules", Source: handoffPublish(t, c, s, skillKey, skillV1, "skill-tar.v1", "application/x-tar", "", 0)}
	a := handoffClaim(t, c, s, []task.SkillPin{pin})
	run, request, err := c.PrepareSandboxInput(t.Context(), a, profile)
	if err != nil || run.State != SandboxPrepared || !run.Input.Sealed || request.Spec.Input != "original input" || request.ParentSessionID != "" {
		t.Fatal("initial input not sealed from real claim", run.State, err)
	}
	if len(request.Skills) != 1 || request.Skills[0].Blob.SHA256 != pin.Source.SHA256 {
		t.Fatal("selected Skill bytes were not bound")
	}
	// Changing the source head cannot upgrade this already selected v1.
	handoffPublish(t, c, s, skillKey, handoffTree(t, "SKILL.md", "---\nname: rules\n---\nnew source v2\n"), "skill-tar.v1", "application/x-tar", "", 1)
	second, again, err := c.PrepareSandboxInput(t.Context(), a, profile)
	if err != nil || second.Input != run.Input || !reflect.DeepEqual(again, request) {
		t.Fatal("retry changed frozen input", err)
	}
	permit, err := c.FenceSandboxCreate(t.Context(), a, run.Deadline)
	if err != nil || !permit.CreateAllowed {
		t.Fatal("cannot read prepared fixture", err)
	}
	_, reader, err := c.ReadSandboxInput(t.Context(), permit.Capabilities.Input)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || sandboxTestBlob(string(raw)).SHA256 != run.Input.SHA256 {
		t.Fatal("prepared outer envelope differs", err)
	}
	decoded, paths, err := nativeguest.DecodeRequest(t.Context(), bytes.NewReader(raw), filepath.Join(t.TempDir(), "decoded"), nativeguest.DefaultLimits())
	if err != nil || !reflect.DeepEqual(decoded, request) {
		t.Fatal("guest cannot validate trusted package", err)
	}
	packageBytes, err := os.ReadFile(paths.Skills[0])
	if err != nil || !bytes.Equal(packageBytes, skillV1) {
		t.Fatal("original Skill package changed", err)
	}
	metadata, _ := json.Marshal(decoded)
	for _, private := range []string{a.Attempt, c.sandboxTemp, permit.Capabilities.Input, permit.Capabilities.Output, permit.Capabilities.Model, `"scope":`, `"attempt":`} {
		if bytes.Contains(metadata, []byte(private)) {
			t.Fatal("trusted authority or path leaked into native request")
		}
	}

	// Complete a synthetic base checkpoint, then prepare a real frozen turn.
	session := uuid.NewString()
	workspaceBytes := handoffTree(t, "saved.txt", "original saved workspace")
	sessionBytes := handoffTree(t, "native.session", "original synthetic session bytes")
	workspaceRef := handoffPublish(t, c, s, asset.Key{Kind: "workspace", ID: "root"}, workspaceBytes, "workspace-tar.v1", "application/x-tar", "", 0)
	sessionRef := handoffPublish(t, c, s, asset.Key{Kind: "session", ID: session}, sessionBytes, "fixture-session.v1", "application/x-tar", "", 0)
	handoffPublish(t, c, s, asset.Key{Kind: "artifact", ID: "out/report.txt"}, []byte("synthetic report"), "file.v1", "text/plain", session, 0)
	handoffReadyForFinish(t, c, a, profile, &permit)
	if _, err = c.finishTask(handoffFinishContext(t.Context(), s), taskAddress{Scope: s}, task.Finish{Attempt: a.Attempt, SessionID: session}); err != nil {
		t.Fatal(err)
	}
	turn := uuid.NewString()
	if _, err = c.submitTurn(t.Context(), taskAddress{s, turn}, task.Question{SessionID: session, Message: "continue from the saved workspace"}); err != nil {
		t.Fatal(err)
	}
	a = SandboxAddress{Scope: s, TurnID: turn, Attempt: uuid.NewString()}
	if _, err = c.claimTask(t.Context(), taskAddress{s, turn}, task.Claim{Attempt: a.Attempt, Skills: []task.SkillPin{pin}, ExecutionPolicy: task.ExecutionUntrustedIsolated}); err != nil {
		t.Fatal(err)
	}
	// Newer asset heads are unrelated to the frozen parent checkpoint.
	handoffPublish(t, c, s, workspaceRef.Key, handoffTree(t, "saved.txt", "new workspace head"), "workspace-tar.v1", "application/x-tar", "", 1)
	handoffPublish(t, c, s, sessionRef.Key, handoffTree(t, "native.session", "new session head"), "fixture-session.v1", "application/x-tar", "", 1)
	run, request, err = c.PrepareSandboxInput(t.Context(), a, profile)
	if err != nil || request.ParentSessionID != session || request.Workspace.SHA256 != workspaceRef.SHA256 || request.Session.SHA256 != sessionRef.SHA256 || request.Skills[0].Blob.SHA256 != pin.Source.SHA256 {
		t.Fatal("continuation upgraded frozen parent or pins", err)
	}
	permit, err = c.FenceSandboxCreate(t.Context(), a, run.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	_, reader, err = c.ReadSandboxInput(t.Context(), permit.Capabilities.Input)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	_, paths, err = nativeguest.DecodeRequest(t.Context(), reader, filepath.Join(t.TempDir(), "turn"), nativeguest.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string][]byte{paths.Workspace: workspaceBytes, paths.Session: sessionBytes, paths.Skills[0]: skillV1} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatal("raw parent bytes changed", err)
		}
	}
}

func TestSandboxPreparedInputUsesFrozenCatalogOnly(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := scope("prepared-input")
	s.Adapter = "handoff-fixture"
	checkSandboxPreparedInput(t, c, s)
}

func TestSandboxPreparedInputRejectsWrongAuthorityAndCorruptSource(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{dir})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := scope("bad-input")
	key := asset.Key{Kind: "skill", ID: "rules"}
	raw := handoffTree(t, "SKILL.md", "---\nname: rules\n---\noriginal")
	pin := task.SkillPin{Name: "rules", Source: handoffPublish(t, c, s, key, raw, "skill-tar.v1", "application/x-tar", "", 0)}
	a := handoffClaim(t, c, s, []task.SkillPin{pin})
	wrong := a
	wrong.Attempt = uuid.NewString()
	if _, _, err = c.PrepareSandboxInput(t.Context(), wrong, handoffProfile(s)); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong private claim accepted", err)
	}
	wrong = a
	wrong.Scope.Tenant = "other-owner"
	if _, _, err = c.PrepareSandboxInput(t.Context(), wrong, handoffProfile(wrong.Scope)); !errors.Is(err, ErrMissing) {
		t.Fatal("cross-scope ref read", err)
	}
	if err = os.WriteFile(filepath.Join(dir, objectKey(s, pin.Source.SHA256)), []byte("corrupt backing object"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.PrepareSandboxInput(t.Context(), a, handoffProfile(s)); err == nil {
		t.Fatal("bad backing bytes became a prepared input")
	}
	if _, err = c.loadSandbox(t.Context(), a); !errors.Is(err, ErrMissing) {
		t.Fatal("failed packing created an intent", err)
	}
	entries, err := os.ReadDir(c.sandboxTemp)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed preparation left private spools", err)
	}
	legacy := sandboxTestClaim(t, c, scope("trusted-claim"))
	if _, _, err = c.PrepareSandboxInput(t.Context(), legacy, handoffProfile(legacy.Scope)); !errors.Is(err, ErrConflict) {
		t.Fatal("local claim could also start isolated execution", err)
	}
}

func TestLegacySandboxFingerprintRemainsReadableAndBindsNewPolicy(t *testing.T) {
	a := SandboxAddress{Scope: scope("old-fingerprint"), Attempt: uuid.NewString()}
	r := task.Record{Spec: task.Spec{Instruction: "legacy", Output: "report.txt"}, Deadline: time.Unix(123, 456).UTC()}
	old, _ := json.Marshal(struct {
		Address  SandboxAddress
		Spec     task.Spec
		Parent   *task.Checkpoint
		Skills   []task.SkillPin
		Deadline time.Time
	}{a, r.Spec, r.Parent, r.Skills, r.Deadline})
	h := sha256.Sum256(old)
	got, err := sandboxRecordFingerprint(a, r)
	if err != nil || got != hex.EncodeToString(h[:]) {
		t.Fatal("legacy persistent fence fingerprint changed", err)
	}
	r.ExecutionPolicy = task.ExecutionUntrustedIsolated
	isolated, _ := sandboxRecordFingerprint(a, r)
	if isolated == got || strings.TrimSpace(isolated) == "" {
		t.Fatal("new execution policy not bound by fingerprint")
	}
}

func TestSandboxPreparationSpoolIsCollectedOnCatalogRestart(t *testing.T) {
	dir, objects := t.TempDir(), DiskObjects{t.TempDir()}
	c, err := OpenCatalog(t.Context(), dir, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	a := handoffClaim(t, c, scope("prepare-restart"), nil)
	run, _, err := c.PrepareSandboxInput(t.Context(), a, handoffProfile(a.Scope))
	if err != nil {
		t.Fatal(err)
	}
	permit, err := c.FenceSandboxCreate(t.Context(), a, run.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(c.sandboxTemp, ".upload-prepare-stopped-process")
	marker := filepath.Join(c.sandboxTemp, "unrelated-marker")
	for _, path := range []string{orphan, marker} {
		if err = os.WriteFile(path, []byte("owned partial spool fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = OpenCatalog(t.Context(), dir, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restart retained a never-registered preparation spool", err)
	}
	if _, err = os.Stat(marker); err != nil {
		t.Fatal("restart removed an unrelated private file", err)
	}
	_, stream, err := c.ReadSandboxInput(t.Context(), permit.Capabilities.Input)
	if err != nil {
		t.Fatal("restart removed live sealed bytes", err)
	}
	defer stream.Close()
	if _, err = io.Copy(io.Discard, stream); err != nil {
		t.Fatal(err)
	}
}
