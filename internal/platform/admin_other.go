//go:build !windows

package platform

import "errors"

var errWindowsOnly = errors.New("Windows only")

// TrustedSystemPath: the MSI's checks are Windows-only.
func TrustedSystemPath(string, ...string) error { return errWindowsOnly }

// AccountSID: Windows-only.
func AccountSID(string) (string, error) { return "", errWindowsOnly }

// UserSID: Windows-only.
func UserSID(string) error { return errWindowsOnly }

// IsLocalAdmin: Windows-only.
func IsLocalAdmin(string) (bool, error) { return false, errWindowsOnly }
