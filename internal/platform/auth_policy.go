package platform

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	MaxAuthPolicyBytes    = 1 << 20
	MaxAuthPolicyKeys     = 16
	MaxRevokedCredentials = 4096
)

var (
	ErrAuthPolicyUnavailable = errors.New("authentication policy unavailable")
	ErrAuthPolicyConflict    = errors.New("authentication policy conflict")
	authKeyID                = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	authCredentialID         = regexp.MustCompile(`^[a-f0-9]{32}$`)
	authDigest               = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// AuthPolicy is a private, operator-managed file. It must never be mounted in
// an untrusted execution environment. The server reloads it for every request.
type AuthPolicy struct {
	Version int                 `json:"version"`
	Active  string              `json:"active"`
	Keys    []AuthPolicyKey     `json:"keys"`
	Revoked []RevokedCredential `json:"revoked"`
}

type AuthPolicyKey struct {
	ID     string `json:"id"`
	Key    []byte `json:"key"`
	Legacy bool   `json:"legacy,omitempty"`
}

type RevokedCredential struct {
	SHA256  string `json:"sha256"`
	Expires int64  `json:"expires"`
}

type policyCredential struct {
	Grant
	Version *int    `json:"v,omitempty"`
	KeyID   *string `json:"kid,omitempty"`
	ID      *string `json:"cid,omitempty"`
}

func (p *AuthPolicy) validate() error {
	if p.Version != 1 || !authKeyID.MatchString(p.Active) || len(p.Keys) == 0 || len(p.Keys) > MaxAuthPolicyKeys || len(p.Revoked) > MaxRevokedCredentials {
		return ErrAuthPolicyUnavailable
	}
	ids, materials, digests := map[string]bool{}, map[string]bool{}, map[string]bool{}
	found, legacy := false, 0
	for _, key := range p.Keys {
		if !authKeyID.MatchString(key.ID) || len(key.Key) != 32 || ids[key.ID] || materials[string(key.Key)] {
			return ErrAuthPolicyUnavailable
		}
		ids[key.ID] = true
		materials[string(key.Key)] = true
		found = found || key.ID == p.Active
		if key.Legacy {
			legacy++
		}
	}
	if !found || legacy > 1 {
		return ErrAuthPolicyUnavailable
	}
	for _, revoked := range p.Revoked {
		if !authDigest.MatchString(revoked.SHA256) || revoked.Expires <= 0 || digests[revoked.SHA256] {
			return ErrAuthPolicyUnavailable
		}
		digests[revoked.SHA256] = true
	}
	return nil
}

// strictAuthJSON rejects unknown fields, duplicate members and trailing JSON.
// Policy and credential JSON are small and bounded before this function runs.
func strictAuthJSON(raw []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var value func() error
	value = func() error {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				field, err := dec.Token()
				name, ok := field.(string)
				// All supported members use lower-case ASCII JSON tags.
				// encoding/json also folds Unicode aliases (for example a
				// long s), which must not alias a canonical signed member.
				canonical := true
				for i := 0; i < len(name); i++ {
					if name[i] > 127 || (name[i] >= 'A' && name[i] <= 'Z') {
						canonical = false
						break
					}
				}
				if err != nil || !ok || !canonical || seen[name] {
					return ErrAuthPolicyUnavailable
				}
				seen[name] = true
				if err := value(); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim('}') {
				return ErrAuthPolicyUnavailable
			}
		case '[':
			for dec.More() {
				if err := value(); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim(']') {
				return ErrAuthPolicyUnavailable
			}
		default:
			return ErrAuthPolicyUnavailable
		}
		return nil
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return ErrAuthPolicyUnavailable
	}
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return ErrAuthPolicyUnavailable
	}
	return nil
}

func LoadAuthPolicy(path string) (*AuthPolicy, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrAuthPolicyUnavailable
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxAuthPolicyBytes {
		return nil, ErrAuthPolicyUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxAuthPolicyBytes+1))
	if err != nil || len(raw) > MaxAuthPolicyBytes {
		return nil, ErrAuthPolicyUnavailable
	}
	var policy AuthPolicy
	if strictAuthJSON(raw, &policy) != nil || policy.validate() != nil {
		return nil, ErrAuthPolicyUnavailable
	}
	return &policy, nil
}

func credentialDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (p *AuthPolicy) verify(token string, now time.Time, checkRevoked bool) (Grant, error) {
	var empty Grant
	body, signature, ok := strings.Cut(token, ".")
	if !ok || len(token) > 4096 || strings.Contains(signature, ".") {
		return empty, errDenied
	}
	// Canonical base64 prevents a differently encoded signature bypassing an
	// exact-token revocation. Legacy Issue emits this canonical form as well.
	raw, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != body {
		return empty, errDenied
	}
	macBytes, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || len(macBytes) != sha256.Size || base64.RawURLEncoding.EncodeToString(macBytes) != signature {
		return empty, errDenied
	}
	var claim policyCredential
	if strictAuthJSON(raw, &claim) != nil {
		return empty, errDenied
	}
	var key []byte
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		return empty, errDenied
	}
	_, hasVersion := members["v"]
	_, hasKey := members["kid"]
	_, hasID := members["cid"]
	modern := hasVersion || hasKey || hasID
	if modern {
		if claim.Version == nil || *claim.Version != 2 || claim.KeyID == nil || !authKeyID.MatchString(*claim.KeyID) || claim.ID == nil || !authCredentialID.MatchString(*claim.ID) {
			return empty, errDenied
		}
		for _, candidate := range p.Keys {
			if candidate.ID == *claim.KeyID {
				key = candidate.Key
				break
			}
		}
	} else {
		for _, candidate := range p.Keys {
			if candidate.Legacy {
				key = candidate.Key
				break
			}
		}
	}
	if len(key) != 32 {
		return empty, errDenied
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	if !hmac.Equal(macBytes, mac.Sum(nil)) || !claim.Scope.Valid() || !(claim.Read || claim.Write) || claim.Expires <= now.Unix() {
		return empty, errDenied
	}
	if checkRevoked {
		digest := credentialDigest(token)
		for _, revoked := range p.Revoked {
			if revoked.SHA256 == digest && revoked.Expires > now.Unix() {
				return empty, errDenied
			}
		}
	}
	return claim.Grant, nil
}

// AuthenticateAuthPolicy never falls back to API.Secret or to another key when
// a v2 key ID is missing or unknown.
func AuthenticateAuthPolicy(path, token string, now time.Time) (Grant, error) {
	policy, err := LoadAuthPolicy(path)
	if err != nil {
		return Grant{}, err
	}
	return policy.verify(token, now, true)
}

func IssueAuthPolicy(path string, grant Grant, now time.Time) (string, error) {
	policy, err := LoadAuthPolicy(path)
	if err != nil {
		return "", err
	}
	if !grant.Scope.Valid() || !(grant.Read || grant.Write) || grant.Expires <= now.Unix() || grant.Expires > now.Add(24*time.Hour).Unix() {
		return "", errDenied
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return "", ErrAuthPolicyUnavailable
	}
	version, id := 2, hex.EncodeToString(nonce)
	claim := policyCredential{Grant: grant, Version: &version, KeyID: &policy.Active, ID: &id}
	raw, err := json.Marshal(claim)
	if err != nil {
		return "", ErrAuthPolicyUnavailable
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	for _, key := range policy.Keys {
		if key.ID != policy.Active {
			continue
		}
		mac := hmac.New(sha256.New, key.Key)
		mac.Write([]byte(body))
		token := body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		if len(token) > 4096 {
			return "", errDenied
		}
		return token, nil
	}
	return "", ErrAuthPolicyUnavailable
}

// lockAuthPolicy uses a persistent lock inode. Never unlink this file to
// recover a lock: the kernel releases the advisory lock when its owner exits.
func lockAuthPolicy(ctx context.Context, path string) (*os.File, string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", ErrAuthPolicyUnavailable
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return nil, "", ErrAuthPolicyUnavailable
	}
	absolute = filepath.Join(parent, filepath.Base(absolute))
	fd, err := unix.Open(absolute+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, "", ErrAuthPolicyUnavailable
	}
	file := os.NewFile(uintptr(fd), absolute+".lock")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, "", ErrAuthPolicyUnavailable
	}
	until := time.NewTimer(10 * time.Second)
	defer until.Stop()
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, "", err
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, absolute, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			file.Close()
			return nil, "", ErrAuthPolicyUnavailable
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, "", ctx.Err()
		case <-until.C:
			file.Close()
			return nil, "", ErrAuthPolicyConflict
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A failure after rename/link may leave the complete new policy published.
// Callers must re-read before retrying; do not restore an older policy, which
// could restore a revoked credential. No partially written policy is published.
func writeAuthPolicy(path string, policy *AuthPolicy, initial bool) error {
	if policy.validate() != nil {
		return ErrAuthPolicyUnavailable
	}
	raw, err := json.MarshalIndent(policy, "", "  ")
	if err != nil || len(raw)+1 > MaxAuthPolicyBytes {
		return ErrAuthPolicyUnavailable
	}
	if info, err := os.Lstat(path); err == nil {
		if initial {
			return ErrAuthPolicyConflict
		}
		if !info.Mode().IsRegular() {
			return ErrAuthPolicyUnavailable
		}
	} else if !errors.Is(err, os.ErrNotExist) || !initial {
		return ErrAuthPolicyUnavailable
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".auth-policy-*")
	if err != nil {
		return ErrAuthPolicyUnavailable
	}
	defer os.Remove(file.Name())
	_, err = file.Write(append(raw, '\n'))
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return ErrAuthPolicyUnavailable
	}
	if initial {
		err = os.Link(file.Name(), path)
	} else {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		return ErrAuthPolicyUnavailable
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return ErrAuthPolicyUnavailable
	}
	defer parent.Close()
	if parent.Sync() != nil {
		return ErrAuthPolicyUnavailable
	}
	return nil
}

func generatedAuthKeyID() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", ErrAuthPolicyUnavailable
	}
	return "key-" + hex.EncodeToString(raw), nil
}

// InitAuthPolicy creates a new private policy. legacyKey is imported only when
// the operator explicitly supplies one; fresh policies reject all legacy tokens.
func InitAuthPolicy(ctx context.Context, path, keyID string, legacyKey []byte) (string, error) {
	lock, path, err := lockAuthPolicy(ctx, path)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if keyID == "" {
		keyID, err = generatedAuthKeyID()
		if err != nil {
			return "", err
		}
	}
	if !authKeyID.MatchString(keyID) || (legacyKey != nil && len(legacyKey) != 32) {
		return "", ErrAuthPolicyUnavailable
	}
	key := bytes.Clone(legacyKey)
	if key == nil {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return "", ErrAuthPolicyUnavailable
		}
	}
	policy := &AuthPolicy{Version: 1, Active: keyID, Keys: []AuthPolicyKey{{ID: keyID, Key: key, Legacy: legacyKey != nil}}, Revoked: []RevokedCredential{}}
	if err = writeAuthPolicy(path, policy, true); err != nil {
		return "", err
	}
	return keyID, nil
}

func updateAuthPolicy(ctx context.Context, path string, now time.Time, update func(*AuthPolicy) error) error {
	lock, path, err := lockAuthPolicy(ctx, path)
	if err != nil {
		return err
	}
	defer lock.Close()
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return ErrAuthPolicyUnavailable
	}
	policy, err := LoadAuthPolicy(path)
	if err != nil {
		return err
	}
	kept := policy.Revoked[:0]
	for _, revoked := range policy.Revoked {
		if revoked.Expires > now.Unix() {
			kept = append(kept, revoked)
		}
	}
	policy.Revoked = kept
	if err := update(policy); err != nil {
		return err
	}
	return writeAuthPolicy(path, policy, false)
}

func RotateAuthPolicy(ctx context.Context, path, keyID string, now time.Time) (string, error) {
	var err error
	if keyID == "" {
		keyID, err = generatedAuthKeyID()
		if err != nil {
			return "", err
		}
	}
	if !authKeyID.MatchString(keyID) {
		return "", ErrAuthPolicyUnavailable
	}
	err = updateAuthPolicy(ctx, path, now, func(policy *AuthPolicy) error {
		if len(policy.Keys) >= MaxAuthPolicyKeys {
			return ErrAuthPolicyConflict
		}
		for _, key := range policy.Keys {
			if key.ID == keyID {
				return ErrAuthPolicyConflict
			}
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return ErrAuthPolicyUnavailable
		}
		policy.Keys = append(policy.Keys, AuthPolicyKey{ID: keyID, Key: key})
		policy.Active = keyID
		return nil
	})
	return keyID, err
}

func RetireAuthPolicyKey(ctx context.Context, path, keyID string, now time.Time) error {
	return updateAuthPolicy(ctx, path, now, func(policy *AuthPolicy) error {
		if !authKeyID.MatchString(keyID) || keyID == policy.Active {
			return ErrAuthPolicyConflict
		}
		for i, key := range policy.Keys {
			if key.ID == keyID {
				policy.Keys = append(policy.Keys[:i], policy.Keys[i+1:]...)
				return nil
			}
		}
		return ErrAuthPolicyConflict
	})
}

func RevokeAuthPolicyCredential(ctx context.Context, path, token string, now time.Time) error {
	return updateAuthPolicy(ctx, path, now, func(policy *AuthPolicy) error {
		grant, err := policy.verify(token, now, false)
		if err != nil {
			return err
		}
		digest := credentialDigest(token)
		for _, revoked := range policy.Revoked {
			if revoked.SHA256 == digest {
				return nil
			}
		}
		if len(policy.Revoked) >= MaxRevokedCredentials {
			return ErrAuthPolicyConflict
		}
		policy.Revoked = append(policy.Revoked, RevokedCredential{SHA256: digest, Expires: grant.Expires})
		return nil
	})
}
