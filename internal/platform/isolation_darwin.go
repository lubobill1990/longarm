//go:build darwin

package platform

import "errors"

// ErrNoIsolation: per-user isolation is not implemented on macOS.
var ErrNoIsolation = errors.New("per-user isolation is not supported on macOS workers (use [isolation] mode = \"shared\"; see docs/security.md)")

// CanIsolate reports whether this process can run workloads as other OS
// accounts (Windows: LocalSystem; Linux: root; macOS: never).
func CanIsolate() error { return ErrNoIsolation }

func EnsureAccount(string) (*Account, error)       { return nil, ErrNoIsolation }
func LookupAccount(string) (*Account, error)       { return nil, ErrNoIsolation }
func DeleteAccount(string) error                   { return ErrNoIsolation }
func AccountEnv(*Account) ([]string, error)        { return nil, ErrNoIsolation }
func SecureUserDir(string, *Account) error         { return ErrNoIsolation }
func SecurePrivateDir(string) error                { return ErrNoIsolation }
func PortOwners(int, *Account) (bool, bool, error) { return false, false, ErrNoIsolation }

func LoadProfile(*Account) (func(), error) { return nil, ErrNoIsolation }
