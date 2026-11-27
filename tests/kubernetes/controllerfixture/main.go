// controllerfixture is synthetic acceptance code, never a native agent. Its
// fixed guest-run shape exercises controller admission, capabilities and exit.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agentmoor/internal/snapshot"
)

func main() {
	if err := run(); err != nil {
		// Never emit flags, transport errors, request contents or credentials.
		fmt.Fprintln(os.Stderr, "AGENTMOOR_SYNTHETIC_CONTROLLER_FIXTURE_FAILED")
		os.Exit(1)
	}
	fmt.Println("AGENTMOOR_SYNTHETIC_CONTROLLER_FIXTURE_COMPLETE")
}

func run() error {
	if len(os.Args) < 2 || os.Args[1] != "guest-run" {
		return fmt.Errorf("fixture requires fixed guest-run")
	}
	f := flag.NewFlagSet("synthetic-controller-fixture", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	adapter := f.String("adapter", "", "")
	runtime := f.String("runtime-cli", "", "")
	root := f.String("root", "", "")
	gateway := f.String("gateway", "", "")
	ca := f.String("ca-file", "", "")
	caps := f.String("capabilities", "", "")
	sha := f.String("input-sha256", "", "")
	size := f.Int64("input-size", 0, "")
	deadline := f.String("deadline", "", "")
	model := f.String("model", "", "")
	if err := f.Parse(os.Args[2:]); err != nil || f.NArg() != 0 || *adapter != "codex" || *runtime != "/usr/local/bin/fixture-native-disabled" ||
		*model != "synthetic-controller-fixture" || *size < 1 || *size > 4096 || !filepath.IsAbs(*root) {
		return fmt.Errorf("invalid fixed fixture profile")
	}
	until, err := time.Parse(time.RFC3339Nano, *deadline)
	if err != nil || !time.Now().Before(until) {
		return fmt.Errorf("fixture deadline")
	}
	ctx, cancel := context.WithDeadline(context.Background(), until)
	defer cancel()
	if os.Getuid() != 65532 || os.Getgid() != 65532 {
		return fmt.Errorf("fixture user")
	}
	for _, key := range []string{"AGENTMOOR_TOKEN", "AGENTMOOR_TOKEN_FILE", "AGENTMOOR_POSTGRES_DSN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "OPENAI_API_KEY"} {
		if os.Getenv(key) != "" {
			return fmt.Errorf("fixture unexpected credential")
		}
	}
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); !os.IsNotExist(err) {
		return fmt.Errorf("fixture mounted SA")
	}
	if err := os.WriteFile("/etc/agentmoor/readonly-sentinel", []byte("unexpected"), 0600); !errors.Is(err, syscall.EROFS) {
		return fmt.Errorf("fixture writable root")
	}
	if err := os.Mkdir(*root, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(*root)
	// Use the production no-replace publication primitive on the actual work
	// mount; host tmpfs emptyDir alone previously failed this on gVisor Gofer.
	if err := snapshot.BuildDirectory(filepath.Join(*root, "atomic"), func(stage string) error { return os.WriteFile(filepath.Join(stage, "proof"), []byte("fixture"), 0600) }); err != nil {
		return err
	}
	input, err := capability(*caps, "input")
	if err != nil {
		return err
	}
	output, err := capability(*caps, "output")
	if err != nil {
		return err
	}
	if _, err := capability(*caps, "model"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*caps, "input"), []byte("tamper"), 0600); err == nil {
		return fmt.Errorf("fixture writable cap mount")
	}
	public, err := os.ReadFile(*ca)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(public) {
		return fmt.Errorf("fixture CA")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "GET", *gateway+"/input", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+input)
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(io.LimitReader(res.Body, *size+1))
	res.Body.Close()
	h := sha256.Sum256(body)
	if readErr != nil || res.StatusCode != 200 || int64(len(body)) != *size || hex.EncodeToString(h[:]) != *sha {
		return fmt.Errorf("fixture input")
	}
	if string(body) == "synthetic-controller:hold" {
		<-ctx.Done()
		return ctx.Err()
	}
	if string(body) != "synthetic-controller:success" {
		return fmt.Errorf("fixture mode")
	}
	result := []byte("synthetic-controller:sealed-result")
	h = sha256.Sum256(result)
	req, err = http.NewRequestWithContext(ctx, "PUT", *gateway+"/output", bytes.NewReader(result))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+output)
	req.Header.Set("X-Agentmoor-SHA256", hex.EncodeToString(h[:]))
	req.Header.Set("X-Agentmoor-Size", strconv.Itoa(len(result)))
	res, err = client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != 204 {
		return fmt.Errorf("fixture output")
	}
	return nil
}

func capability(directory, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(directory, name))
	token := strings.TrimSpace(string(b))
	if err != nil || len(token) != 48 || !strings.HasPrefix(token, "sbx1.") {
		return "", fmt.Errorf("fixture cap")
	}
	return token, nil
}
