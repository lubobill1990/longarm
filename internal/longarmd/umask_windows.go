//go:build windows

package longarmd

// privateUmask: the data dir's ACL (SYSTEM and Administrators) is
// inherited by everything longarmd creates.
func privateUmask() {}
