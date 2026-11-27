package platform

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/intSheep/AgentMoor/internal/asset"
)

type Grant struct {
	Scope   asset.Scope `json:"scope"`
	Read    bool        `json:"read"`
	Write   bool        `json:"write"`
	Submit  bool        `json:"submit,omitempty"`
	Execute bool        `json:"execute,omitempty"`
	Collect bool        `json:"collect,omitempty"`
	Expires int64       `json:"expires"`
}

var errDenied = errors.New("access denied")

func Issue(secret []byte, grant Grant, now time.Time) (string, error) {
	if len(secret) < 32 || !grant.Scope.Valid() || !(grant.Read || grant.Write) || grant.Expires <= now.Unix() || grant.Expires > now.Add(24*time.Hour).Unix() {
		return "", errDenied
	}
	b, err := json.Marshal(grant)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func authenticate(secret []byte, token string, now time.Time) (Grant, error) {
	var grant Grant
	body, sig, ok := strings.Cut(token, ".")
	if !ok || len(token) > 4096 {
		return grant, errDenied
	}
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	if err != nil || !hmac.Equal(raw, mac.Sum(nil)) {
		return grant, errDenied
	}
	data, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || json.Unmarshal(data, &grant) != nil || !grant.Scope.Valid() || grant.Expires <= now.Unix() {
		return Grant{}, errDenied
	}
	return grant, nil
}
