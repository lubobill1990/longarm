//go:build !windows

package msiupgrade

import (
	"errors"
	"os"
	"strings"
)

var errWindowsOnly = errors.New("the MSI helper is Windows-only")

// WindowsEnv: the helper is Windows-only; elsewhere every step fails.
func WindowsEnv() Env {
	fail := func() (string, error) { return "", errWindowsOnly }
	return Env{InstallDir: fail, CurrentSID: fail, StateDir: fail,
		IsAdmin: func(string) (bool, error) { return false, errWindowsOnly }, ServiceNames: func() []string { return nil }}
}

// InUse: no image locking outside Windows.
func InUse(string) bool { return false }

// RawCommandLine: the arguments, joined.
func RawCommandLine() string { return strings.Join(os.Args, " ") }
