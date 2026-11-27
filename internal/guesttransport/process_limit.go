package guesttransport

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// GuestProcessLimit bounds virtual guest tasks. gVisor guest processes are not
// host processes, so the kubelet's host pids.max is a separate required bound.
// This includes threads belonging to the Go bootstrap, native CLI and its
// tools. A 32-task budget aborted the real fixed Codex flow at pthread_create.
const GuestProcessLimit uint64 = 64

// LimitGuestProcesses must run in the unprivileged Linux guest before any
// native command. The hard limit survives exec and cannot be raised by the
// native program. A privileged or unsupported environment fails closed.
func LimitGuestProcesses() error {
	if os.Geteuid() == 0 {
		return fmt.Errorf("guest process limit requires an unprivileged UID")
	}
	return platformProcessLimit()
}

func restrictProcesses(get func() (uint64, uint64, error), set func(uint64, uint64) error) error {
	if err := set(GuestProcessLimit, GuestProcessLimit); err != nil {
		return fmt.Errorf("set guest process hard limit: %w", err)
	}
	soft, hard, err := get()
	if err != nil || soft != GuestProcessLimit || hard != GuestProcessLimit {
		return fmt.Errorf("guest process hard limit was not installed")
	}
	// Verify that the current unprivileged process cannot restore a larger
	// hard limit. Both values are valid; only a permissions denial is proof.
	if err := set(GuestProcessLimit+1, GuestProcessLimit+1); !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("guest process hard limit can be raised or was not verified")
	}
	soft, hard, err = get()
	if err != nil || soft != GuestProcessLimit || hard != GuestProcessLimit {
		return fmt.Errorf("guest process hard limit changed during verification")
	}
	return nil
}
