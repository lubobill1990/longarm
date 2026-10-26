//go:build !linux

package loopguard

import "errors"

// Supported reports whether the platform has loopback isolation (Linux).
const Supported = false

// Apply is Linux only.
func Apply([]User) error {
	return errors.New("loopback isolation between users is implemented on Linux only")
}

// Remove has nothing to remove here.
func Remove() error { return nil }
