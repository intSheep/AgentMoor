// A synthetic launcher counter around the production preflight. No vendor CLI
// runs here: a rejected check must leave this fixture counter unchanged.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"strconv"
	"strings"
	"time"

	"agentmoor/internal/guesttransport"
)

func main() {
	gateway := flag.String("gateway", "", "fixed fixture TLS origin")
	ca := flag.String("ca", "", "fixture public CA")
	api := flag.String("api", "", "same-IP different-port denied canary")
	kube := flag.String("kube", "", "fixed Kubernetes service canary")
	counter := flag.String("counter", "/run/work/preflight-count", "synthetic execution counter")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := guesttransport.CheckNetwork(ctx, *gateway, *ca, *api, *kube)
	if err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"allowed": false, "production_check": true, "vendor_cli_executions": 0, "error": "network preflight rejected"})
		os.Exit(1)
	}
	count := 0
	if raw, err := os.ReadFile(*counter); err == nil {
		count, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	count++
	if err = os.WriteFile(*counter, []byte(strconv.Itoa(count)), 0600); err != nil {
		os.Exit(2)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"allowed": true, "production_check": true, "fixture_executions": count, "vendor_cli_executions": 0})
}
