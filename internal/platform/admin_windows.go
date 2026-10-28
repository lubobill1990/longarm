//go:build windows

package platform

import win "github.com/lubobill1990/longarm/internal/platform/windows"

// TrustedSystemPath checks that only SYSTEM, Administrators and
// TrustedInstaller (and the SIDs in also) can change path (see
// windows.TrustedSystemPath).
func TrustedSystemPath(path string, also ...string) error {
	return win.TrustedSystemPath(path, also...)
}

// AccountSID is the SID of an account name.
func AccountSID(name string) (string, error) { return win.AccountSID(name) }

// UserSID checks that sid is an existing user account.
func UserSID(sid string) error { return win.UserSID(sid) }

// IsLocalAdmin reports whether the account sid is a local administrator.
func IsLocalAdmin(sid string) (bool, error) { return win.IsLocalAdmin(sid) }
