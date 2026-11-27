// Package guesttransport transfers one frozen handoff through a capability-only
// TLS listener. It establishes no sandbox and never holds a platform credential.
package guesttransport

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/intSheep/AgentMoor/internal/nativeguest"
)

type Config struct {
	Gateway, CAFile, Capabilities string
	InputSHA256                   string
	InputSize                     int64
	Deadline                      time.Time
	MaxInputBytes, MaxOutputBytes int64
}

// Execute is invoked once, after the complete input matches its independently
// assigned digest. Its result must be a complete native-guest envelope.
type Execute func(context.Context, io.Reader, io.Writer) error

func (c Config) Validate() error {
	u, err := url.Parse(c.Gateway)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" ||
		u.Fragment != "" || u.Opaque != "" || u.Path != "" || u.RawPath != "" || u.ForceQuery || u.String() != c.Gateway || strings.HasSuffix(c.Gateway, "/") {
		return fmt.Errorf("guest gateway must be a fixed HTTPS origin")
	}
	if !filepath.IsAbs(c.CAFile) || !filepath.IsAbs(c.Capabilities) || c.InputSize < 1 ||
		c.MaxInputBytes < 1 || c.MaxInputBytes > nativeguest.MaxBundleBytes || c.InputSize > c.MaxInputBytes ||
		c.MaxOutputBytes < 1 || c.MaxOutputBytes > nativeguest.MaxBundleBytes {
		return fmt.Errorf("invalid guest transport paths or bounds")
	}
	b, err := hex.DecodeString(c.InputSHA256)
	if err != nil || len(b) != sha256.Size || hex.EncodeToString(b) != c.InputSHA256 || !time.Now().Before(c.Deadline) {
		return fmt.Errorf("invalid guest input digest or deadline")
	}
	return nil
}

// CapabilityFile permits Kubernetes' atomic DownwardAPI symlinks, but bounds
// the opened regular file and never includes its contents in an error.
func CapabilityFile(directory, purpose string) (string, error) {
	if !filepath.IsAbs(directory) || (purpose != "input" && purpose != "output" && purpose != "model") {
		return "", fmt.Errorf("invalid guest capability path")
	}
	raw, err := readRegular(filepath.Join(directory, purpose), 1024)
	if err != nil {
		return "", fmt.Errorf("guest capability file is unavailable")
	}
	token := strings.TrimSpace(string(raw))
	if len(token) != 48 || !strings.HasPrefix(token, "sbx1.") || strings.Trim(token[5:], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
		return "", fmt.Errorf("invalid guest capability")
	}
	return token, nil
}

func readRegular(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("guest configuration must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, fmt.Errorf("guest configuration exceeds its bound")
	}
	return raw, nil
}

func Run(ctx context.Context, c Config, temporary string, execute Execute) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(temporary) || execute == nil {
		return fmt.Errorf("guest transport requires a private absolute temporary directory and executor")
	}
	ctx, cancel := context.WithDeadline(ctx, c.Deadline)
	defer cancel()
	input, err := CapabilityFile(c.Capabilities, "input")
	if err != nil {
		return err
	}
	output, err := CapabilityFile(c.Capabilities, "output")
	if err != nil {
		return err
	}
	ca, err := readRegular(c.CAFile, 1<<20)
	if err != nil {
		return fmt.Errorf("guest gateway CA is unavailable or over bound")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return fmt.Errorf("invalid guest gateway CA")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
		MaxResponseHeaderBytes: 32 << 10}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	dir, err := os.MkdirTemp(temporary, "guest-transfer-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	request := filepath.Join(dir, "request.tar")
	if err = retry(ctx, func() (bool, error) { return download(ctx, client, c, input, request) }); err != nil {
		return err
	}
	source, err := os.Open(request)
	if err != nil {
		return err
	}
	defer source.Close()
	result, err := os.OpenFile(filepath.Join(dir, "result.tar"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer result.Close()
	hash := sha256.New()
	w := &boundedWriter{writer: io.MultiWriter(result, hash), left: c.MaxOutputBytes}
	if err = execute(ctx, source, w); err != nil {
		return fmt.Errorf("guest execution failed: %w", err)
	}
	size := c.MaxOutputBytes - w.left
	if size == 0 || w.failed {
		return fmt.Errorf("guest execution returned no complete bounded result")
	}
	if err = result.Sync(); err != nil {
		return err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	return retry(ctx, func() (bool, error) { return upload(ctx, client, c.Gateway, output, result, digest, size) })
}

type boundedWriter struct {
	writer io.Writer
	left   int64
	failed bool
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if w.failed || int64(len(p)) > w.left {
		w.failed = true
		return 0, fmt.Errorf("guest output exceeds transport bound")
	}
	n, err := w.writer.Write(p)
	w.left -= int64(n)
	if err != nil || n != len(p) {
		w.failed = true
	}
	return n, err
}

func download(ctx context.Context, client *http.Client, c Config, token, path string) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, "GET", c.Gateway+"/input", nil)
	if err != nil {
		return false, fmt.Errorf("invalid guest input request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return ctx.Err() == nil, fmt.Errorf("guest input transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return transientStatus(response.StatusCode), fmt.Errorf("guest input HTTP %d", response.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return false, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(response.Body, c.InputSize+1))
	closeErr := errors.Join(f.Sync(), f.Close())
	if copyErr != nil {
		return ctx.Err() == nil, fmt.Errorf("guest input stream failed")
	}
	if n != c.InputSize || hex.EncodeToString(h.Sum(nil)) != c.InputSHA256 {
		return false, fmt.Errorf("guest input differs from the sealed envelope")
	}
	return false, closeErr
}

func upload(ctx context.Context, client *http.Client, gateway, token string, result *os.File, digest string, size int64) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, "PUT", gateway+"/output", io.NewSectionReader(result, 0, size))
	if err != nil {
		return false, fmt.Errorf("invalid guest output request")
	}
	request.ContentLength = size
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/x-tar")
	request.Header.Set("X-Agentmoor-SHA256", digest)
	request.Header.Set("X-Agentmoor-Size", strconv.FormatInt(size, 10))
	response, err := client.Do(request)
	if err != nil {
		return ctx.Err() == nil, fmt.Errorf("guest output transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return transientStatus(response.StatusCode), fmt.Errorf("guest output HTTP %d", response.StatusCode)
	}
	return false, nil
}

func transientStatus(code int) bool {
	return code == 408 || code == 429 || code == 502 || code == 503 || code == 504 || code == 500
}

func retry(ctx context.Context, operation func() (bool, error)) error {
	for attempt := range 3 {
		if err := ctx.Err(); err != nil {
			return err
		}
		again, err := operation()
		if err == nil || !again || attempt == 2 {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("guest transfer retries exhausted")
}
