//go:build unix

package main

import (
	"os"
	"strconv"
	"syscall"
)

// execve replaces this process (the container's PID 1 becomes argv[0], which
// then receives the runtime's SIGTERM directly).
func execve(path string, argv []string) error {
	return syscall.Exec(path, argv, syscall.Environ())
}

const sigKill = int(syscall.SIGKILL)

// exitSignal: the signal that ended a process, if one did.
func exitSignal(ps *os.ProcessState) (int, bool) {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return int(ws.Signal()), true
	}
	return 0, false
}

// signalName: SIGKILL rather than Go's "killed".
func signalName(sig int) string {
	names := map[syscall.Signal]string{
		syscall.SIGKILL: "SIGKILL", syscall.SIGTERM: "SIGTERM", syscall.SIGINT: "SIGINT",
		syscall.SIGSEGV: "SIGSEGV", syscall.SIGABRT: "SIGABRT", syscall.SIGBUS: "SIGBUS",
		syscall.SIGQUIT: "SIGQUIT", syscall.SIGHUP: "SIGHUP",
	}
	if n, ok := names[syscall.Signal(sig)]; ok {
		return n
	}
	return "signal " + strconv.Itoa(sig) + " (" + syscall.Signal(sig).String() + ")"
}
