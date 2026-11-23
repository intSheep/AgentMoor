package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"agentmoor/internal/adapter"
	"agentmoor/internal/adapters/claudecode"
	"agentmoor/internal/adapters/codex"
	"agentmoor/internal/adapters/gemini"
	"agentmoor/internal/adapters/opencode"
)

const usage = `Usage:
  agentmoor adapters
  agentmoor detect <workspace>
  agentmoor inspect <adapter> <workspace> [absolute-runtime-executable]
  agentmoor skills <adapter> <workspace>
  agentmoor init --data DIR
  agentmoor serve --data DIR [--listen ADDRESS] [--tls-cert PEM --tls-key PEM] [--turn-jobs FILE]
  agentmoor issue --data DIR --tenant ID --project ID --runtime ID --adapter ID [--write] [--gc] [--submit] [--execute] [--ttl 1h]
  agentmoor run-task --config FILE [--wait 2m | --turn UUID]
  agentmoor sync|assets|status|gc --config FILE
  agentmoor watch --config FILE [--interval 1m] [--periodic-only]
  agentmoor restore --config FILE --kind KIND --id ID --to NEW_PATH [--version N]
  agentmoor restore-native --config FILE --id ID --to NEW_NATIVE_DATA_DIR [--version N]
  agentmoor restore-sessions --config FILE --to NEW_HISTORY_DIRECTORY
  agentmoor resolve --config FILE --expected-version N`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type match struct {
	Adapter  string   `json:"adapter"`
	Evidence []string `json:"evidence"`
}

func run(ctx context.Context, args []string, out io.Writer) error {
	registry, err := adapter.NewRegistry(claudecode.New(), codex.New(), gemini.New(), opencode.New())
	if err != nil {
		return err
	}
	jsonOut := json.NewEncoder(out)
	jsonOut.SetIndent("", "  ")
	if len(args) > 0 {
		switch args[0] {
		case "init", "issue", "serve":
			return platformCommand(ctx, args, out, registry)
		case "run-task", "sync", "watch", "restore", "restore-native", "restore-sessions", "assets", "status", "gc", "resolve":
			return workerCommand(ctx, args, out, registry)
		}
	}

	switch {
	case (len(args) == 3 || len(args) == 4) && args[0] == "inspect":
		a, err := registry.Lookup(args[1])
		if err != nil {
			return err
		}
		root, err := os.OpenRoot(args[2])
		if err != nil {
			return err
		}
		defer root.Close()
		exe := ""
		if len(args) == 4 {
			exe = args[3]
		}
		info, err := adapter.Inspect(ctx, a, root, exe)
		if err != nil {
			return err
		}
		return jsonOut.Encode(info)
	case len(args) == 1 && (args[0] == "-h" || args[0] == "--help"):
		_, err := fmt.Fprintln(out, usage)
		return err
	case len(args) == 1 && args[0] == "adapters":
		var descriptors []adapter.Descriptor
		for _, a := range registry.All() {
			descriptors = append(descriptors, a.Descriptor())
		}
		return jsonOut.Encode(descriptors)
	case len(args) == 2 && args[0] == "detect":
		root, err := os.OpenRoot(args[1])
		if err != nil {
			return err
		}
		defer root.Close()
		matches := []match{}
		for _, a := range registry.All() {
			evidence, err := a.Detect(ctx, root)
			if err != nil {
				return err
			}
			if len(evidence) > 0 {
				matches = append(matches, match{Adapter: a.Descriptor().ID, Evidence: evidence})
			}
		}
		return jsonOut.Encode(matches)
	case len(args) == 3 && args[0] == "skills":
		a, err := registry.Lookup(args[1])
		if err != nil {
			return err
		}
		reader, ok := a.(adapter.SkillDiscoverer)
		if !ok {
			return fmt.Errorf("adapter %q does not support skill discovery", args[1])
		}
		root, err := os.OpenRoot(args[2])
		if err != nil {
			return err
		}
		defer root.Close()
		skills, err := reader.DiscoverSkills(ctx, root)
		if err != nil {
			return err
		}
		return jsonOut.Encode(skills)
	default:
		return errors.New(usage)
	}
}
