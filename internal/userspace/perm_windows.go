//go:build windows

package userspace

import (
	"path/filepath"
	"strings"

	"github.com/lubobill1990/longarm/internal/platform"
	win "github.com/lubobill1990/longarm/internal/platform/windows"
)

// secureUsersDir creates the users dir (SYSTEM and Administrators; local
// users may only traverse it and read its attributes, not list it).
func secureUsersDir(dir string) error { return win.SecureTraverseDir(dir, "BU") }

// secureSpaceRoot creates a user's space root (SYSTEM and Administrators;
// the account may traverse it).
func secureSpaceRoot(dir string, a *platform.Account) error { return win.SecureTraverseDir(dir, a.SID) }

func checkTraversable(string) error { return nil }

// CheckReachable: Windows ACLs (SecureTraverseDir) let the accounts reach
// the users dir wherever it lies; nothing to check before longarmd runs.
func CheckReachable(string, string) error { return nil }

// secureDataDir locks longarmd's data dir down for per-user isolation:
// SYSTEM and Administrators only, inherited by everything in it (reset
// explicitly; the users dir has ACLs of its own), plus traverse/read
// attributes for local users on the dir itself, which programs like git
// need to resolve paths into their own data below it.
func secureDataDir(dir string) error {
	// The MSI's parent (C:\ProgramData\Longarm): nobody may swap the data
	// dir underneath it, but the accounts must reach through it.
	if parent := filepath.Dir(dir); strings.EqualFold(filepath.Base(parent), "longarm") {
		if err := win.SecureTraverseDir(parent, "BU"); err != nil {
			return err
		}
	}
	if win.HasSDDL(dir, win.TraverseSDDL("BU")) {
		return nil // locked down already (by an earlier start)
	}
	if err := win.SecureTraverseDir(dir, "BU"); err != nil {
		return err
	}
	users := filepath.Join(dir, UsersDir)
	return win.ResetInherited(dir, func(p string) bool { return strings.EqualFold(p, users) })
}
