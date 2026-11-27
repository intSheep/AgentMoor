//go:build linux

package guesttransport

import "golang.org/x/sys/unix"

func platformProcessLimit() error {
	return restrictProcesses(func() (uint64, uint64, error) {
		var value unix.Rlimit
		err := unix.Getrlimit(unix.RLIMIT_NPROC, &value)
		return value.Cur, value.Max, err
	}, func(soft, hard uint64) error {
		return unix.Setrlimit(unix.RLIMIT_NPROC, &unix.Rlimit{Cur: soft, Max: hard})
	})
}
