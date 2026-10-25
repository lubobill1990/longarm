//go:build !windows

package shim

import (
	"os"
	"syscall"
)

// signalExitCode maps "killed by signal N" to the shell convention 128+N.
func signalExitCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return -1
}
