//go:build !windows

package e2e

import "syscall"

// noConsoleAttr starts a process in a new session without a controlling
// terminal, like systemd/launchd start a service.
func noConsoleAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
