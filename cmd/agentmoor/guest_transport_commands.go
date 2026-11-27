package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"agentmoor/internal/adapter"
	"agentmoor/internal/guesttransport"
	"agentmoor/internal/nativeguest"
)

// guestRunCommand is the fixed sandbox PID1, not a Worker entrypoint. A Pod
// profile supplies every argument; no request or model reply selects them.
func guestRunCommand(ctx context.Context, args []string, registry *adapter.Registry) error {
	flags := commandFlags(args[0])
	id := flags.String("adapter", "", "operator-assigned adapter")
	executable := flags.String("runtime-cli", "", "absolute image-native executable")
	root := flags.String("root", "", "new private guest execution root")
	gateway := flags.String("gateway", "", "fixed capability-only HTTPS origin")
	ca := flags.String("ca-file", "", "public gateway CA in the image")
	caps := flags.String("capabilities", "", "read-only directory of per-run capability files")
	digest := flags.String("input-sha256", "", "persisted expected whole input digest")
	size := flags.Int64("input-size", 0, "persisted expected whole input size")
	deadline := flags.String("deadline", "", "original task deadline")
	model := flags.String("model", "", "operator-assigned model")
	tool := flags.String("file-tool", "", "optional operator-assigned image tool")
	inputLimit := flags.Int64("max-input-bytes", nativeguest.MaxBundleBytes, "bounded input envelope limit")
	outputLimit := flags.Int64("max-output-bytes", nativeguest.MaxBundleBytes, "bounded output envelope limit")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *id != "codex" || !filepath.IsAbs(*executable) || !filepath.IsAbs(*root) ||
		(*tool != "" && !filepath.IsAbs(*tool)) || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`).MatchString(*model) {
		return fmt.Errorf("guest-run requires a fixed Codex image profile, model and absolute paths")
	}
	until, err := time.Parse(time.RFC3339Nano, *deadline)
	if err != nil {
		return fmt.Errorf("invalid original guest deadline")
	}
	config := guesttransport.Config{Gateway: *gateway, CAFile: *ca, Capabilities: *caps,
		InputSHA256: *digest, InputSize: *size, Deadline: until, MaxInputBytes: *inputLimit, MaxOutputBytes: *outputLimit}
	if err = config.Validate(); err != nil {
		return err
	}
	a, err := registry.Lookup(*id)
	if err != nil {
		return err
	}
	modelCap, err := guesttransport.CapabilityFile(*caps, "model")
	if err != nil {
		return err
	}
	// These are the delegated per-run model capability and fixed gateway, never
	// an upstream provider key. Native launch still uses the adapter whitelist.
	for name, value := range map[string]string{"OPENAI_API_KEY": modelCap, "OPENAI_BASE_URL": *gateway + "/v1", "CODEX_MODEL": *model, "CODEX_FILE_TOOL": *tool} {
		old, present := os.LookupEnv(name)
		if err = os.Setenv(name, value); err != nil {
			return fmt.Errorf("cannot configure guest model environment")
		}
		defer func() {
			if present {
				_ = os.Setenv(name, old)
			} else {
				_ = os.Unsetenv(name)
			}
		}()
	}
	return guesttransport.Run(ctx, config, filepath.Dir(*root), func(runCtx context.Context, input io.Reader, output io.Writer) error {
		return nativeguest.Execute(runCtx, a, *executable, *root, input, output,
			nativeguest.Limits{MaxInputBytes: *inputLimit, MaxOutputBytes: *outputLimit})
	})
}
