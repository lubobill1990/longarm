//go:build unix

package main

import "syscall"

// execve replaces this process (the container's PID 1 becomes argv[0], which
// then receives the runtime's SIGTERM directly).
func execve(path string, argv []string) error {
	return syscall.Exec(path, argv, syscall.Environ())
}
