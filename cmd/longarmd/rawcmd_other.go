//go:build !windows

package main

import (
	"os"
	"strings"
)

// rawCommandLine: the MSI is Windows-only; elsewhere the arguments joined.
func rawCommandLine() string { return strings.Join(os.Args, " ") }
