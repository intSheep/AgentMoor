package nativeguest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
)

type inspectEOFReader struct {
	*bytes.Reader
	eof bool
}

func (r *inspectEOFReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.eof = r.eof || err == io.EOF
	return n, err
}

func TestInspectRequestValidatesCompleteEnvelopeWithoutExtraction(t *testing.T) {
	req := bundleTestRequest()
	// Inner bytes are deliberately opaque. The inspector verifies their raw
	// checksum, never imports native data or extracts/validates a tree.
	body := []byte("opaque original Skill bytes; no extraction is requested")
	meta := bundleTestMeta(body, "skill-tar.v1", "application/x-tar")
	req.Skills = []Skill{{Name: "rules", Blob: meta}}
	raw := bundleWrite(t, req, map[string][]byte{"skills/0.tar": body})
	reader := &inspectEOFReader{Reader: bytes.NewReader(raw)}
	got, err := InspectRequest(t.Context(), reader, Limits{MaxInputBytes: int64(len(raw))})
	if err != nil || !reflect.DeepEqual(got, req) || !reader.eof {
		t.Fatal("complete outer envelope not independently inspected", err, reader.eof)
	}
	if _, err = InspectRequest(t.Context(), bytes.NewReader(raw), Limits{MaxInputBytes: int64(len(raw) - 1)}); err == nil {
		t.Fatal("inspector exceeded exact byte bound")
	}
	corrupt := bytes.Replace(raw, body, bytes.Repeat([]byte("x"), len(body)), 1)
	if _, err = InspectRequest(t.Context(), bytes.NewReader(corrupt), Limits{}); err == nil {
		t.Fatal("wrong raw slot hash was accepted")
	}
	for _, bad := range [][]byte{raw[:len(raw)-1], append(append([]byte(nil), raw...), byte('x'))} {
		if _, err = InspectRequest(t.Context(), bytes.NewReader(bad), Limits{}); err == nil {
			t.Fatal("truncation or trailing data was accepted")
		}
	}
}

func TestInspectRequestRejectsAmbiguousMetadataSlotsAndCancellation(t *testing.T) {
	req := bundleTestRequest()
	meta, _ := json.Marshal(req)
	for _, bad := range [][]byte{
		bytes.Replace(meta, []byte(`"version":`), []byte(`"Version":`), 1),
		append([]byte(`{"version":"duplicate",`), meta[1:]...),
		bytes.Replace(meta, []byte(`"adapter_id":"fixture"`), []byte(`"adapter_id":null`), 1),
	} {
		raw := bundleCustom(t, bad, nil, nil)
		if _, err := InspectRequest(t.Context(), bytes.NewReader(raw), Limits{}); err == nil {
			t.Fatal("ambiguous metadata was accepted")
		}
	}
	extra := bundleCustom(t, meta, map[string][]byte{"unexpected": []byte("x")}, []string{"unexpected"})
	if _, err := InspectRequest(t.Context(), bytes.NewReader(extra), Limits{}); err == nil {
		t.Fatal("extra slot was accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := InspectRequest(ctx, bytes.NewReader(bundleWrite(t, req, nil)), Limits{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled inspection continued", err)
	}
}
