// Package adapter defines the runtime-facing contracts used by discovery and synchronization.
package adapter

import (
	"context"
	"os"
)

type AssetKind string

const (
	Session   AssetKind = "session"
	Skill     AssetKind = "skill"
	Artifact  AssetKind = "artifact"
	Workspace AssetKind = "workspace"
)

type Capability struct {
	Operation   string `json:"operation"`
	Mode        string `json:"mode"`
	Reliability string `json:"reliability"`
}

type Descriptor struct {
	TaskModes     []string                   `json:"task_modes,omitempty"`
	TestedRuntime string                     `json:"tested_runtime,omitempty"`
	ID            string                     `json:"id"`
	Version       string                     `json:"version"`
	Capabilities  map[AssetKind][]Capability `json:"capabilities"`
}

// Detect returns observed workspace markers. An empty result means no match.
type Adapter interface {
	Descriptor() Descriptor
	Detect(context.Context, *os.Root) ([]string, error)
}

// SkillRef identifies a manifest relative to the supplied workspace.
// The runtime may apply additional rules before loading it.
type SkillRef struct {
	Manifest string `json:"manifest"`
}

type SkillDiscoverer interface {
	DiscoverSkills(context.Context, *os.Root) ([]SkillRef, error)
}
