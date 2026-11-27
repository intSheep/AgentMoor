package guesttransport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const networkProbeTimeout = time.Second

type networkDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// CheckNetwork catches an absent/ineffective deny policy before any native
// command. The API canary must share the gateway IP and use a different port;
// the operator keeps both listeners/Service ports healthy. Only a TCP timeout
// counts as denial, never a refused port or an absent route. These finite
// canaries do not prove the policy for every possible destination.
func CheckNetwork(ctx context.Context, gateway, caFile, apiCanary, kubeCanary string) error {
	return checkNetwork(ctx, gateway, caFile, apiCanary, kubeCanary, &net.Dialer{})
}

func probeAddress(address string) (net.IP, int, error) {
	host, port, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	n, parseErr := strconv.Atoi(port)
	if err != nil || ip == nil || ip.IsUnspecified() || parseErr != nil || n < 1 || n > 65535 {
		return nil, 0, fmt.Errorf("invalid fixed network canary")
	}
	return ip, n, nil
}

func checkNetwork(ctx context.Context, gateway, caFile, apiCanary, kubeCanary string, dialer networkDialer) error {
	u, err := url.Parse(gateway)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != gateway {
		return fmt.Errorf("invalid fixed network gateway")
	}
	ip, apiPort, err := probeAddress(apiCanary)
	if err != nil {
		return err
	}
	if _, _, err = probeAddress(kubeCanary); err != nil {
		return err
	}
	gatewayPort := 443
	if u.Port() != "" {
		gatewayPort, err = strconv.Atoi(u.Port())
	}
	if err != nil || gatewayPort < 1 || gatewayPort > 65535 || gatewayPort == apiPort || strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("gateway and denied API must use separate ports")
	}
	ca, err := readRegular(caFile, 1<<20)
	roots := x509.NewCertPool()
	if err != nil || !roots.AppendCertsFromPEM(ca) {
		return fmt.Errorf("network probe gateway CA unavailable")
	}
	positive, cancel := context.WithTimeout(ctx, networkProbeTimeout)
	connection, err := dialer.DialContext(positive, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(gatewayPort)))
	if err != nil {
		cancel()
		return fmt.Errorf("required TLS gateway is unavailable")
	}
	secure := tls.Client(connection, &tls.Config{RootCAs: roots, ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
	err = secure.HandshakeContext(positive)
	// Do not let TLS close-notify replace the original bounded operation with
	// an unbounded close. A handshake probe sends no credentials or HTTP body.
	_ = connection.SetDeadline(time.Now())
	_ = connection.Close()
	cancel()
	if err != nil || ctx.Err() != nil {
		return fmt.Errorf("required TLS gateway failed network preflight")
	}
	for _, address := range []string{apiCanary, kubeCanary} {
		probe, stop := context.WithTimeout(ctx, networkProbeTimeout)
		connection, err = dialer.DialContext(probe, "tcp", address)
		stop()
		if connection != nil {
			connection.Close()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var timeout net.Error
		if err == nil || !errors.As(err, &timeout) || !timeout.Timeout() {
			return fmt.Errorf("network policy did not demonstrate the required TCP denial")
		}
	}
	return nil
}
