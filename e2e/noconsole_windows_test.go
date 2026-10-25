//go:build windows

package e2e

import "syscall"

// noConsoleAttr starts a process without a console, like the SCM starts a
// service.
func noConsoleAttr() *syscall.SysProcAttr {
	const detachedProcess = 0x00000008
	return &syscall.SysProcAttr{CreationFlags: detachedProcess}
}
