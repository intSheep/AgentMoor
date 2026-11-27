package nativeguest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"agentmoor/internal/snapshot"
)

type part struct {
	name string
	meta BlobMeta
}
type sourceOpener func(string) (io.ReadCloser, error)

func requestParts(r Request) []part {
	var parts []part
	if r.Workspace != nil {
		parts = append(parts, part{"workspace.tar", *r.Workspace})
	}
	if r.Session != nil {
		parts = append(parts, part{"parent.session", *r.Session})
	}
	for i, s := range r.Skills {
		parts = append(parts, part{"skills/" + strconv.Itoa(i) + ".tar", s.Blob})
	}
	return parts
}
func resultParts(r Result) []part {
	return []part{{"workspace.tar", r.Workspace}, {"session.blob", r.Session}}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type boundedWriter struct {
	ctx  context.Context
	dst  io.Writer
	left int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.left {
		return 0, fmt.Errorf("guest envelope exceeds limit")
	}
	n, err := w.dst.Write(p)
	w.left -= int64(n)
	return n, err
}

func strictJSON(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value func() error
	value = func() error {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		if token == nil {
			return fmt.Errorf("null guest metadata is not allowed")
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				tok, err := dec.Token()
				name, ok := tok.(string)
				if err != nil || !ok || name == "" || seen[name] {
					return fmt.Errorf("invalid guest metadata member")
				}
				for _, c := range name {
					if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
						return fmt.Errorf("noncanonical guest metadata member")
					}
				}
				seen[name] = true
				if err = value(); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim('}') {
				return fmt.Errorf("invalid guest metadata object")
			}
		case '[':
			for dec.More() {
				if err := value(); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim(']') {
				return fmt.Errorf("invalid guest metadata array")
			}
		default:
			return fmt.Errorf("invalid guest metadata delimiter")
		}
		return nil
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing guest metadata")
	}
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid guest metadata schema")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing guest metadata")
	}
	return nil
}

// Read only one fixed header before using archive/tar. In particular extension
// records must be rejected before tar.Reader could allocate their PAX/GNU data.
func nextHeader(r io.Reader) (*tar.Header, error) {
	var raw [512]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return nil, err
	}
	if bytes.Equal(raw[:], make([]byte, 512)) {
		var second [512]byte
		if _, err := io.ReadFull(r, second[:]); err != nil {
			return nil, err
		}
		if !bytes.Equal(second[:], make([]byte, 512)) {
			return nil, fmt.Errorf("invalid guest envelope terminator")
		}
		var extra [1]byte
		n, err := r.Read(extra[:])
		if n != 0 || err != io.EOF {
			return nil, fmt.Errorf("trailing guest envelope bytes")
		}
		return nil, io.EOF
	}
	if raw[156] != tar.TypeReg {
		return nil, fmt.Errorf("guest envelope only permits regular entries")
	}
	h, err := tar.NewReader(bytes.NewReader(raw[:])).Next()
	if err != nil {
		return nil, fmt.Errorf("invalid guest envelope header")
	}
	if h.Typeflag != tar.TypeReg || h.Format != tar.FormatUSTAR || len(h.PAXRecords) != 0 || len(h.Xattrs) != 0 || h.Linkname != "" || h.Mode != 0600 || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" || h.Size < 0 {
		return nil, fmt.Errorf("noncanonical guest envelope header")
	}
	return h, nil
}
func padding(r io.Reader, size int64) error {
	var raw [512]byte
	n := (512 - size%512) % 512
	if _, err := io.ReadFull(r, raw[:n]); err != nil {
		return err
	}
	if !bytes.Equal(raw[:n], make([]byte, n)) {
		return fmt.Errorf("invalid guest envelope padding")
	}
	return nil
}
func readMetadata(r io.Reader, name string, limit int64, dst any) ([]byte, int64, error) {
	h, err := nextHeader(r)
	if err != nil {
		return nil, 0, err
	}
	if h.Name != name || h.Size < 1 || h.Size > limit {
		return nil, 0, fmt.Errorf("invalid guest metadata entry")
	}
	raw := make([]byte, h.Size)
	if _, err = io.ReadFull(r, raw); err != nil {
		return nil, 0, err
	}
	if err = padding(r, h.Size); err != nil {
		return nil, 0, err
	}
	if err = strictJSON(raw, dst); err != nil {
		return nil, 0, err
	}
	return raw, 512 + ((h.Size+511)/512)*512, nil
}
func envelopeSize(metadataBytes int, parts []part) int64 {
	total := int64(512 + ((metadataBytes+511)/512)*512 + 1024)
	for _, p := range parts {
		total += 512 + ((p.meta.Size+511)/512)*512
	}
	return total
}
func writeEnvelope(ctx context.Context, dst io.Writer, name string, meta any, jsonLimit int, parts []part, open sourceOpener, limit int64) error {
	raw, err := json.Marshal(meta)
	if err != nil || len(raw) > jsonLimit {
		return fmt.Errorf("guest metadata exceeds limit")
	}
	if envelopeSize(len(raw), parts) > limit {
		return fmt.Errorf("guest envelope exceeds limit")
	}
	writer := &boundedWriter{ctx: ctx, dst: dst, left: limit}
	tw := tar.NewWriter(writer)
	header := func(name string, size int64) error {
		return tw.WriteHeader(&tar.Header{Name: name, Size: size, Mode: 0600, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0)})
	}
	if err = header(name, int64(len(raw))); err != nil {
		return err
	}
	if _, err = tw.Write(raw); err != nil {
		return err
	}
	for _, p := range parts {
		if open == nil {
			return fmt.Errorf("missing guest blob source")
		}
		source, err := open(p.name)
		if err != nil {
			return err
		}
		if source == nil {
			return fmt.Errorf("missing guest blob source")
		}
		err = func() error {
			if err := header(p.name, p.meta.Size); err != nil {
				return err
			}
			h := sha256.New()
			if _, err := io.CopyN(io.MultiWriter(tw, h), contextReader{ctx, source}, p.meta.Size); err != nil {
				return err
			}
			var extra [1]byte
			n, err := (contextReader{ctx, source}).Read(extra[:])
			if n != 0 || err != io.EOF {
				return fmt.Errorf("guest source size mismatch")
			}
			if hex.EncodeToString(h.Sum(nil)) != p.meta.SHA256 {
				return fmt.Errorf("guest source checksum mismatch")
			}
			return nil
		}()
		err = errors.Join(err, source.Close())
		if err != nil {
			return err
		}
	}
	return tw.Close()
}
func readParts(ctx context.Context, r io.Reader, root *os.Root, parts []part) error {
	for _, p := range parts {
		h, err := nextHeader(r)
		if err != nil {
			return err
		}
		if h.Name != p.name || h.Size != p.meta.Size {
			return fmt.Errorf("guest blob slot differs from metadata")
		}
		if err = root.MkdirAll(filepath.Dir(p.name), 0700); err != nil {
			return err
		}
		f, err := root.OpenFile(p.name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		digest := sha256.New()
		_, err = io.CopyN(io.MultiWriter(f, digest), contextReader{ctx, r}, h.Size)
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
		if hex.EncodeToString(digest.Sum(nil)) != p.meta.SHA256 {
			return fmt.Errorf("guest blob checksum mismatch")
		}
		if err = padding(r, h.Size); err != nil {
			return err
		}
	}
	if _, err := nextHeader(r); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("extra guest blob entry")
	}
	return nil
}

func validateTrees(ctx context.Context, dir string, parts []part, limit int64) error {
	for _, p := range parts {
		if p.meta.MediaType == "application/x-tar" {
			if err := ValidateTree(ctx, filepath.Join(dir, p.name), limit); err != nil {
				return err
			}
		}
	}
	return nil
}

func WriteRequest(ctx context.Context, dst io.Writer, req Request, open func(string) (io.ReadCloser, error), limits Limits) error {
	l, err := limits.normalized()
	if err != nil {
		return err
	}
	if err = req.Validate(); err != nil {
		return err
	}
	return writeEnvelope(ctx, dst, "request.json", req, MaxRequestJSONBytes, requestParts(req), open, l.MaxInputBytes)
}
func WriteResult(ctx context.Context, dst io.Writer, result Result, open func(string) (io.ReadCloser, error), limits Limits) error {
	l, err := limits.normalized()
	if err != nil {
		return err
	}
	if err = result.Validate(); err != nil {
		return err
	}
	return writeEnvelope(ctx, dst, "result.json", result, MaxResultJSONBytes, resultParts(result), open, l.MaxOutputBytes)
}
func DecodeRequest(ctx context.Context, src io.Reader, freshDir string, limits Limits) (Request, Files, error) {
	l, err := limits.normalized()
	if err != nil {
		return Request{}, Files{}, err
	}
	var req Request
	err = snapshot.BuildDirectory(freshDir, func(stage string) error {
		reader := &io.LimitedReader{R: contextReader{ctx, src}, N: l.MaxInputBytes + 1}
		raw, _, err := readMetadata(reader, "request.json", min(int64(MaxRequestJSONBytes), l.MaxInputBytes), &req)
		if err != nil {
			return err
		}
		if err = req.Validate(); err != nil {
			return err
		}
		parts := requestParts(req)
		if envelopeSize(len(raw), parts) > l.MaxInputBytes {
			return fmt.Errorf("guest envelope exceeds limit")
		}
		root, err := os.OpenRoot(stage)
		if err != nil {
			return err
		}
		defer root.Close()
		if err = root.WriteFile("request.json", raw, 0600); err != nil {
			return err
		}
		if err = readParts(ctx, reader, root, parts); err != nil {
			return err
		}
		if err = validateTrees(ctx, stage, parts, l.MaxInputBytes); err != nil {
			return err
		}
		if l.MaxInputBytes+1-reader.N > l.MaxInputBytes {
			return fmt.Errorf("guest envelope exceeds limit")
		}
		return nil
	})
	if err != nil {
		return Request{}, Files{}, err
	}
	files := Files{}
	if req.Workspace != nil {
		files.Workspace = filepath.Join(freshDir, "workspace.tar")
	}
	if req.Session != nil {
		files.Session = filepath.Join(freshDir, "parent.session")
	}
	for i := range req.Skills {
		files.Skills = append(files.Skills, filepath.Join(freshDir, "skills", strconv.Itoa(i)+".tar"))
	}
	return req, files, nil
}
func ReadResult(ctx context.Context, src io.Reader, freshDir string, expected Request, limits Limits) (Result, error) {
	l, err := limits.normalized()
	if err != nil {
		return Result{}, err
	}
	if err = expected.Validate(); err != nil {
		return Result{}, err
	}
	var result Result
	err = snapshot.BuildDirectory(freshDir, func(stage string) error {
		reader := &io.LimitedReader{R: contextReader{ctx, src}, N: l.MaxOutputBytes + 1}
		raw, _, err := readMetadata(reader, "result.json", min(int64(MaxResultJSONBytes), l.MaxOutputBytes), &result)
		if err != nil {
			return err
		}
		if err = result.Match(expected); err != nil {
			return err
		}
		parts := resultParts(result)
		if envelopeSize(len(raw), parts) > l.MaxOutputBytes {
			return fmt.Errorf("guest envelope exceeds limit")
		}
		root, err := os.OpenRoot(stage)
		if err != nil {
			return err
		}
		defer root.Close()
		if err = root.WriteFile("result.json", raw, 0600); err != nil {
			return err
		}
		if err = readParts(ctx, reader, root, parts); err != nil {
			return err
		}
		if err = validateTrees(ctx, stage, parts, l.MaxOutputBytes); err != nil {
			return err
		}
		if l.MaxOutputBytes+1-reader.N > l.MaxOutputBytes {
			return fmt.Errorf("guest envelope exceeds limit")
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}
