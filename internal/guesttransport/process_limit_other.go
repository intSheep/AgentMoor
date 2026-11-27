//go:build !linux

package guesttransport

import "fmt"

func platformProcessLimit() error {
	return fmt.Errorf("guest process hard limit requires Linux")
}
