package guesttransport

import (
	"context"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

type networkDialFunc func(context.Context, string, string) (net.Conn, error)

func (f networkDialFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func TestNetworkPreflightTLSAndDenialClassification(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("network probe sent HTTP data") }))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	ca := filepath.Join(t.TempDir(), "public-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	api := listener.Addr().String()
	kube := "127.0.0.1:23456" // Only the injected dialer sees this test address.
	for _, outcome := range []string{"drop-timeout", "refused", "no-route", "reachable", "cancelled", "wrong-ca"} {
		t.Run(outcome, func(t *testing.T) {
			probes := 0
			dial := networkDialFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
				if address == u.Host {
					return (&net.Dialer{}).DialContext(ctx, network, address)
				}
				probes++
				switch outcome {
				case "reachable":
					return (&net.Dialer{}).DialContext(ctx, network, api)
				case "refused":
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
				case "no-route":
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no route")}
				case "cancelled":
					return nil, context.Canceled
				default:
					// Classification unit only; real packet DROP is verified by the
					// separate controlled CNI/gVisor acceptance, not this fixture.
					return nil, &net.DNSError{Err: "controlled timeout", IsTimeout: true}
				}
			})
			selected := ca
			if outcome == "wrong-ca" {
				selected = filepath.Join(t.TempDir(), "not-a-ca")
				os.WriteFile(selected, []byte("invalid public CA fixture"), 0600)
			}
			err := checkNetwork(t.Context(), server.URL, selected, api, kube, dial)
			if (err == nil) != (outcome == "drop-timeout") {
				t.Fatal("network denial classification was unsafe", outcome, err)
			}
			if outcome == "drop-timeout" && probes != 2 {
				t.Fatal("did not check both denial canaries")
			}
		})
	}
}

func TestNetworkPreflightRefusesActualReachableControlPort(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = CheckNetwork(t.Context(), server.URL, ca, listener.Addr().String(), "127.0.0.1:23456"); err == nil {
		t.Fatal("reachable control API passed the production network guard")
	}
	listener.Close()
	if err = CheckNetwork(t.Context(), server.URL, ca, listener.Addr().String(), "127.0.0.1:23456"); err == nil {
		t.Fatal("closed control port was treated as policy enforcement")
	}
}

func TestNetworkPreflightRejectsInvalidAddressesBeforeDial(t *testing.T) {
	for _, pair := range [][2]string{{"host.example:7401", "10.0.0.1:443"}, {"0.0.0.0:7401", "10.0.0.1:443"}, {"10.0.0.2:0", "10.0.0.1:443"}, {"10.0.0.2:7443", "10.0.0.1:443"}, {"10.0.0.2:7401", "untrusted-name:443"}} {
		called := false
		dial := networkDialFunc(func(context.Context, string, string) (net.Conn, error) {
			called = true
			return nil, errors.New("must not dial")
		})
		if err := checkNetwork(t.Context(), "https://gateway.example:7443", "/unavailable-ca", pair[0], pair[1], dial); err == nil || called {
			t.Fatal("invalid network profile reached network")
		}
	}
}
