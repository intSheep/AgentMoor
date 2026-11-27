package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Bind every listener before serving either surface. A failed gateway must not
// leave a partially configured API running (or the reverse).
func serveHTTPServers(ctx context.Context, out io.Writer, certFile, keyFile string, servers ...*http.Server) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(servers) == 0 {
		return errors.New("missing HTTP server")
	}
	var certificate tls.Certificate
	if (certFile == "") != (keyFile == "") {
		return errors.New("TLS requires certificate and key")
	}
	if certFile != "" {
		var err error
		certificate, err = tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return err
		}
	}
	listeners := make([]net.Listener, 0, len(servers))
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for _, server := range servers {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			return err
		}
		listeners = append(listeners, listener)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	results := make(chan error, len(servers))
	for i, server := range servers {
		server.BaseContext = func(net.Listener) context.Context { return runCtx }
		if certFile != "" {
			if server.TLSConfig == nil {
				server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			} else {
				server.TLSConfig = server.TLSConfig.Clone()
			}
			server.TLSConfig.Certificates = []tls.Certificate{certificate}
		}
		fmt.Fprintln(out, "listening", listeners[i].Addr())
		go func(server *http.Server, listener net.Listener) {
			if certFile == "" {
				results <- server.Serve(listener)
			} else {
				results <- server.ServeTLS(listener, "", "")
			}
		}(server, listeners[i])
	}
	remaining := len(servers)
	var first error
	select {
	case <-ctx.Done():
	case first = <-results:
		remaining--
	}
	stop()
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(closeCtx); err != nil {
			_ = server.Close()
		}
	}
	for ; remaining > 0; remaining-- {
		if err := <-results; first == nil && !errors.Is(err, http.ErrServerClosed) {
			first = err
		}
	}
	if errors.Is(first, http.ErrServerClosed) {
		return nil
	}
	return first
}
