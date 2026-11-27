package main

import (
	"context"
	"fmt"
	"io"

	"agentmoor/internal/adapter"
	"agentmoor/internal/nativeguest"
)

// guestCommand does not read Worker configuration, platform credentials or
// allocation state. The operator's sandbox profile chooses its executable.
func guestCommand(ctx context.Context, args []string, in io.Reader, out io.Writer, registry *adapter.Registry) error {
	flags := commandFlags(args[0])
	id := flags.String("adapter", "", "operator-assigned native adapter")
	executable := flags.String("runtime-cli", "", "operator-assigned absolute native executable")
	root := flags.String("root", "", "new guest-only execution directory")
	inputLimit := flags.Int64("max-input-bytes", nativeguest.MaxBundleBytes, "bounded input envelope limit")
	outputLimit := flags.Int64("max-output-bytes", nativeguest.MaxBundleBytes, "bounded output envelope limit")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *id == "" || *executable == "" || *root == "" || flags.NArg() != 0 {
		return fmt.Errorf("guest-execute requires --adapter ID --runtime-cli ABS --root NEW_DIR")
	}
	a, err := registry.Lookup(*id)
	if err != nil {
		return err
	}
	return nativeguest.Execute(ctx, a, *executable, *root, in, out,
		nativeguest.Limits{MaxInputBytes: *inputLimit, MaxOutputBytes: *outputLimit})
}
