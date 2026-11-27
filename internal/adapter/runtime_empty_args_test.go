package adapter

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRunCLIWithoutArgumentsReturnsExecError(t *testing.T) {
	for name, run := range map[string]func(context.Context, string, string) error{
		"RunCLI": func(ctx context.Context, exe, dir string) error {
			return RunCLI(ctx, exe, dir, nil, io.Discard, 4096)
		},
		"RunCLIInput": func(ctx context.Context, exe, dir string) error {
			return RunCLIInput(ctx, exe, dir, nil, nil, io.Discard, 4096)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			err := run(t.Context(), filepath.Join(dir, "does-not-exist"), dir)
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("zero-argument failed command did not return its original error", err)
			}
		})
	}
}

func TestRunCLIWithoutArgumentsCanSucceed(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "synthetic-no-arguments")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := RunCLI(t.Context(), exe, dir, nil, io.Discard, 4096); err != nil {
		t.Fatal("zero arguments must remain valid for a successful command", err)
	}
	if err := RunCLIInput(t.Context(), exe, dir, nil, nil, io.Discard, 4096); err != nil {
		t.Fatal("zero-argument input helper changed successful behavior", err)
	}
}
