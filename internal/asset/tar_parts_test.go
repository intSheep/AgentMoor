package asset

import (
	"bytes"
	"strings"
	"testing"
)

func tarPartsChange(size int64) Change {
	return Change{Metadata: Metadata{Key: Key{Kind: "workspace", ID: "root"}, AdapterVersion: "test", Format: "workspace-tar.v1", MediaType: "application/x-tar", Name: "root", Origin: "runtime", Completeness: "persisted-files", SHA256: strings.Repeat("a", 64), Size: size}}
}

func TestTarPartsValidation(t *testing.T) {
	valid := func() TarParts {
		return TarParts{Format: TarPartsFormat, Parts: []TarPart{{Size: 3, Inline: []byte("hdr")}, {Size: 4, SHA256: strings.Repeat("b", 64)}, {Size: 4, SHA256: strings.Repeat("b", 64)}}}
	}
	if err := valid().Validate(tarPartsChange(11)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		edit func(*TarParts, *Change)
	}{
		{"format", func(m *TarParts, _ *Change) { m.Format = "tar-parts.v2" }},
		{"kind", func(_ *TarParts, c *Change) { c.Kind = "artifact" }},
		{"logical format", func(_ *TarParts, c *Change) { c.Format = "file.v1" }},
		{"media type", func(_ *TarParts, c *Change) { c.MediaType = "application/json" }},
		{"invalid change", func(_ *TarParts, c *Change) { c.Base = -1 }},
		{"empty", func(m *TarParts, _ *Change) { m.Parts = nil }},
		{"too many parts", func(m *TarParts, _ *Change) { m.Parts = make([]TarPart, MaxTarParts+1) }},
		{"mixed reference", func(m *TarParts, _ *Change) { m.Parts[0].SHA256 = strings.Repeat("a", 64) }},
		{"inline size", func(m *TarParts, _ *Change) { m.Parts[0].Size-- }},
		{"inline part limit", func(m *TarParts, c *Change) {
			m.Parts = []TarPart{{Size: MaxTarPartInlineBytes + 1, Inline: make([]byte, MaxTarPartInlineBytes+1)}}
			c.Size = MaxTarPartInlineBytes + 1
		}},
		{"zero size", func(m *TarParts, _ *Change) { m.Parts[1].Size = 0 }},
		{"negative size", func(m *TarParts, _ *Change) { m.Parts[1].Size = -1 }},
		{"missing reference", func(m *TarParts, _ *Change) { m.Parts[1].SHA256 = "" }},
		{"bad reference", func(m *TarParts, _ *Change) { m.Parts[1].SHA256 = strings.Repeat("g", 64) }},
		{"uppercase reference", func(m *TarParts, _ *Change) { m.Parts[1].SHA256 = strings.Repeat("B", 64) }},
		{"object part limit", func(m *TarParts, c *Change) { m.Parts[1].Size = MaxTarPartBytes + 1; c.Size += MaxTarPartBytes - 3 }},
		{"same hash different size", func(m *TarParts, c *Change) { m.Parts[2].Size++; c.Size++ }},
		{"underflow", func(_ *TarParts, c *Change) { c.Size-- }},
		{"excess logical bytes", func(_ *TarParts, c *Change) { c.Size++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, ch := valid(), tarPartsChange(11)
			test.edit(&m, &ch)
			if err := m.Validate(ch); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

func TestTarPartsAggregateBounds(t *testing.T) {
	inline := bytes.Repeat([]byte{'x'}, MaxTarPartInlineBytes)
	m := TarParts{Format: TarPartsFormat}
	for range MaxTarPartsInlineBytes / MaxTarPartInlineBytes {
		m.Parts = append(m.Parts, TarPart{Size: int64(len(inline)), Inline: inline})
	}
	if err := m.Validate(tarPartsChange(MaxTarPartsInlineBytes)); err != nil {
		t.Fatal("exact inline limit rejected", err)
	}
	m.Parts = append(m.Parts, TarPart{Size: 1, Inline: []byte{'x'}})
	if err := m.Validate(tarPartsChange(MaxTarPartsInlineBytes + 1)); err == nil || !strings.Contains(err.Error(), "inline limit") {
		t.Fatal("excess inline total accepted", err)
	}
	m.Parts = m.Parts[:len(m.Parts)-1]
	total := int64(MaxTarPartsInlineBytes)
	for len(m.Parts) < MaxTarParts {
		m.Parts = append(m.Parts, TarPart{SHA256: strings.Repeat("a", 64), Size: 1})
		total++
	}
	if err := m.Validate(tarPartsChange(total)); err == nil || !strings.Contains(err.Error(), "manifest limit") {
		t.Fatal("excess encoded manifest accepted", err)
	}
}

func TestTarPartsDigestCoversOrderAndInlineBytes(t *testing.T) {
	m := TarParts{Format: TarPartsFormat, Parts: []TarPart{{Size: 3, Inline: []byte("abc")}, {Size: 3, Inline: []byte("def")}}}
	a, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	m.Parts[0], m.Parts[1] = m.Parts[1], m.Parts[0]
	b, err := m.Digest()
	if err != nil || a == b {
		t.Fatal("digest did not bind part order", err)
	}
	m.Parts[0].Inline[0] = 'z'
	c, err := m.Digest()
	if err != nil || b == c {
		t.Fatal("digest did not bind inline bytes", err)
	}
}
