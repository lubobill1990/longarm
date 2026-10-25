//go:build windows

package shim

// privateUmask: Windows has no umask; new files inherit the ACL of their
// directory (profiles and per-user data dirs are private).
func privateUmask() {}
