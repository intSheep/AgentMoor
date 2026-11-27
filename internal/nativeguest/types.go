// Package nativeguest defines the bounded handoff used by a credentialless
// native execution guest. Its protocol does not confer platform authority.
package nativeguest

import (
	"fmt"
	"mime"
	"path"
	"regexp"
	"strings"

	"github.com/intSheep/AgentMoor/internal/asset"
	"github.com/intSheep/AgentMoor/internal/task"
)

const (
	Protocol                  = "native-guest.v1"
	MaxBundleBytes      int64 = 64 << 20
	MaxRequestJSONBytes       = 128 << 10
	MaxResultJSONBytes        = 16 << 20
	MaxTextBytes              = 2 << 20
)

type Limits struct{ MaxInputBytes, MaxOutputBytes int64 }

func DefaultLimits() Limits { return Limits{MaxBundleBytes, MaxBundleBytes} }
func (l Limits) normalized() (Limits, error) {
	if l.MaxInputBytes == 0 {
		l.MaxInputBytes = MaxBundleBytes
	}
	if l.MaxOutputBytes == 0 {
		l.MaxOutputBytes = MaxBundleBytes
	}
	if l.MaxInputBytes < 1 || l.MaxInputBytes > MaxBundleBytes || l.MaxOutputBytes < 1 || l.MaxOutputBytes > MaxBundleBytes {
		return Limits{}, fmt.Errorf("invalid native guest limits")
	}
	return l, nil
}

type BlobMeta struct {
	Format    string `json:"format"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}
type Skill struct {
	Name string   `json:"name"`
	Blob BlobMeta `json:"blob"`
}
type Request struct {
	Version         string    `json:"version"`
	AdapterID       string    `json:"adapter_id"`
	AdapterVersion  string    `json:"adapter_version"`
	RuntimeVersion  string    `json:"runtime_version"`
	Spec            task.Spec `json:"spec"`
	ParentSessionID string    `json:"parent_session_id,omitempty"`
	Workspace       *BlobMeta `json:"workspace,omitempty"`
	Session         *BlobMeta `json:"session,omitempty"`
	Skills          []Skill   `json:"skills,omitempty"`
}
type Result struct {
	Version        string   `json:"version"`
	AdapterID      string   `json:"adapter_id"`
	AdapterVersion string   `json:"adapter_version"`
	RuntimeVersion string   `json:"runtime_version"`
	SessionID      string   `json:"session_id"`
	Text           string   `json:"text"`
	Workspace      BlobMeta `json:"workspace"`
	Session        BlobMeta `json:"session"`
}

// Files contains only decoder-assigned local paths, never wire-supplied paths.
type Files struct {
	Workspace, Session string
	Skills             []string
}

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var formatID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var skillName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func versionText(v string) bool {
	if v == "" || len(v) > 256 || strings.TrimSpace(v) != v {
		return false
	}
	for _, r := range v {
		if r < 32 || r > 126 {
			return false
		}
	}
	return true
}
func sessionID(id string) bool { return len(id) <= 128 && asset.SafePath(id) && path.Base(id) == id }
func (b BlobMeta) validate() error {
	if !formatID.MatchString(b.Format) || !digest.MatchString(b.SHA256) || b.Size < 1 || b.Size > MaxBundleBytes {
		return fmt.Errorf("invalid native guest blob")
	}
	if !versionText(b.MediaType) {
		return fmt.Errorf("invalid native guest media type")
	}
	if _, _, err := mime.ParseMediaType(b.MediaType); err != nil {
		return fmt.Errorf("invalid native guest media type")
	}
	return nil
}
func (b BlobMeta) workspace() error {
	if err := b.validate(); err != nil {
		return err
	}
	if b.Format != "workspace-tar.v1" || b.MediaType != "application/x-tar" {
		return fmt.Errorf("invalid guest workspace format")
	}
	return nil
}
func (r Request) Validate() error {
	if r.Version != Protocol || !identifier.MatchString(r.AdapterID) || !versionText(r.AdapterVersion) || !versionText(r.RuntimeVersion) || !r.Spec.Valid() {
		return fmt.Errorf("invalid native guest request")
	}
	if r.ParentSessionID == "" {
		if r.Workspace != nil || r.Session != nil {
			return fmt.Errorf("initial guest task cannot supply parent state")
		}
	} else {
		if !sessionID(r.ParentSessionID) || r.Workspace == nil || r.Session == nil {
			return fmt.Errorf("guest continuation requires workspace and session")
		}
		if err := r.Workspace.workspace(); err != nil {
			return err
		}
		if err := r.Session.validate(); err != nil {
			return err
		}
	}
	if len(r.Skills) > task.MaxSkills {
		return fmt.Errorf("too many guest skills")
	}
	seen := map[string]bool{}
	for _, s := range r.Skills {
		if !skillName.MatchString(s.Name) || seen[s.Name] {
			return fmt.Errorf("invalid or duplicate guest skill")
		}
		seen[s.Name] = true
		if err := s.Blob.validate(); err != nil {
			return err
		}
		if s.Blob.Format != "skill-tar.v1" || s.Blob.MediaType != "application/x-tar" {
			return fmt.Errorf("invalid guest skill format")
		}
	}
	return nil
}
func (r Result) Validate() error {
	if r.Version != Protocol || !identifier.MatchString(r.AdapterID) || !versionText(r.AdapterVersion) || !versionText(r.RuntimeVersion) || !sessionID(r.SessionID) || len(r.Text) > MaxTextBytes {
		return fmt.Errorf("invalid native guest result")
	}
	if err := r.Workspace.workspace(); err != nil {
		return err
	}
	return r.Session.validate()
}
func (r Result) Match(expected Request) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if r.AdapterID != expected.AdapterID || r.AdapterVersion != expected.AdapterVersion || r.RuntimeVersion != expected.RuntimeVersion || (expected.ParentSessionID != "" && r.SessionID != expected.ParentSessionID) {
		return fmt.Errorf("guest result differs from assigned request")
	}
	if expected.ParentSessionID != "" && (r.Session.Format != expected.Session.Format || r.Session.MediaType != expected.Session.MediaType) {
		return fmt.Errorf("guest continuation changed native session format")
	}
	return nil
}
