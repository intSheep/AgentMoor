package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/intSheep/AgentMoor/tests/kubernetes/faultproxy"
)

func main() {
	listen := flag.String("listen", ":7443", "Worker HTTPS listener")
	upstream := flag.String("upstream", "https://agentmoor:7401", "trusted upstream HTTPS origin")
	ca := flag.String("ca", "", "upstream CA PEM")
	cert := flag.String("cert", "", "downstream server certificate PEM")
	key := flag.String("key", "", "downstream private key PEM")
	flag.Parse()
	if flag.NArg() != 0 || *ca == "" || *cert == "" || *key == "" {
		fmt.Fprintln(os.Stderr, "require --ca, --cert, --key and no positional arguments")
		os.Exit(2)
	}
	if err := run(*listen, *upstream, *ca, *cert, *key); err != nil {
		// Avoid printing configured URLs, credentials, keys or request content.
		fmt.Fprintln(os.Stderr, "fault proxy stopped:", err)
		os.Exit(1)
	}
}

func run(listen, upstream, ca, cert, key string) error {
	pem, err := os.ReadFile(ca)
	if err != nil {
		return fmt.Errorf("cannot read upstream CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return fmt.Errorf("invalid upstream CA")
	}
	p, err := faultproxy.New(upstream, roots)
	if err != nil {
		return err
	}
	defer p.Close()
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return fmt.Errorf("invalid downstream certificate or key")
	}
	admin, err := net.Listen("tcp", "127.0.0.1:7444")
	if err != nil {
		return fmt.Errorf("cannot bind loopback admin listener")
	}
	defer admin.Close()
	worker, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("cannot bind Worker listener")
	}
	defer worker.Close()
	tlsConfig := faultproxy.HTTP1TLSConfig()
	tlsConfig.Certificates = []tls.Certificate{pair}
	workers := &http.Server{Handler: p, TLSConfig: tlsConfig, TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}, ReadHeaderTimeout: 10 * time.Second}
	management := &http.Server{Handler: p.Admin(), ReadHeaderTimeout: 5 * time.Second}
	failed := make(chan error, 2)
	go func() { failed <- workers.Serve(tls.NewListener(worker, tlsConfig)) }()
	go func() { failed <- management.Serve(admin) }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case <-failed:
		_ = workers.Close()
		_ = management.Close()
		return fmt.Errorf("HTTP listener failed")
	}
	// Close cancels held and streaming requests immediately; no publication
	// barrier can delay process shutdown or require an admin release.
	_ = workers.Close()
	_ = management.Close()
	return nil
}
