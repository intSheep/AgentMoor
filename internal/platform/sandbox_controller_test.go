package platform

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func sandboxControllerCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestSandboxControllerTemplateSeal(t *testing.T) {
	c := sandboxControllerCatalog(t)
	a := sandboxTestClaim(t, c, scope("controller-template"))
	sandboxTestBegin(t, c, a, "fixed input")
	digest := sandboxTestBlob("fixed operator Pod template").SHA256
	if _, err := c.SealSandboxPodTemplate(t.Context(), a, "sandbox", digest); !errors.Is(err, ErrConflict) {
		t.Fatal("template was sealed before the create fence", err)
	}
	if _, err := c.SealSandboxInput(t.Context(), a, strings.NewReader("fixed input")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SealSandboxPodTemplate(t.Context(), a, "sandbox", digest); !errors.Is(err, ErrConflict) {
		t.Fatal("sealed input alone authorized a template", err)
	}
	r, err := c.SandboxRun(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.FenceSandboxCreate(t.Context(), a, r.Deadline)
	if err != nil || !p.CreateAllowed {
		t.Fatal(p, err)
	}
	for _, namespace := range []string{"", "Upper", "bad.namespace", "../sandbox", "-sandbox", "sandbox-", strings.Repeat("n", 64)} {
		if _, err := c.SealSandboxPodTemplate(t.Context(), a, namespace, digest); !errors.Is(err, errBadRequest) {
			t.Fatal("invalid namespace accepted", namespace, err)
		}
	}
	for _, value := range []string{"", "not-a-digest", strings.Repeat("0", 63), strings.Repeat("A", 64), strings.Repeat("0", 65)} {
		if _, err := c.SealSandboxPodTemplate(t.Context(), a, "sandbox", value); !errors.Is(err, errBadRequest) {
			t.Fatal("invalid template digest accepted", err)
		}
	}
	r, err = c.SealSandboxPodTemplate(t.Context(), a, "sandbox", digest)
	if err != nil || r.PodNamespace != "sandbox" || r.PodTemplateSHA != digest || r.PodUID != "" || !r.CreateStarted {
		t.Fatal("template seal changed execution ownership", r, err)
	}
	retry, err := c.SealSandboxPodTemplate(t.Context(), a, "sandbox", digest)
	if err != nil || retry.PodNamespace != r.PodNamespace || retry.PodTemplateSHA != r.PodTemplateSHA {
		t.Fatal("identical seal was not idempotent", retry, err)
	}
	for _, changed := range [][2]string{{"other-namespace", digest}, {"sandbox", sandboxTestBlob("changed template").SHA256}} {
		if _, err := c.SealSandboxPodTemplate(t.Context(), a, changed[0], changed[1]); !errors.Is(err, ErrConflict) {
			t.Fatal("fixed template identity was replaced", changed, err)
		}
	}
	uid := uuid.NewString()
	if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CancelSandbox(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SealSandboxPodTemplate(t.Context(), a, "sandbox", digest); !errors.Is(err, ErrConflict) {
		t.Fatal("cancelled run resealed its template", err)
	}
	sandboxTestFinish(t, c, a, true)
	if n, err := c.Collect(t.Context(), a.Scope, time.Now().Add(time.Second)); err != nil || n != 1 {
		t.Fatal("GC positive control did not collect the private input", n, err)
	}
	if _, err := c.SealSandboxPodTemplate(t.Context(), a, "sandbox", digest); !errors.Is(err, ErrConflict) {
		t.Fatal("collected run resealed its template", err)
	}
	r, err = c.SandboxRun(t.Context(), a)
	if err != nil || !r.Collected || r.PodUID != uid || r.PodNamespace != "sandbox" || r.PodTemplateSHA != digest {
		t.Fatal("cancel or collection erased the sealed template", r, err)
	}
}

func sandboxControllerCheckCaps(t *testing.T, c *Catalog, a SandboxAddress, caps SandboxCapabilities, active bool) {
	t.Helper()
	if err := c.MatchSandboxCapabilities(t.Context(), a, caps); err != nil {
		t.Fatal("exact historical capabilities did not match", err)
	}
	for purpose, token := range map[SandboxPurpose]string{SandboxInput: caps.Input, SandboxOutput: caps.Output, SandboxModel: caps.Model} {
		_, err := c.AuthenticateSandboxCapability(t.Context(), token, purpose)
		if active && err != nil {
			t.Fatal("live capability positive control denied", purpose, err)
		}
		if !active && !errors.Is(err, ErrSandboxDenied) {
			t.Fatal("historical match restored guest authority", purpose, err)
		}
	}
}

func TestSandboxControllerHistoricalCapabilities(t *testing.T) {
	for _, stopped := range []string{"cancelled", "expired", "collected"} {
		t.Run(stopped, func(t *testing.T) {
			c := sandboxControllerCatalog(t)
			a := sandboxTestClaim(t, c, scope("controller-caps-"+stopped))
			p := sandboxTestFence(t, c, a, "fixed input")
			sandboxControllerCheckCaps(t, c, a, p.Capabilities, true)
			switch stopped {
			case "cancelled":
				if _, err := c.CancelSandbox(t.Context(), a); err != nil {
					t.Fatal(err)
				}
			case "expired":
				// Advance the persisted capability expiry without a timing-dependent
				// sleep. The task and its frozen deadline remain unchanged.
				if _, err := c.writer.ExecContext(t.Context(), "UPDATE sandbox_caps SET expires=$1 WHERE scope=$2 AND run_id=$3", time.Now().Add(-time.Second).UnixNano(), a.Scope.Prefix(), p.Run.ID); err != nil {
					t.Fatal(err)
				}
			case "collected":
				sandboxTestFinish(t, c, a, true)
				if n, err := c.Collect(t.Context(), a.Scope, time.Now().Add(time.Second)); err != nil || n != 1 {
					t.Fatal("GC did not create a tombstone", n, err)
				}
			}
			sandboxControllerCheckCaps(t, c, a, p.Capabilities, false)
		})
	}
}

func TestSandboxControllerCapabilityIdentityCannotBeSubstituted(t *testing.T) {
	c := sandboxControllerCatalog(t)
	a := sandboxTestClaim(t, c, scope("controller-cap-identity"))
	p := sandboxTestFence(t, c, a, "one input")
	b := sandboxTestClaim(t, c, scope("controller-cap-foreign"))
	q := sandboxTestFence(t, c, b, "other input")
	unfenced := sandboxTestClaim(t, c, scope("controller-cap-unfenced"))
	sandboxTestBegin(t, c, unfenced, "unfenced input")
	if err := c.MatchSandboxCapabilities(t.Context(), unfenced, p.Capabilities); !errors.Is(err, ErrConflict) {
		t.Fatal("unfenced run matched capabilities", err)
	}
	for _, changed := range []SandboxCapabilities{
		{},
		{Input: p.Capabilities.Output, Output: p.Capabilities.Input, Model: p.Capabilities.Model},
		{Input: p.Capabilities.Input, Output: p.Capabilities.Model, Model: p.Capabilities.Output},
		{Input: q.Capabilities.Input, Output: p.Capabilities.Output, Model: p.Capabilities.Model},
		q.Capabilities,
		{Input: p.Capabilities.Input + "x", Output: p.Capabilities.Output, Model: p.Capabilities.Model},
	} {
		if err := c.MatchSandboxCapabilities(t.Context(), a, changed); !errors.Is(err, ErrSandboxDenied) {
			t.Fatal("capability purpose, hash or scope was substituted", err)
		}
	}
	foreignAttempt := a
	foreignAttempt.Attempt = uuid.NewString()
	if err := c.MatchSandboxCapabilities(t.Context(), foreignAttempt, p.Capabilities); !errors.Is(err, ErrMissing) {
		t.Fatal("capabilities crossed the attempt identity", err)
	}
	foreignScope := a
	foreignScope.Scope.Tenant = "unrelated-tenant"
	if err := c.MatchSandboxCapabilities(t.Context(), foreignScope, p.Capabilities); !errors.Is(err, ErrMissing) {
		t.Fatal("capabilities crossed scope", err)
	}
	if _, err := c.AuthenticateSandboxCapability(t.Context(), p.Capabilities.Input, SandboxOutput); !errors.Is(err, ErrSandboxDenied) {
		t.Fatal("input token acquired output authority", err)
	}
	if _, err := c.AuthenticateSandboxCapability(t.Context(), p.Capabilities.Model, SandboxPurpose("other")); !errors.Is(err, ErrSandboxDenied) {
		t.Fatal("unknown capability purpose accepted", err)
	}
	sandboxControllerCheckCaps(t, c, a, p.Capabilities, true)
}

func TestSandboxControllerCollectedCleanupIsNotExecution(t *testing.T) {
	c := sandboxControllerCatalog(t)
	a := sandboxTestClaim(t, c, scope("controller-late-cleanup"))
	p := sandboxTestFence(t, c, a, "frozen input")
	digest := sandboxTestBlob("fixed operator Pod template").SHA256
	if _, err := c.SealSandboxPodTemplate(t.Context(), a, "sandbox", digest); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("frozen output"), strings.NewReader("frozen output")); err != nil {
		t.Fatal(err)
	}
	sandboxTestFinish(t, c, a, true)
	if n, err := c.Collect(t.Context(), a.Scope, time.Now().Add(time.Second)); err != nil || n != 2 {
		t.Fatal("GC did not really delete both private objects", n, err)
	}
	before, err := c.SandboxRun(t.Context(), a)
	if err != nil || !before.Collected || before.PodUID != "" || !before.Input.Sealed || !before.Output.Sealed {
		t.Fatal("missing collected unbound Pod positive control", before, err)
	}
	uid, wrong := uuid.NewString(), uuid.NewString()
	if _, err := c.RecordSandboxTerminal(t.Context(), a, uid, 255); !errors.Is(err, ErrConflict) {
		t.Fatal("unbound Pod recorded a terminal status", err)
	}
	if _, err := c.ConfirmSandboxDeleted(t.Context(), a, uid); !errors.Is(err, ErrConflict) {
		t.Fatal("unbound Pod recorded deletion", err)
	}
	for range 2 {
		if _, err := c.BindSandboxPod(t.Context(), a, uid); err != nil {
			t.Fatal("late exact Pod UID could not be recorded", err)
		}
	}
	if _, err := c.BindSandboxPod(t.Context(), a, wrong); !errors.Is(err, ErrConflict) {
		t.Fatal("late cleanup replaced the Pod UID", err)
	}
	if _, err := c.RecordSandboxTerminal(t.Context(), a, wrong, 255); !errors.Is(err, ErrConflict) {
		t.Fatal("foreign UID recorded a terminal status", err)
	}
	for range 2 {
		if _, err := c.RecordSandboxTerminal(t.Context(), a, uid, 255); err != nil {
			t.Fatal("late terminal evidence was denied", err)
		}
	}
	if _, err := c.RecordSandboxTerminal(t.Context(), a, uid, 0); !errors.Is(err, ErrConflict) {
		t.Fatal("terminal evidence changed its exit code", err)
	}
	if _, err := c.ConfirmSandboxDeleted(t.Context(), a, wrong); !errors.Is(err, ErrConflict) {
		t.Fatal("foreign UID confirmed deletion", err)
	}
	for range 2 {
		if _, err := c.ConfirmSandboxDeleted(t.Context(), a, uid); err != nil {
			t.Fatal("late deletion evidence was denied", err)
		}
	}
	if _, err := c.BeginSandbox(t.Context(), a, before.ProfileFingerprint, sandboxTestBlob("frozen input")); err == nil {
		t.Fatal("collected attempt began again")
	}
	if permit, err := c.FenceSandboxCreate(t.Context(), a, before.Deadline); err == nil || permit.CreateAllowed || permit.Capabilities != (SandboxCapabilities{}) {
		t.Fatal("cleanup granted another create or new capabilities", permit.CreateAllowed, err)
	}
	if _, err := c.SealSandboxInput(t.Context(), a, sandboxPanicReader{}); !errors.Is(err, ErrConflict) {
		t.Fatal("collected input seal was revived", err)
	}
	if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("frozen output"), sandboxPanicReader{}); !errors.Is(err, ErrSandboxDenied) {
		t.Fatal("collected output seal was revived", err)
	}
	if _, err := c.SealSandboxPodTemplate(t.Context(), a, "sandbox", digest); !errors.Is(err, ErrConflict) {
		t.Fatal("cleanup permitted a template seal", err)
	}
	if _, err := c.CancelSandbox(t.Context(), a); !errors.Is(err, ErrConflict) {
		t.Fatal("collected run allowed general state mutation", err)
	}
	if _, err := c.FailSandbox(t.Context(), a, "late-failure"); !errors.Is(err, ErrConflict) {
		t.Fatal("collected run allowed general failure mutation", err)
	}
	if _, stream, err := c.ReadSandboxOutput(t.Context(), a); !errors.Is(err, ErrMissing) || stream != nil {
		if stream != nil {
			stream.Close()
		}
		t.Fatal("cleanup exposed a collected result", err)
	}
	sandboxControllerCheckCaps(t, c, a, p.Capabilities, false)
	after, err := c.SandboxRun(t.Context(), a)
	if err != nil || !after.Collected || !after.PodTerminal || !after.PodDeleted || after.ExitCode != 255 || after.PodUID != uid || after.State == SandboxReady {
		t.Fatal("late cleanup did not remain a tombstone", after, err)
	}
	want := before
	want.PodUID, want.PodTerminal, want.PodDeleted, want.ExitCode, want.UpdatedAt = uid, true, true, 255, after.UpdatedAt
	if !reflect.DeepEqual(after, want) {
		t.Fatal("cleanup altered frozen execution identity or assets", after, want)
	}
	var total, revoked int
	if err := c.db.QueryRowContext(t.Context(), "SELECT count(*), sum(revoked) FROM sandbox_caps WHERE scope=$1 AND run_id=$2", a.Scope.Prefix(), before.ID).Scan(&total, &revoked); err != nil || total != 3 || revoked != 3 {
		t.Fatal("cleanup minted or restored a capability", total, revoked, err)
	}
	for purpose, blob := range map[SandboxPurpose]SandboxBlob{SandboxInput: after.Input, SandboxOutput: after.Output} {
		if exists, err := c.objects.Exists(t.Context(), sandboxObjectKey(a.Scope, purpose, blob.SHA256)); err != nil || exists {
			t.Fatal("cleanup recreated collected blob bytes", purpose, exists, err)
		}
	}
}
