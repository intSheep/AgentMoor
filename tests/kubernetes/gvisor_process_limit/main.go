// This fixture calls the production hard-limit helper before replacing itself
// with a bounded synthetic workload. It never executes a vendor CLI.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"agentmoor/internal/guesttransport"
)

func main() {
	if err := guesttransport.LimitGuestProcesses(); err != nil {
		fmt.Fprintln(os.Stderr, "production guest process limit rejected")
		os.Exit(1)
	}
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	path, err := exec.LookPath(os.Args[1])
	if err == nil {
		err = syscall.Exec(path, os.Args[1:], os.Environ())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "synthetic workload exec failed")
		os.Exit(3)
	}
}
