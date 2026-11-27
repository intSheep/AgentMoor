package guesttransport

import (
	"errors"
	"syscall"
	"testing"
)

func TestProcessLimitMustBeInstalledAndUnraisable(t *testing.T) {
	for _, mode := range []string{"ok", "set-fails", "get-fails", "ignored", "raisable", "wrong-denial", "changed"} {
		t.Run(mode, func(t *testing.T) {
			var soft, hard uint64
			reads := 0
			get := func() (uint64, uint64, error) {
				reads++
				if mode == "get-fails" {
					return 0, 0, errors.New("synthetic read failure")
				}
				if mode == "changed" && reads == 2 {
					return soft, hard + 1, nil
				}
				return soft, hard, nil
			}
			set := func(cur, max uint64) error {
				if mode == "set-fails" {
					return syscall.EPERM
				}
				if cur > GuestProcessLimit && mode != "raisable" {
					if mode == "wrong-denial" {
						return syscall.EINVAL
					}
					return syscall.EPERM
				}
				if mode != "ignored" {
					soft, hard = cur, max
				}
				return nil
			}
			err := restrictProcesses(get, set)
			if (err == nil) != (mode == "ok") {
				t.Fatalf("limit validation result: %v", err)
			}
		})
	}
}
