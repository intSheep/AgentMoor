package nativeguest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/intSheep/AgentMoor/internal/task"
)

func bundleTestMeta(body []byte, format, media string) BlobMeta {
	sum := sha256.Sum256(body)
	return BlobMeta{Format: format, MediaType: media, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
}
func bundleTestRequest() Request {
	return Request{Version: Protocol, AdapterID: "fixture", AdapterVersion: "1", RuntimeVersion: "fixture 1", Spec: task.Spec{Mode: task.FileTurn, Instruction: "Render the supplied input", Input: "input preserved", Output: "report.txt"}}
}
func bundleTree(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(body)), Mode: 0600, Typeflag: tar.TypeReg, Format: tar.FormatPAX, ModTime: time.Unix(0, 0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func bundleOpen(files map[string][]byte) func(string) (io.ReadCloser, error) {
	return func(name string) (io.ReadCloser, error) {
		raw, ok := files[name]
		if !ok {
			return nil, os.ErrNotExist
		}
		return io.NopCloser(bytes.NewReader(raw)), nil
	}
}
func bundleWrite(t *testing.T, req Request, files map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := WriteRequest(t.Context(), &out, req, bundleOpen(files), Limits{}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func bundleCustom(t *testing.T, meta []byte, parts map[string][]byte, order []string) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for _, name := range append([]string{"request.json"}, order...) {
		raw := parts[name]
		if name == "request.json" {
			raw = meta
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(raw)), Mode: 0600, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func TestBundleRequestAndResultRoundTrip(t *testing.T) {
	req := bundleTestRequest()
	workspace := bundleTree(t, "input.txt", []byte("original workspace contents"))
	session := bundleTree(t, "session.json", []byte("{\"original\":\"native bytes\"}"))
	skill := bundleTree(t, "SKILL.md", []byte("original Skill contents"))
	wm, sm := bundleTestMeta(workspace, "workspace-tar.v1", "application/x-tar"), bundleTestMeta(session, "fixture-session.v1", "application/x-tar")
	req.ParentSessionID = "native-session"
	req.Workspace = &wm
	req.Session = &sm
	req.Skills = []Skill{{Name: "rules", Blob: bundleTestMeta(skill, "skill-tar.v1", "application/x-tar")}}
	files := map[string][]byte{"workspace.tar": workspace, "parent.session": session, "skills/0.tar": skill}
	raw := bundleWrite(t, req, files)
	dest := filepath.Join(t.TempDir(), "input")
	got, paths, err := DecodeRequest(t.Context(), bytes.NewReader(raw), dest, Limits{})
	if err != nil || !reflect.DeepEqual(got, req) {
		t.Fatal("request round trip", got, err)
	}
	for name, path := range map[string]string{"workspace.tar": paths.Workspace, "parent.session": paths.Session, "skills/0.tar": paths.Skills[0]} {
		body, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(body, files[name]) {
			t.Fatal("raw blob changed", name, err)
		}
	}
	result := Result{Version: Protocol, AdapterID: req.AdapterID, AdapterVersion: req.AdapterVersion, RuntimeVersion: req.RuntimeVersion, SessionID: req.ParentSessionID, Text: "done", Workspace: wm, Session: sm}
	var output bytes.Buffer
	if err := WriteResult(t.Context(), &output, result, bundleOpen(map[string][]byte{"workspace.tar": workspace, "session.blob": session}), Limits{}); err != nil {
		t.Fatal(err)
	}
	outdir := filepath.Join(t.TempDir(), "result")
	res, err := ReadResult(t.Context(), bytes.NewReader(output.Bytes()), outdir, req, Limits{})
	if err != nil || !reflect.DeepEqual(res, result) {
		t.Fatal("result round trip", res, err)
	}
	for name, want := range map[string][]byte{"workspace.tar": workspace, "session.blob": session} {
		got, err := os.ReadFile(filepath.Join(outdir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("result raw blob changed", name, err)
		}
	}
}
func TestBundleInitialAndFreshDestination(t *testing.T) {
	req := bundleTestRequest()
	raw := bundleWrite(t, req, nil)
	parent := t.TempDir()
	dest := filepath.Join(parent, "input")
	if err := os.Mkdir(dest, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dest, "keep")
	os.WriteFile(sentinel, []byte("unchanged"), 0600)
	if _, _, err := DecodeRequest(t.Context(), bytes.NewReader(raw), dest, Limits{}); err == nil {
		t.Fatal("existing destination accepted")
	}
	got, err := os.ReadFile(sentinel)
	if err != nil || string(got) != "unchanged" {
		t.Fatal("existing destination overwritten")
	}
	fresh := filepath.Join(parent, "new")
	gotReq, files, err := DecodeRequest(t.Context(), bytes.NewReader(raw), fresh, Limits{})
	if err != nil || gotReq.Spec != req.Spec || files.Workspace != "" || files.Session != "" || len(files.Skills) != 0 {
		t.Fatal(gotReq, files, err)
	}
}
func TestBundleRejectsMetadataAndStructureBeforePublishing(t *testing.T) {
	req := bundleTestRequest()
	meta, _ := json.Marshal(req)
	good := bundleWrite(t, req, nil)
	cases := map[string][]byte{
		"unknown":          bundleCustom(t, bytes.Replace(meta, []byte(`"version":`), []byte(`"unknown":1,"version":`), 1), nil, nil),
		"duplicate":        bundleCustom(t, bytes.Replace(meta, []byte(`"version":`), []byte(`"version":"native-guest.v1","version":`), 1), nil, nil),
		"case-alias":       bundleCustom(t, bytes.Replace(meta, []byte(`"version":`), []byte(`"Version":`), 1), nil, nil),
		"unicode-alias":    bundleCustom(t, bytes.Replace(meta, []byte(`"spec":`), []byte(`"ſpec":`), 1), nil, nil),
		"null":             bundleCustom(t, bytes.Replace(meta, []byte(`"input":"input preserved"`), []byte(`"input":null`), 1), nil, nil),
		"trailing-json":    bundleCustom(t, append(bytes.Clone(meta), []byte(` {}`)...), nil, nil),
		"oversized-json":   bundleCustom(t, bytes.Repeat([]byte(" "), MaxRequestJSONBytes+1), nil, nil),
		"extra":            bundleCustom(t, meta, map[string][]byte{"extra": []byte("x")}, []string{"extra"}),
		"trailing-data":    append(bytes.Clone(good), []byte("x")...),
		"trailing-zeros":   append(bytes.Clone(good), make([]byte, 512)...),
		"concatenated-tar": append(bytes.Clone(good), good...),
		"truncated":        good[:len(good)-1],
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "input")
			if _, _, err := DecodeRequest(t.Context(), bytes.NewReader(raw), dest, Limits{}); err == nil {
				t.Fatal("invalid envelope accepted")
			}
			if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("partial destination escaped", err)
			}
		})
	}
}
func TestBundleRejectsBlobTamperingAndSlots(t *testing.T) {
	req := bundleTestRequest()
	body := []byte("selected skill body")
	req.Skills = []Skill{{Name: "rules", Blob: bundleTestMeta(body, "skill-tar.v1", "application/x-tar")}}
	meta, _ := json.Marshal(req)
	for name, raw := range map[string][]byte{
		"checksum":       bundleCustom(t, meta, map[string][]byte{"skills/0.tar": []byte("changed skill bytes")}, []string{"skills/0.tar"}),
		"wrong-slot":     bundleCustom(t, meta, map[string][]byte{"skills/1.tar": body}, []string{"skills/1.tar"}),
		"missing-slot":   bundleCustom(t, meta, nil, nil),
		"duplicate-slot": bundleCustom(t, meta, map[string][]byte{"skills/0.tar": body}, []string{"skills/0.tar", "skills/0.tar"}),
		"path":           bundleCustom(t, meta, map[string][]byte{"../skills/0.tar": body}, []string{"../skills/0.tar"}),
	} {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "input")
			if _, _, err := DecodeRequest(t.Context(), bytes.NewReader(raw), dest, Limits{}); err == nil {
				t.Fatal("invalid blob accepted")
			}
			if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("partial input published")
			}
		})
	}
	var out bytes.Buffer
	if err := WriteRequest(t.Context(), &out, req, bundleOpen(map[string][]byte{"skills/0.tar": []byte("changed skill bytes")}), Limits{}); err == nil {
		t.Fatal("writer accepted mutable source bytes")
	}
}
func TestBundleRejectsExtensionsWithoutReadingTheirBodies(t *testing.T) {
	for _, kind := range []byte{tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink, tar.TypeGNUSparse, tar.TypeSymlink, tar.TypeLink, tar.TypeDir} {
		t.Run(string(kind), func(t *testing.T) {
			var out bytes.Buffer
			tw := tar.NewWriter(&out)
			if err := tw.WriteHeader(&tar.Header{Name: "request.json", Size: MaxBundleBytes, Mode: 0600, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0)}); err != nil {
				t.Fatal(err)
			}
			raw := bytes.Clone(out.Bytes()[:512])
			raw[156] = kind
			for i := 148; i < 156; i++ {
				raw[i] = ' '
			}
			sum := 0
			for _, b := range raw {
				sum += int(b)
			}
			copy(raw[148:156], []byte(fmtOctal(sum)))
			dest := filepath.Join(t.TempDir(), "input")
			if _, _, err := DecodeRequest(t.Context(), bytes.NewReader(raw), dest, Limits{}); err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal("extension was read instead of rejected at its header", err)
			}
		})
	}
}
func fmtOctal(n int) string {
	const oct = "01234567"
	var b [8]byte
	for i := 0; i < 6; i++ {
		b[i] = '0'
	}
	b[6] = 0
	b[7] = ' '
	for i := 5; n > 0 && i >= 0; i-- {
		b[i] = oct[n&7]
		n >>= 3
	}
	return string(b[:])
}
func TestBundleLimitsAndCancellation(t *testing.T) {
	req := bundleTestRequest()
	raw := bundleWrite(t, req, nil)
	for _, limits := range []Limits{{MaxInputBytes: -1}, {MaxOutputBytes: -1}, {MaxInputBytes: MaxBundleBytes + 1}, {MaxOutputBytes: MaxBundleBytes + 1}, {MaxInputBytes: int64(len(raw) - 1)}} {
		var out bytes.Buffer
		if err := WriteRequest(t.Context(), &out, req, nil, limits); err == nil {
			t.Fatal("invalid or exceeded limit accepted", limits)
		}
		dest := filepath.Join(t.TempDir(), "input")
		if _, _, err := DecodeRequest(t.Context(), bytes.NewReader(raw), dest, limits); err == nil {
			t.Fatal("decoder accepted invalid or exceeded limit", limits)
		}
		if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("limited decoder published input")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dest := filepath.Join(t.TempDir(), "input")
	if _, _, err := DecodeRequest(ctx, bytes.NewReader(raw), dest, Limits{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled input published")
	}
	var cancelled bytes.Buffer
	if err := WriteRequest(ctx, &cancelled, req, nil, Limits{}); !errors.Is(err, context.Canceled) || cancelled.Len() != 0 {
		t.Fatal("cancelled writer emitted output", cancelled.Len(), err)
	}
	for _, edit := range []func(*Request){func(r *Request) { r.ParentSessionID = "session" }, func(r *Request) { r.AdapterID = "../escape" }, func(r *Request) { r.Skills = []Skill{{Name: "rules"}, {Name: "rules"}} }, func(r *Request) { r.Spec.Output = "../report" }} {
		copy := req
		edit(&copy)
		if err := copy.Validate(); err == nil {
			t.Fatal("invalid request accepted", copy)
		}
	}
}
func TestBundleResultBindingAndTextLimits(t *testing.T) {
	req := bundleTestRequest()
	blob := bundleTree(t, "data.txt", []byte("raw original file"))
	meta := bundleTestMeta(blob, "workspace-tar.v1", "application/x-tar")
	session := bundleTestMeta(blob, "fixture-session.v1", "application/x-tar")
	req.ParentSessionID = "original"
	req.Workspace = &meta
	req.Session = &session
	result := Result{Version: Protocol, AdapterID: req.AdapterID, AdapterVersion: req.AdapterVersion, RuntimeVersion: req.RuntimeVersion, SessionID: "original", Workspace: meta, Session: session}
	var out bytes.Buffer
	if err := WriteResult(t.Context(), &out, result, bundleOpen(map[string][]byte{"workspace.tar": blob, "session.blob": blob}), Limits{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadResult(t.Context(), bytes.NewReader(out.Bytes()), filepath.Join(t.TempDir(), "valid-result"), req, Limits{}); err != nil {
		t.Fatal("result binding positive control failed", err)
	}
	result.SessionID = "changed"
	out.Reset()
	if err := WriteResult(t.Context(), &out, result, bundleOpen(map[string][]byte{"workspace.tar": blob, "session.blob": blob}), Limits{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadResult(t.Context(), bytes.NewReader(out.Bytes()), filepath.Join(t.TempDir(), "result"), req, Limits{}); err == nil {
		t.Fatal("continuation changed session identity")
	}
	result.SessionID = "original"
	for name, edit := range map[string]func(*Result){
		"format": func(r *Result) { r.Session.Format = "fixture-session.v2" },
		"media":  func(r *Result) { r.Session.MediaType = "application/json" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := result
			edit(&changed)
			var encoded bytes.Buffer
			if err := WriteResult(t.Context(), &encoded, changed, bundleOpen(map[string][]byte{"workspace.tar": blob, "session.blob": blob}), Limits{}); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "result")
			if _, err := ReadResult(t.Context(), bytes.NewReader(encoded.Bytes()), dest, req, Limits{}); err == nil {
				t.Fatal("continuation changed native format")
			}
			if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected native format published output")
			}
		})
	}
	result.Text = strings.Repeat("x", MaxTextBytes+1)
	if err := result.Validate(); err == nil {
		t.Fatal("oversized text accepted")
	}
	result.Text = strings.Repeat("<", MaxTextBytes)
	out.Reset()
	if err := WriteResult(t.Context(), &out, result, bundleOpen(map[string][]byte{"workspace.tar": blob, "session.blob": blob}), Limits{}); err != nil {
		t.Fatal("bounded text JSON expansion was not supported", err)
	}
}
