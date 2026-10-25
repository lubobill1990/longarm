//go:build !windows

package shim

import "syscall"

// privateUmask makes files the workload creates private to its account
// (per-user isolation: /tmp and the like are shared between accounts).
func privateUmask() { syscall.Umask(0o077) }
