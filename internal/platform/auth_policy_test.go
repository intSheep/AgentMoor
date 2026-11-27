package platform

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/task"
	"golang.org/x/sys/unix"
)

func authTestGrant(now time.Time) Grant {
	return Grant{Scope: scope("auth-owner"), Read: true, Write: true, Submit: true, Execute: true, Collect: true, Expires: now.Add(time.Hour).Unix()}
}

func authTestIssue(t *testing.T, policy string, grant Grant, now time.Time) string {
	t.Helper()
	token, err := IssueAuthPolicy(policy, grant, now)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func authTestRequest(t *testing.T, endpoint, method, route, token string, body []byte, want int) []byte {
	t.Helper()
	req, err := http.NewRequest(method, endpoint+route, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s returned %d, want %d", method, route, response.StatusCode, want)
	}
	if want != 200 && want != 204 && response.Header.Get("X-Agentmoor-Revision") != "" {
		t.Fatal("denied request exposed revision metadata")
	}
	return data
}

func authTestTokenFile(t *testing.T, path, token string) {
	t.Helper()
	temp := path + ".new"
	if err := os.WriteFile(temp, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
}

func TestAuthPolicyLiveRotationRevocationAndClientRenewal(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "policy.json")
	legacyKey := bytes.Repeat([]byte{51}, 32)
	if _, err := InitAuthPolicy(t.Context(), path, "legacy", legacyKey); err != nil {
		t.Fatal(err)
	}
	grant := authTestGrant(now)
	old, err := Issue(legacyKey, grant, now)
	if err != nil {
		t.Fatal(err)
	}
	first, second := authTestIssue(t, path, grant, now), authTestIssue(t, path, grant, now)
	legacyGrant := grant
	legacyGrant.Expires++
	legacySurvivor, err := Issue(legacyKey, legacyGrant, now)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("equal grants must have independent credential identities")
	}
	server := httptest.NewServer((&API{AuthPolicy: path, Secret: legacyKey}).Handler())
	defer server.Close()
	for _, token := range []string{old, legacySurvivor, first, second} {
		authTestRequest(t, server.URL, "GET", "/v1/context", token, nil, 200)
	}
	if _, err = RotateAuthPolicy(t.Context(), path, "current", now); err != nil {
		t.Fatal(err)
	}
	current, survivor := authTestIssue(t, path, grant, now), authTestIssue(t, path, grant, now)
	for _, token := range []string{old, first, second, current, survivor} {
		authTestRequest(t, server.URL, "GET", "/v1/context", token, nil, 200)
	}

	for i := 0; i < 2; i++ {
		if err = RevokeAuthPolicyCredential(t.Context(), path, first, now); err != nil {
			t.Fatal(err)
		}
	}
	authTestRequest(t, server.URL, "GET", "/v1/context", first, nil, 401)
	for _, token := range []string{old, second, current, survivor} {
		authTestRequest(t, server.URL, "GET", "/v1/context", token, nil, 200)
	}
	if err = RevokeAuthPolicyCredential(t.Context(), path, old, now); err != nil {
		t.Fatal(err)
	}
	authTestRequest(t, server.URL, "GET", "/v1/context", old, nil, 401)
	authTestRequest(t, server.URL, "GET", "/v1/context", legacySurvivor, nil, 200)
	authTestRequest(t, server.URL, "GET", "/v1/context", second, nil, 200)

	tokenFile := filepath.Join(t.TempDir(), "worker.token")
	authTestTokenFile(t, tokenFile, second)
	client, err := NewFileClient(server.URL, tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := client.Context(t.Context()); err != nil || got != grant {
		t.Fatal("old file client positive control", err)
	}
	if err = RetireAuthPolicyKey(t.Context(), path, "legacy", now); err != nil {
		t.Fatal(err)
	}
	authTestRequest(t, server.URL, "GET", "/v1/context", legacySurvivor, nil, 401)
	if _, err := client.Context(t.Context()); err == nil {
		t.Fatal("retired verification key remained accepted")
	}
	authTestTokenFile(t, tokenFile, current)
	if got, err := client.Context(t.Context()); err != nil || got != grant {
		t.Fatal("same file client did not pick up atomic renewal", err)
	}

	before, _ := os.ReadFile(path)
	if err = RetireAuthPolicyKey(t.Context(), path, "current", now); !errors.Is(err, ErrAuthPolicyConflict) {
		t.Fatal("active key retirement must fail", err)
	}
	if _, err = RotateAuthPolicy(t.Context(), path, "current", now); !errors.Is(err, ErrAuthPolicyConflict) {
		t.Fatal("duplicate key ID must fail", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("rejected management operation changed policy")
	}
	if err = RevokeAuthPolicyCredential(t.Context(), path, current, now); err != nil {
		t.Fatal(err)
	}
	server.Close()
	restarted := httptest.NewServer((&API{AuthPolicy: path, Secret: legacyKey}).Handler())
	defer restarted.Close()
	authTestRequest(t, restarted.URL, "GET", "/v1/context", current, nil, 401)
	authTestRequest(t, restarted.URL, "GET", "/v1/context", survivor, nil, 200)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("policy must be private", err)
	}
}

func signAuthTestPayload(t *testing.T, raw []byte, key []byte) string {
	t.Helper()
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestAuthPolicyStrictTokensAndExplicitLegacy(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "policy.json")
	key := bytes.Repeat([]byte{52}, 32)
	if _, err := InitAuthPolicy(t.Context(), path, "old", key); err != nil {
		t.Fatal(err)
	}
	grant := authTestGrant(now)
	token := authTestIssue(t, path, grant, now)
	raw, _ := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"scope", "write", "expires", "kid", "cid", "v"} {
		t.Run("tampered-"+field, func(t *testing.T) {
			copy := make(map[string]any)
			for k, v := range fields {
				copy[k] = v
			}
			copy[field] = "forged"
			body, _ := json.Marshal(copy)
			forged := base64.RawURLEncoding.EncodeToString(body) + "." + strings.Split(token, ".")[1]
			if _, err := AuthenticateAuthPolicy(path, forged, now); !errors.Is(err, errDenied) {
				t.Fatal("tampered signed field accepted", err)
			}
		})
	}
	for name, patch := range map[string]map[string]any{
		"unknown-kid":     {"kid": "missing"},
		"unknown-version": {"v": 3},
		"null-selectors":  {"v": nil, "kid": nil, "cid": nil},
		"missing-version": {"v": nil},
		"empty-id":        {"cid": ""},
		"extra-field":     {"unexpected": "value"},
	} {
		t.Run(name, func(t *testing.T) {
			copy := make(map[string]any)
			for k, v := range fields {
				copy[k] = v
			}
			for k, v := range patch {
				copy[k] = v
			}
			body, _ := json.Marshal(copy)
			// Even a correct MAC under the legacy key must not make an
			// unknown/partial modern envelope fall back to legacy parsing.
			if _, err := AuthenticateAuthPolicy(path, signAuthTestPayload(t, body, key), now); !errors.Is(err, errDenied) {
				t.Fatal("invalid envelope accepted", err)
			}
		})
	}
	for _, selectors := range []map[string]any{
		{"V": 2, "Kid": "missing", "Cid": strings.Repeat("a", 32)},
		{"V": nil, "Kid": nil, "Cid": nil},
		{"v": 2, "V": 2, "kid": "old", "cid": strings.Repeat("a", 32)},
		{"v": 2, "kid": "old", "Kid": "missing", "cid": strings.Repeat("a", 32)},
		{"v": 2, "kid": "old", "cid": strings.Repeat("a", 32), "ſcope": map[string]any{"tenant": "foreign", "project": "project", "runtime": "runtime", "adapter": "codex"}},
	} {
		legacyRaw, _ := json.Marshal(grant)
		var candidate map[string]any
		json.Unmarshal(legacyRaw, &candidate)
		for name, value := range selectors {
			candidate[name] = value
		}
		payload, _ := json.Marshal(candidate)
		if _, err := AuthenticateAuthPolicy(path, signAuthTestPayload(t, payload, key), now); !errors.Is(err, errDenied) {
			t.Fatal("case-aliased modern selector fell back to legacy verification", err)
		}
	}
	if err := RevokeAuthPolicyCredential(t.Context(), path, token, now); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, parts[1][len(parts[1])-1])
	variant := parts[0] + "." + parts[1][:len(parts[1])-1] + string(alphabet[last|1])
	originalMAC, _ := base64.RawURLEncoding.DecodeString(parts[1])
	variantMAC, _ := base64.RawURLEncoding.DecodeString(strings.Split(variant, ".")[1])
	if variant == token || !bytes.Equal(originalMAC, variantMAC) {
		t.Fatal("test needs same MAC bytes with a noncanonical encoding")
	}
	if _, err := AuthenticateAuthPolicy(path, variant, now); !errors.Is(err, errDenied) {
		t.Fatal("noncanonical signature bypassed exact-token revocation", err)
	}

	legacy, _ := Issue(key, grant, now)
	fresh := filepath.Join(t.TempDir(), "fresh.json")
	if _, err := InitAuthPolicy(t.Context(), fresh, "fresh", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := AuthenticateAuthPolicy(fresh, legacy, now); !errors.Is(err, errDenied) {
		t.Fatal("fresh policy implicitly accepted legacy tokens", err)
	}
	freshPolicy, err := LoadAuthPolicy(fresh)
	if err != nil {
		t.Fatal(err)
	}
	sameKeyLegacy, err := Issue(freshPolicy.Keys[0].Key, grant, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AuthenticateAuthPolicy(fresh, sameKeyLegacy, now); !errors.Is(err, errDenied) {
		t.Fatal("unmarked verification key implicitly accepted legacy format", err)
	}
	// Old mode remains unchanged: it can still verify a new-format token
	// signed with its existing imported key during a controlled migration.
	if got, err := authenticate(key, token, now); err != nil || got != grant {
		t.Fatal("legacy verifier compatibility changed", err)
	}
	expired := grant
	expired.Expires = now.Add(-time.Hour).Unix()
	expiredToken := authTestIssue(t, path, expired, now.Add(-2*time.Hour))
	if _, err := AuthenticateAuthPolicy(path, expiredToken, now); !errors.Is(err, errDenied) {
		t.Fatal("expired token accepted", err)
	}
	before, _ := os.ReadFile(path)
	if err := RevokeAuthPolicyCredential(t.Context(), path, expiredToken, now); !errors.Is(err, errDenied) {
		t.Fatal("expired credential accepted for revocation", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("rejected expired revocation changed policy")
	}
	tooLong := grant
	tooLong.Expires = now.Add(25 * time.Hour).Unix()
	if _, err := IssueAuthPolicy(path, tooLong, now); !errors.Is(err, errDenied) {
		t.Fatal("more than 24 hours issued", err)
	}
}

func TestAuthPolicyRevocationAndUnavailableApplyToEveryRoute(t *testing.T) {
	f := newAuthorizationFixture(t, task.Succeeded, task.Queued)
	now := time.Now()
	path := filepath.Join(t.TempDir(), "policy.json")
	if _, err := InitAuthPolicy(t.Context(), path, "initial", f.secret); err != nil {
		t.Fatal(err)
	}
	grant := authTestGrant(now)
	grant.Scope = f.scope
	token := authTestIssue(t, path, grant, now)
	server := httptest.NewServer((&API{Catalog: f.catalog, Secret: f.secret, AuthPolicy: path, Temp: t.TempDir(), TaskAdapters: map[string]bool{f.scope.Adapter: true}, TurnEnabled: func(_ asset.Scope) bool { return true }}).Handler())
	defer server.Close()
	authTestRequest(t, server.URL, "GET", "/v1/context", token, nil, 200)
	authTestRequest(t, server.URL, "GET", f.artifact.Key.ContentPath(1), token, nil, 200)
	before := f.ownerState()
	requests := []struct {
		method, path string
		body         []byte
	}{
		{"GET", "/v1/context", nil}, {"GET", "/v1/assets", nil}, {"GET", "/v1/heads", nil}, {"GET", "/v1/status", nil},
		{"GET", f.artifact.Key.ContentPath(1), nil}, {"POST", "/v1/changes", authorizationJSON(t, f.rawChange)},
		{"PUT", f.rawRoute() + "/content", []byte("pending upload")}, {"POST", f.rawRoute() + "/commit", nil},
		{"POST", f.partRoute() + "/parts", authorizationJSON(t, f.manifest)}, {"PUT", f.partRoute() + "/parts/" + f.part.SHA256, f.partBody},
		{"POST", "/v1/gc", nil}, {"PUT", f.route(), authorizationJSON(t, f.spec)}, {"GET", f.route(), nil},
		{"GET", f.route() + "/checkpoint", nil}, {"POST", f.route() + "/cancel", nil},
		{"POST", f.route() + "/claim", authorizationJSON(t, task.Claim{Attempt: f.attempt})},
		{"POST", f.route() + "/finish", authorizationJSON(t, task.Finish{Attempt: f.attempt, SessionID: f.session})},
		{"PUT", f.turnRoute(), authorizationJSON(t, f.question())}, {"GET", f.turnRoute(), nil}, {"POST", f.turnRoute() + "/cancel", nil},
		{"POST", f.turnRoute() + "/claim", authorizationJSON(t, task.Claim{Attempt: f.attempt})},
		{"POST", f.turnRoute() + "/finish", authorizationJSON(t, task.Finish{Attempt: f.attempt, SessionID: f.session})},
	}
	if len(requests) != 22 {
		t.Fatal("update the explicit current route list")
	}
	if err := RevokeAuthPolicyCredential(t.Context(), path, token, now); err != nil {
		t.Fatal(err)
	}
	for _, request := range requests {
		authTestRequest(t, server.URL, request.method, request.path, token, request.body, 401)
		f.unchanged(before)
	}
	renewed := authTestIssue(t, path, grant, now)
	authTestRequest(t, server.URL, "GET", "/v1/context", renewed, nil, 200)
	good, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(`{"version":1,"secret":"sensitive-invalid-value"}`), 0600); err != nil {
		t.Fatal(err)
	}
	legacy, _ := Issue(f.secret, grant, now)
	for _, request := range requests {
		body := authTestRequest(t, server.URL, request.method, request.path, legacy, request.body, 503)
		if string(body) != "authentication unavailable\n" {
			t.Fatal("policy failure leaked configuration")
		}
		f.unchanged(before)
	}
	if err := os.WriteFile(path, good, 0600); err != nil {
		t.Fatal(err)
	}
	authTestRequest(t, server.URL, "GET", "/v1/context", renewed, nil, 200)
}

func TestAuthPolicyValidationAndBounds(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "policy.json")
	if _, err := InitAuthPolicy(t.Context(), path, "initial", nil); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(path)
	policy, _ := LoadAuthPolicy(path)
	for name, mutation := range map[string]func(*AuthPolicy){
		"version":            func(p *AuthPolicy) { p.Version = 2 },
		"missing-active":     func(p *AuthPolicy) { p.Active = "missing" },
		"short-key":          func(p *AuthPolicy) { p.Keys[0].Key = []byte("short") },
		"invalid-id":         func(p *AuthPolicy) { p.Keys[0].ID = "../escape" },
		"duplicate-id":       func(p *AuthPolicy) { k := p.Keys[0]; k.Key = bytes.Repeat([]byte{9}, 32); p.Keys = append(p.Keys, k) },
		"duplicate-material": func(p *AuthPolicy) { k := p.Keys[0]; k.ID = "other"; p.Keys = append(p.Keys, k) },
		"multiple-legacy": func(p *AuthPolicy) {
			p.Keys[0].Legacy = true
			p.Keys = append(p.Keys, AuthPolicyKey{ID: "other", Key: bytes.Repeat([]byte{9}, 32), Legacy: true})
		},
		"invalid-revocation": func(p *AuthPolicy) { p.Revoked = []RevokedCredential{{SHA256: "not-a-digest", Expires: now.Unix()}} },
		"duplicate-revocation": func(p *AuthPolicy) {
			r := RevokedCredential{SHA256: strings.Repeat("a", 64), Expires: now.Unix()}
			p.Revoked = []RevokedCredential{r, r}
		},
		"zero-expiry": func(p *AuthPolicy) { p.Revoked = []RevokedCredential{{SHA256: strings.Repeat("a", 64)}} },
		"too-many-keys": func(p *AuthPolicy) {
			for i := 0; i < MaxAuthPolicyKeys; i++ {
				p.Keys = append(p.Keys, AuthPolicyKey{ID: fmt.Sprintf("key-%d", i), Key: bytes.Repeat([]byte{byte(i + 1)}, 32)})
			}
		},
		"too-many-revocations": func(p *AuthPolicy) {
			for i := 0; i <= MaxRevokedCredentials; i++ {
				p.Revoked = append(p.Revoked, RevokedCredential{SHA256: fmt.Sprintf("%064x", i), Expires: now.Unix()})
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var candidate AuthPolicy
			if err := json.Unmarshal(original, &candidate); err != nil {
				t.Fatal(err)
			}
			mutation(&candidate)
			raw, _ := json.Marshal(candidate)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadAuthPolicy(path); !errors.Is(err, ErrAuthPolicyUnavailable) {
				t.Fatal("bad policy accepted", err)
			}
		})
	}
	for _, raw := range [][]byte{append(bytes.Clone(original), original...), []byte(`{"version":1,"version":1}`), bytes.Repeat([]byte(" "), MaxAuthPolicyBytes+1)} {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAuthPolicy(path); !errors.Is(err, ErrAuthPolicyUnavailable) {
			t.Fatal("non-strict or oversized policy accepted", err)
		}
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < MaxAuthPolicyKeys; i++ {
		if _, err := RotateAuthPolicy(t.Context(), path, fmt.Sprintf("key-%d", i), now); err != nil {
			t.Fatal(err)
		}
	}
	full, _ := os.ReadFile(path)
	if _, err := RotateAuthPolicy(t.Context(), path, "overflow", now); !errors.Is(err, ErrAuthPolicyConflict) {
		t.Fatal("key limit ignored", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(full, after) {
		t.Fatal("rejected key update changed file")
	}

	// A full, valid revocation list rejects one more live entry without
	// publishing a partial policy. Expired entries are pruned on a later update.
	policy.Revoked = nil
	for i := 0; i < MaxRevokedCredentials; i++ {
		policy.Revoked = append(policy.Revoked, RevokedCredential{SHA256: fmt.Sprintf("%064x", i+1), Expires: now.Add(time.Minute).Unix()})
	}
	raw, _ := json.Marshal(policy)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	token := authTestIssue(t, path, authTestGrant(now), now)
	if err := RevokeAuthPolicyCredential(t.Context(), path, token, now); !errors.Is(err, ErrAuthPolicyConflict) {
		t.Fatal("revocation limit ignored", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(raw, after) {
		t.Fatal("rejected revocation changed file")
	}
	if err := RevokeAuthPolicyCredential(t.Context(), path, token, now.Add(2*time.Minute)); err != nil {
		t.Fatal("expired revocations were not pruned", err)
	}
	loaded, err := LoadAuthPolicy(path)
	if err != nil || len(loaded.Revoked) != 1 {
		t.Fatal("wrong pruned revocation state", err)
	}
}

func TestAuthPolicyNonregularInputsAndProjectedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	if _, err := InitAuthPolicy(t.Context(), path, "initial", nil); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "projected-policy")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuthPolicy(link); err != nil {
		t.Fatal("projected Secret symlink must be readable", err)
	}
	before, _ := os.ReadFile(path)
	if _, err := RotateAuthPolicy(t.Context(), link, "other", time.Now()); !errors.Is(err, ErrAuthPolicyUnavailable) {
		t.Fatal("management followed a policy symlink", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("symlink update changed target")
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := LoadAuthPolicy(fifo); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrAuthPolicyUnavailable) {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		// Release an accidentally blocking old implementation before failing.
		fd, _ := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if fd >= 0 {
			unix.Close(fd)
		}
		t.Fatal("nonregular policy blocked while opening a FIFO")
	}
	badLock := filepath.Join(t.TempDir(), "bad.json")
	if err := os.Symlink(path, badLock+".lock"); err != nil {
		t.Fatal(err)
	}
	if _, err := InitAuthPolicy(t.Context(), badLock, "initial", nil); !errors.Is(err, ErrAuthPolicyUnavailable) {
		t.Fatal("management followed a lock symlink", err)
	}
}

func TestAuthPolicyConcurrentManagementAndReaders(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "policy.json")
	if _, err := InitAuthPolicy(t.Context(), path, "initial", nil); err != nil {
		t.Fatal(err)
	}
	var tokens []string
	for i := 0; i < 10; i++ {
		tokens = append(tokens, authTestIssue(t, path, authTestGrant(now), now))
	}
	errs := make(chan error, 24)
	var writers, readers sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := LoadAuthPolicy(path); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	for i, token := range tokens {
		writers.Go(func() {
			_, err := RotateAuthPolicy(t.Context(), path, fmt.Sprintf("rotation-%d", i), now)
			if err != nil {
				errs <- err
			}
		})
		writers.Go(func() {
			if err := RevokeAuthPolicyCredential(t.Context(), path, token, now); err != nil {
				errs <- err
			}
		})
	}
	writers.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Fatal("concurrent management or atomic reader failed", err)
	}
	policy, err := LoadAuthPolicy(path)
	if err != nil || len(policy.Keys) != 11 || len(policy.Revoked) != 10 {
		t.Fatal("concurrent update lost keys or revocations", err)
	}
	for _, token := range tokens {
		if _, err := AuthenticateAuthPolicy(path, token, now); !errors.Is(err, errDenied) {
			t.Fatal("concurrent revocation was lost", err)
		}
	}
}

func TestAuthPolicyFailedWritePreservesState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-denied fixture requires an unprivileged process")
	}
	now := time.Now()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	if _, err := InitAuthPolicy(t.Context(), path, "initial", nil); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	token := authTestIssue(t, path, authTestGrant(now), now)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	if _, err := RotateAuthPolicy(t.Context(), path, "not-published", now); !errors.Is(err, ErrAuthPolicyUnavailable) {
		t.Fatal("write to non-writable parent unexpectedly succeeded", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed physical write changed published policy", err)
	}
	if _, err := AuthenticateAuthPolicy(path, token, now); err != nil {
		t.Fatal("failed write invalidated previous credential", err)
	}
}

func TestAuthPolicyLockProcess(t *testing.T) {
	if path := os.Getenv("AGENTMOOR_AUTH_LOCK_HELPER"); path != "" {
		lock, _, err := lockAuthPolicy(context.Background(), path)
		if err != nil {
			os.Exit(3)
		}
		defer lock.Close()
		fmt.Println("locked")
		for {
			time.Sleep(time.Hour)
		}
	}
	now := time.Now()
	path := filepath.Join(t.TempDir(), "policy.json")
	if _, err := InitAuthPolicy(t.Context(), path, "initial", nil); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	lockInfo, err := os.Stat(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestAuthPolicyLockProcess$")
	child.Env = []string{"AGENTMOOR_AUTH_LOCK_HELPER=" + path}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if child.ProcessState == nil {
			child.Process.Kill()
			child.Wait()
		}
	})
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "locked\n" {
			t.Fatal("owned lock helper did not acquire lock")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owned lock helper did not start")
	}
	wait, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	_, err = RotateAuthPolicy(wait, path, "blocked", now)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("management ignored held lock cancellation", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("failed lock acquisition changed policy")
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = child.Wait(); err == nil {
		t.Fatal("owned lock helper did not die")
	}
	if _, err = RotateAuthPolicy(t.Context(), path, "after-crash", now); err != nil {
		t.Fatal("kernel did not release crashed owner's lock", err)
	}
	afterInfo, err := os.Stat(path + ".lock")
	if err != nil || !os.SameFile(lockInfo, afterInfo) {
		t.Fatal("lock inode was replaced rather than reused", err)
	}
}
