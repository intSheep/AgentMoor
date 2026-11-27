package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"agentmoor/internal/adapter"
	"agentmoor/internal/adapters/codex"
	"agentmoor/internal/nativeguest"
	"agentmoor/internal/task"
)

func TestGuestCommandRejectsInvalidHandoffWithoutNativeInvocation(t *testing.T) {
	for _, condition := range []string{"bad-packet", "limit-exceeds-hard-ceiling", "forbidden-platform-environment", "missing-argument"} {
		t.Run(condition, func(t *testing.T) {
			for _, name := range []string{"AGENTMOOR_TOKEN", "AGENTMOOR_TOKEN_FILE", "AGENTMOOR_POSTGRES_DSN", "AGENTMOOR_S3_ENDPOINT", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
				t.Setenv(name, "")
			}
			a := codex.New()
			registry, err := adapter.NewRegistry(a)
			if err != nil {
				t.Fatal(err)
			}
			base := t.TempDir()
			executable, root := filepath.Join(base, "must-not-run"), filepath.Join(base, "guest")
			if err = os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'called' > \"$0.invocations\"\nexit 99\n"), 0700); err != nil {
				t.Fatal(err)
			}
			descriptor := a.Descriptor()
			request := nativeguest.Request{Version: nativeguest.Protocol, AdapterID: descriptor.ID, AdapterVersion: descriptor.Version, RuntimeVersion: descriptor.TestedRuntime,
				Spec: task.Spec{Instruction: "synthetic CLI boundary test", Output: "report.txt"}}
			var packet bytes.Buffer
			if err = nativeguest.WriteRequest(t.Context(), &packet, request,
				func(string) (io.ReadCloser, error) { t.Fatal("initial request has no source blob"); return nil, nil }, nativeguest.DefaultLimits()); err != nil {
				t.Fatal(err)
			}
			input := packet.Bytes()
			args := []string{"guest-execute", "--adapter", descriptor.ID, "--runtime-cli", executable, "--root", root}
			switch condition {
			case "bad-packet":
				input = []byte("not a guest request")
			case "limit-exceeds-hard-ceiling":
				args = append(args, "--max-output-bytes", "67108865")
			case "forbidden-platform-environment":
				t.Setenv("AGENTMOOR_TOKEN_FILE", filepath.Join(base, "private-worker-token"))
			case "missing-argument":
				args = []string{"guest-execute", "--adapter", descriptor.ID}
			}
			var output bytes.Buffer
			if err = guestCommand(t.Context(), args, bytes.NewReader(input), &output, registry); err == nil || output.Len() != 0 {
				t.Fatal("invalid guest command emitted a result", err, output.Len())
			}
			if _, err = os.Stat(executable + ".invocations"); !os.IsNotExist(err) {
				t.Fatal("invalid handoff invoked a native command", err)
			}
			if _, err = os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("invalid handoff left a guest execution root", err)
			}
		})
	}
}
