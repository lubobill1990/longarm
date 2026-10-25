//go:build windows

package shim

import "os"

func signalExitCode(*os.ProcessState) int { return -1 }
