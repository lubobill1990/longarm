//go:build !windows

package platform

import (
	"os"
	"os/user"
	"strconv"
)

// SecureDir creates dir with mode 0700, owned by user if given (root only).
func SecureDir(dir, username string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if username == "" {
		return nil
	}
	u, err := user.Lookup(username)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return os.Chown(dir, uid, gid)
}

// GrantServiceLogon is a Windows concept; a no-op here.
func GrantServiceLogon(string) error { return nil }
