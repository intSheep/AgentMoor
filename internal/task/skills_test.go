package task

import (
	"strings"
	"testing"

	"agentmoor/internal/asset"
)

func TestSkillPinsRequireBoundedNamesAndImmutableSkillReferences(t *testing.T) {
	key := asset.Key{Kind: "skill", ID: "catalog/report"}
	valid := SkillPin{Name: "report-rules", Source: Ref{Key: key, Version: 1, SHA256: strings.Repeat("a", 64), ContentPath: key.ContentPath(1)}}
	if err := ValidSkillPins(nil); err != nil {
		t.Fatal("old workers must remain compatible", err)
	}
	if err := ValidSkillPins([]SkillPin{valid}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*SkillPin)
	}{
		{"path instead of name", func(p *SkillPin) { p.Name = "../escape" }},
		{"empty name", func(p *SkillPin) { p.Name = "" }},
		{"long name", func(p *SkillPin) { p.Name = strings.Repeat("a", 65) }},
		{"other kind", func(p *SkillPin) { p.Source.Kind = "workspace" }},
		{"unsafe id", func(p *SkillPin) { p.Source.ID = "../escape" }},
		{"unversioned", func(p *SkillPin) { p.Source.Version = 0 }},
		{"malformed digest", func(p *SkillPin) { p.Source.SHA256 = "digest" }},
		{"missing content path", func(p *SkillPin) { p.Source.ContentPath = "" }},
		{"unversioned path", func(p *SkillPin) { p.Source.ContentPath = "/v1/content" }},
		{"cross origin", func(p *SkillPin) { p.Source.ContentPath = "https://other.invalid" + p.Source.ContentPath }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pin := valid
			tc.edit(&pin)
			if err := ValidSkillPins([]SkillPin{pin}); err == nil {
				t.Fatal("invalid pin accepted", pin)
			}
		})
	}
	if err := ValidSkillPins([]SkillPin{valid, valid}); err == nil {
		t.Fatal("duplicate target accepted")
	}
	if err := ValidSkillPins(make([]SkillPin, MaxSkills+1)); err == nil {
		t.Fatal("unbounded skill list accepted")
	}
}
