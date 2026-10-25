//go:build !windows

package longarmd

import "syscall"

// privateUmask: files longarmd creates (per-user isolation, as root) are
// root's only unless made accessible explicitly.
func privateUmask() { syscall.Umask(0o077) }
