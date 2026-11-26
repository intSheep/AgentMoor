package faultproxy

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestNativeCounterOnlyCountsExecAndPreservesArgumentsExitAndPID(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "native_count.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	counter, termination := filepath.Join(dir, "native-executions"), filepath.Join(dir, "termination-log")
	native, wrapper := filepath.Join(dir, "native-stub"), filepath.Join(dir, "counted-native")
	arguments, pid := filepath.Join(dir, "arguments"), filepath.Join(dir, "pid")
	// Only this private copy redirects the three production fixture paths.
	// The real wrapper has no environment-controlled executable or log paths.
	copy := string(source)
	for original, replacement := range map[string]string{
		"/run/task/native-executions": counter, "/dev/termination-log": termination, "/usr/local/bin/codex": native,
	} {
		if strings.Count(copy, original) != 1 {
			t.Fatal("wrapper changed its fixed path contract", original)
		}
		copy = strings.ReplaceAll(copy, original, replacement)
	}
	if err := os.WriteFile(wrapper, []byte(copy), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("#!/bin/sh\nprintf '%s' \"$$\" > \"$STUB_PID\"\n: > \"$STUB_ARGUMENTS\"\nfor arg do printf '%s\\000' \"$arg\" >> \"$STUB_ARGUMENTS\"; done\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	invoke := func(wantStatus int, args ...string) {
		t.Helper()
		command := exec.CommandContext(t.Context(), "/bin/sh", append([]string{wrapper}, args...)...)
		command.Env = []string{"PATH=/usr/bin:/bin", "STUB_PID=" + pid, "STUB_ARGUMENTS=" + arguments}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		err := command.Wait()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != wantStatus {
			t.Fatalf("wrapper changed exit status: %v", err)
		}
		if wantStatus == 23 {
			actualPID, err := os.ReadFile(pid)
			if err != nil || string(actualPID) != strconv.Itoa(command.Process.Pid) {
				t.Fatal("wrapper forked instead of replacing itself with native CLI", string(actualPID), err)
			}
			actual, err := os.ReadFile(arguments)
			var expected []byte
			for _, arg := range args {
				expected = append(append(expected, []byte(arg)...), 0)
			}
			if err != nil || !bytes.Equal(actual, expected) {
				t.Fatalf("wrapper changed native arguments: %q vs %q: %v", actual, expected, err)
			}
		}
	}
	for _, args := range [][]string{{"--version"}, {"resume", "--help"}, {}} {
		invoke(23, args...)
		for _, file := range []string{counter, termination} {
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatal("non-exec command wrote an execution count", file, err)
			}
		}
	}
	for index, args := range [][]string{{"exec", "a quoted argument", "-"}, {"exec", "resume", "session-id", "line one\nline two"}} {
		invoke(23, args...)
		count, err := os.ReadFile(counter)
		if err != nil || string(count) != strconv.Itoa(index+1)+"\n" {
			t.Fatalf("native invocation count changed: %q %v", count, err)
		}
		raw, err := os.ReadFile(termination)
		var record map[string]int
		if err != nil || json.Unmarshal(raw, &record) != nil || len(record) != 1 || record["native_exec_count"] != index+1 {
			t.Fatalf("invalid termination evidence: %q %v", raw, err)
		}
	}
	invoke(23, "--version")
	count, _ := os.ReadFile(counter)
	if string(count) != "2\n" {
		t.Fatal("version query incremented existing count", string(count))
	}
	if err := os.WriteFile(counter, []byte("invalid-counter\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pid); err != nil {
		t.Fatal(err)
	}
	invoke(1, "exec", "must-not-run")
	if _, err := os.Stat(pid); !os.IsNotExist(err) {
		t.Fatal("invalid counter still executed native CLI", err)
	}
	raw, err := os.ReadFile(termination)
	if err != nil || string(raw) != "{\"native_exec_count\":2}\n" {
		t.Fatal("invalid count overwrote previous termination evidence", string(raw), err)
	}
}
