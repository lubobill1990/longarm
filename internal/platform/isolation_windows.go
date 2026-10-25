//go:build windows

package platform

import (
	"errors"

	win "github.com/lubobill1990/longarm/internal/platform/windows"
)

// CanIsolate reports whether this process can run workloads as other OS
// accounts (Windows: LocalSystem; Linux: root; macOS: never).
func CanIsolate() error { return win.CanIsolate() }

// EnsureAccount creates the local account name if missing (no logon rights
// but batch; a random password nobody knows) and returns it.
func EnsureAccount(name string) (*Account, error) { return win.EnsureAccount(name) }

// LookupAccount returns an existing local account.
func LookupAccount(name string) (*Account, error) { return win.LookupAccount(name) }

// DeleteAccount deletes a local account with its profile / home.
func DeleteAccount(name string) error { return win.DeleteAccount(name) }

// AccountEnv is the base environment of the account's processes.
func AccountEnv(a *Account) ([]string, error) { return win.AccountEnv(a) }

// SecureUserDir creates dir owned by a and readable by a, SYSTEM/root and
// Administrators only.
func SecureUserDir(dir string, a *Account) error {
	if a == nil || a.SID == "" {
		return errors.New("SecureUserDir: account without SID")
	}
	return win.SecureUserDir(dir, a.SID)
}

// SecurePrivateDir creates dir accessible to SYSTEM/root and Administrators
// only.
func SecurePrivateDir(dir string) error { return win.SecureDir(dir, "") }

// PortOwners reports who listens on TCP port: whether a process of a does
// and whether a process of any other account does.
func PortOwners(port int, a *Account) (mine, others bool, err error) {
	pids, err := win.ListenerPIDs(port)
	if err != nil {
		return false, false, err
	}
	for _, pid := range pids {
		if sid := win.ProcessUserSID(pid); sid != "" && sid == a.SID {
			mine = true
		} else {
			others = true
		}
	}
	return mine, others, nil
}
