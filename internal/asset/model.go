// Package asset defines the versioned wire format shared by worker and platform.
package asset

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MaxBytes int64 = 8 << 30

var component = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
var checksum = regexp.MustCompile("^[a-f0-9]{64}$")

type Scope struct {
	Tenant  string `json:"tenant"`
	Project string `json:"project"`
	Runtime string `json:"runtime"`
	Adapter string `json:"adapter"`
}

func (s Scope) Valid() bool {
	return component.MatchString(s.Tenant) && component.MatchString(s.Project) && component.MatchString(s.Runtime) && component.MatchString(s.Adapter)
}

func (s Scope) Prefix() string {
	return strings.Join([]string{s.Tenant, s.Project, s.Runtime, s.Adapter}, "/")
}

type Key struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func (k Key) Valid() bool {
	switch k.Kind {
	case "workspace", "skill", "artifact", "session":
	default:
		return false
	}
	return SafePath(k.ID) && len(k.ID) <= 4096
}

func SafePath(s string) bool {
	return fs.ValidPath(s) && s != "." && !strings.ContainsAny(s, "\\:\x00\r\n")
}

type Metadata struct {
	Key
	AdapterVersion  string   `json:"adapter_version"`
	RuntimeVersion  string   `json:"runtime_version,omitempty"`
	Format          string   `json:"format"`
	MediaType       string   `json:"media_type"`
	Name            string   `json:"name"`
	DeclaredVersion string   `json:"declared_version,omitempty"`
	Dependencies    []string `json:"dependencies,omitempty"`
	SessionID       string   `json:"session_id,omitempty"`
	Origin          string   `json:"origin"`
	Completeness    string   `json:"completeness"`
	SHA256          string   `json:"sha256"`
	Size            int64    `json:"size"`
}

type Change struct {
	Metadata
	Base int64 `json:"base_version"`
}

func (c Change) Validate() error {
	if !c.Key.Valid() || !checksum.MatchString(c.SHA256) || c.Base < 0 || c.Size < 0 || c.Size > MaxBytes || c.AdapterVersion == "" || c.Format == "" || c.MediaType == "" || c.Name == "" || c.Origin == "" || c.Completeness == "" {
		return fmt.Errorf("invalid asset metadata or version")
	}
	return nil
}

func Fingerprint[T Key | Metadata | Change](v T) string {
	p, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(p)
	return hex.EncodeToString(sum[:])
}

type Revision struct {
	Scope Scope `json:"scope"`
	Metadata
	Version     int64     `json:"version"`
	Created     time.Time `json:"created_at"`
	ContentPath string    `json:"content_path"`
}

func (k Key) ContentPath(v int64) string {
	q := url.Values{"kind": {k.Kind}, "id": {k.ID}, "version": {strconv.FormatInt(v, 10)}}
	return "/v1/content?" + q.Encode()
}

type Ticket struct {
	ID      string `json:"id"`
	Upload  bool   `json:"upload"`
	Version int64  `json:"version,omitempty"`
}

type Job struct {
	Operation string `json:"operation"`
	Key
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Updated string `json:"updated_at"`
}
