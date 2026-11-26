//go:build !windows && !linux && !darwin

package main

import (
	"fmt"
	"os"
)

// pattern: the remote desktop test's picture (pattern_windows.go).
func pattern(string, int) {
	fmt.Fprintln(os.Stderr, "pattern: not on this OS yet")
	os.Exit(2)
}
