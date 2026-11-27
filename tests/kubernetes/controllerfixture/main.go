// controllerfixture is synthetic acceptance code, never a native agent. Its
// fixed guest-run shape exercises controller admission, capabilities and exit.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/intSheep/AgentMoor/internal/guesttransport"
	"github.com/intSheep/AgentMoor/internal/snapshot"
	"golang.org/x/sys/unix"
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
	if len(os.Args) == 4 && os.Args[1] == "synthetic-heartbeat" {
		return heartbeat(os.Args[2], os.Args[3])
	}
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
	var heartbeatInput struct {
		Mode     string            `json:"mode"`
		Canaries []heartbeatTarget `json:"canaries"`
	}
	_ = json.Unmarshal(body, &heartbeatInput)
	if string(body) == "synthetic-controller:heartbeat" || heartbeatInput.Mode == "synthetic-controller:heartbeat" {
		// A real, single child process in the actual guest, never a vendor CLI.
		// No user request selects this test-only image/mode.
		if err := guesttransport.LimitGuestProcesses(); err != nil {
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		targets, err := json.Marshal(heartbeatInput.Canaries)
		if err != nil {
			return err
		}
		if _, err = parseHeartbeatTargets(string(targets)); err != nil {
			return err
		}
		child := exec.CommandContext(ctx, executable, "synthetic-heartbeat", *deadline, string(targets))
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		return child.Run()
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

type heartbeatTarget struct {
	Name    string `json:"name"`
	IP      string `json:"ip"`
	Port    int    `json:"port"`
	Allowed bool   `json:"allowed"`
}
type heartbeatConnection struct {
	heartbeatTarget
	Reachable  bool   `json:"reachable"`
	Timeout    bool   `json:"timeout"`
	ErrorClass string `json:"error_class"`
}

func parseHeartbeatTargets(raw string) ([]heartbeatTarget, error) {
	var targets []heartbeatTarget
	if len(raw) > 2048 || json.Unmarshal([]byte(raw), &targets) != nil {
		return nil, fmt.Errorf("invalid heartbeat canaries")
	}
	if len(targets) == 0 {
		return nil, nil
	}
	expected := map[string]int{"gateway-tls": 7443, "main-api": 7401, "kubernetes-api": 443, "other-tenant": 8123}
	if len(targets) != len(expected) {
		return nil, fmt.Errorf("invalid heartbeat canary matrix")
	}
	seen := map[string]bool{}
	for _, target := range targets {
		ip := net.ParseIP(target.IP)
		if expected[target.Name] != target.Port || seen[target.Name] || ip == nil || ip.To4() == nil || !ip.IsPrivate() || target.Allowed != (target.Name == "gateway-tls") {
			return nil, fmt.Errorf("invalid controlled heartbeat target")
		}
		seen[target.Name] = true
	}
	return targets, nil
}

func heartbeat(deadline, rawTargets string) error {
	until, err := time.Parse(time.RFC3339Nano, deadline)
	if err != nil || !time.Now().Before(until) || time.Until(until) > 15*time.Minute {
		return fmt.Errorf("invalid fixture heartbeat deadline")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	targets, err := parseHeartbeatTargets(rawTargets)
	if err != nil {
		return err
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NPROC, &limit); err != nil || limit.Cur != 64 || limit.Max != 64 {
		return fmt.Errorf("fixture heartbeat process limit")
	}
	start := time.Now()
	for sequence := 1; time.Now().Before(until); sequence++ {
		connections := make([]heartbeatConnection, 0, len(targets))
		for _, target := range targets {
			result := heartbeatConnection{heartbeatTarget: target}
			connection, err := net.DialTimeout("tcp", net.JoinHostPort(target.IP, strconv.Itoa(target.Port)), 350*time.Millisecond)
			if err == nil {
				result.Reachable = true
				connection.Close()
			} else {
				var failure net.Error
				result.Timeout = errors.As(err, &failure) && failure.Timeout()
				result.ErrorClass = "unavailable"
				if result.Timeout {
					result.ErrorClass = "timeout"
				}
			}
			connections = append(connections, result)
		}
		body, err := json.Marshal(struct {
			Nonce        string                `json:"nonce"`
			PID          int                   `json:"pid"`
			Sequence     int                   `json:"sequence"`
			ElapsedNano  int64                 `json:"elapsed_nano"`
			WallUnixNano int64                 `json:"wall_unix_nano"`
			UID          int                   `json:"uid"`
			Canaries     []heartbeatConnection `json:"canaries"`
		}{hex.EncodeToString(nonce[:]), os.Getpid(), sequence, time.Since(start).Nanoseconds(), time.Now().UnixNano(), os.Getuid(), connections})
		if err != nil {
			return err
		}
		fmt.Println("AGENTMOOR_NODE_LOSS_HEARTBEAT " + string(body))
		time.Sleep(250 * time.Millisecond)
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
