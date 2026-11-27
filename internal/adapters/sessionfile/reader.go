// Package sessionfile reads identifiers without normalizing native transcripts.
package sessionfile

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"

	"github.com/google/uuid"

	"github.com/intSheep/AgentMoor/internal/snapshot"
)

func UUID(s string) bool { _, err := uuid.Parse(s); return err == nil && len(s) == 36 }

var OpenCodeID = regexp.MustCompile("^ses_[A-Za-z0-9]+$")

func Header(root *os.Root, name string, dst any) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("invalid transcript type at %q", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = json.NewDecoder(io.LimitReader(f, 32<<20)).Decode(dst); err != nil {
		return fmt.Errorf("unsupported session header in %q: %w", name, err)
	}
	return nil
}

func Bundle(root *os.Root, main, id string) ([]snapshot.Entry, error) {
	files := []snapshot.Entry{{Source: main, Name: main}}
	info, err := root.Lstat(id)
	if os.IsNotExist(err) {
		return files, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode().Type() != fs.ModeDir {
		return nil, fmt.Errorf("invalid session sidecar directory %q", id)
	}
	return append(files, snapshot.Entry{Source: id, Name: id}), nil
}

// Fields inspects selected top-level fields without buffering an entire JSON
// export. An optional array field only needs its opening bracket to identify
// the native envelope. Message bodies are copied verbatim by the snapshotter.
func Fields(root *os.Root, name string, fields map[string]any, array string) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("invalid transcript type at %q", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return fmt.Errorf("invalid session envelope at %q", name)
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		k, ok := key.(string)
		if !ok {
			return fmt.Errorf("invalid field in %q", name)
		}
		if dest, ok := fields[k]; ok {
			if err = d.Decode(dest); err != nil {
				return err
			}
			delete(fields, k)
		} else {
			first, err := d.Token()
			if err != nil {
				return err
			}
			if k == array && array != "" {
				if first != json.Delim('[') {
					return fmt.Errorf("invalid session message list")
				}
				array = ""
			}
			if len(fields) == 0 && array == "" {
				return nil
			}
			depth := 0
			if first == json.Delim('[') || first == json.Delim('{') {
				depth++
			}
			for depth > 0 {
				v, err := d.Token()
				if err != nil {
					return err
				}
				switch v {
				case json.Delim('['), json.Delim('{'):
					depth++
				case json.Delim(']'), json.Delim('}'):
					depth--
				}
			}
		}
		if len(fields) == 0 && array == "" {
			return nil
		}
	}
	return fmt.Errorf("missing native session fields in %q", name)
}
