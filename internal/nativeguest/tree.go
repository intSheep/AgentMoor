package nativeguest

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"agentmoor/internal/asset"
)

const MaxTreeEntries = 65536
const maxGuestPAXBytes = 64 << 10

// ValidateTree checks expansion and filesystem semantics before a native or
// trusted host restore. Ordinary PAX preserves original long paths. Sparse
// archives and GNU extensions are refused before archive/tar can allocate or
// expand their maps. This validation never rewrites the original tar bytes.
func ValidateTree(ctx context.Context, file string, maxBytes int64) error {
	if maxBytes < 1 || maxBytes > MaxBundleBytes {
		return fmt.Errorf("invalid guest tree limit")
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return fmt.Errorf("guest tree exceeds its wire limit")
	}
	if err = preflightTreeHeaders(ctx, f, maxBytes); err != nil {
		return err
	}
	if _, err = f.Seek(0, 0); err != nil {
		return err
	}
	reader := tar.NewReader(contextReader{ctx, f})
	seen := make(map[string]bool)
	remaining := maxBytes
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid guest tree archive")
		}
		name := header.Name
		if (name != "." && !asset.SafePath(name)) || seen[name] || len(seen) >= MaxTreeEntries || header.Mode < 0 || header.Mode > 0777 || header.Uid < 0 || header.Gid < 0 {
			return fmt.Errorf("invalid or duplicate guest tree entry")
		}
		seen[name] = true
		switch header.Typeflag {
		case tar.TypeReg:
			if name == "." || header.Size < 0 || header.Size > remaining {
				return fmt.Errorf("guest tree exceeds its expanded limit")
			}
			remaining -= header.Size
			if _, err = io.Copy(io.Discard, reader); err != nil {
				return err
			}
		case tar.TypeDir:
		case tar.TypeSymlink:
			if !guestSafeLink(name, header.Linkname) {
				return fmt.Errorf("guest tree link escapes its root")
			}
		default:
			return fmt.Errorf("unsupported guest tree entry type")
		}
	}
}

func guestSafeLink(name, target string) bool {
	if target == "" || path.IsAbs(target) || strings.ContainsAny(target, "\\:\x00\r\n") {
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	return resolved == "." || asset.SafePath(resolved)
}

// This first pass intentionally does not ask archive/tar to decode sparse
// maps or extension bodies. It only admits bounded normal/PAX structures.
func preflightTreeHeaders(ctx context.Context, file *os.File, maxBytes int64) error {
	reader := contextReader{ctx, file}
	var nextSize *int64
	pendingExtension := false
	var block [512]byte
	for {
		if _, err := io.ReadFull(reader, block[:]); err != nil {
			return fmt.Errorf("truncated guest tree header")
		}
		if allZero(block[:]) {
			if pendingExtension {
				return fmt.Errorf("guest tree ends after an extension header")
			}
			if _, err := io.ReadFull(reader, block[:]); err != nil || !allZero(block[:]) {
				return fmt.Errorf("invalid guest tree end marker")
			}
			// Native source tar files may include normal zero record padding.
			// Concatenated archives or nonzero trailing data are not accepted.
			for {
				n, err := reader.Read(block[:])
				if !allZero(block[:n]) {
					return fmt.Errorf("trailing data after guest tree")
				}
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
			}
		}
		size, err := treeOctal(block[124:136])
		if err != nil || size < 0 || size > maxBytes {
			return fmt.Errorf("guest tree entry exceeds its limit")
		}
		stored, err := treeOctal(block[148:156])
		if err != nil {
			return fmt.Errorf("invalid guest tree checksum")
		}
		var checksum int64
		for i, value := range block {
			if i >= 148 && i < 156 {
				checksum += 32
			} else {
				checksum += int64(value)
			}
		}
		if checksum != stored {
			return fmt.Errorf("invalid guest tree checksum")
		}
		switch block[156] {
		case tar.TypeXHeader:
			if pendingExtension {
				return fmt.Errorf("guest tree has stacked extension headers")
			}
			if size > maxGuestPAXBytes {
				return fmt.Errorf("guest PAX header exceeds its limit")
			}
			raw := make([]byte, size)
			if _, err = io.ReadFull(reader, raw); err != nil {
				return fmt.Errorf("truncated guest PAX header")
			}
			value, err := guestPAXSize(raw, maxBytes)
			if err != nil {
				return err
			}
			nextSize = value
			pendingExtension = true
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
			if nextSize != nil {
				size = *nextSize
				nextSize = nil
			}
			pendingExtension = false
			if block[156] != tar.TypeReg && size != 0 {
				return fmt.Errorf("guest directory or link has a data body")
			}
			if _, err = io.CopyN(io.Discard, reader, size); err != nil {
				return fmt.Errorf("truncated guest tree body")
			}
		default:
			return fmt.Errorf("unsupported guest tree extension or entry type")
		}
		padding := (512 - size%512) % 512
		if _, err = io.CopyN(io.Discard, reader, padding); err != nil {
			return fmt.Errorf("truncated guest tree padding")
		}
	}
}

func treeOctal(raw []byte) (int64, error) {
	text := strings.Trim(string(raw), " \x00")
	if text == "" {
		return 0, nil
	}
	for _, value := range text {
		if value < '0' || value > '7' {
			return 0, fmt.Errorf("unsupported guest tar number")
		}
	}
	return strconv.ParseInt(text, 8, 64)
}

func guestPAXSize(raw []byte, maxBytes int64) (*int64, error) {
	var size *int64
	count := 0
	for len(raw) > 0 {
		space := bytes.IndexByte(raw, ' ')
		if space < 1 || count >= 128 {
			return nil, fmt.Errorf("invalid or excessive guest PAX records")
		}
		n, err := strconv.Atoi(string(raw[:space]))
		if err != nil || n <= space+2 || n > len(raw) || raw[n-1] != '\n' {
			return nil, fmt.Errorf("invalid guest PAX record")
		}
		record := raw[space+1 : n-1]
		equals := bytes.IndexByte(record, '=')
		if equals < 1 {
			return nil, fmt.Errorf("invalid guest PAX member")
		}
		key, value := string(record[:equals]), string(record[equals+1:])
		if strings.HasPrefix(key, "GNU.sparse.") || strings.Contains(strings.ToLower(key), "sparse") {
			return nil, fmt.Errorf("sparse guest trees are not supported")
		}
		if key == "size" {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 0 || parsed > maxBytes {
				return nil, fmt.Errorf("guest PAX expansion exceeds its limit")
			}
			size = &parsed
		}
		raw = raw[n:]
		count++
	}
	return size, nil
}

func allZero(raw []byte) bool {
	for _, value := range raw {
		if value != 0 {
			return false
		}
	}
	return true
}
