//go:build !windows

package userspace

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lubobill1990/longarm/internal/platform"
)

// secureUsersDir creates the users dir: root's, but traversable (0711) so
// the accounts reach their own data below it, without listing anybody's.
func secureUsersDir(dir string) error {
	if fi, err := os.Lstat(dir); err == nil && (fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir()) {
		return fmt.Errorf("%s exists and is not a plain directory", dir)
	}
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return err
	}
	if err := os.Lchown(dir, 0, 0); err != nil {
		return err
	}
	return os.Chmod(dir, 0o711)
}

// secureSpaceRoot creates a user's space root: root's, traversable by the
// account's group only (0710).
func secureSpaceRoot(dir string, a *platform.Account) error {
	if fi, err := os.Lstat(dir); err == nil && (fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir()) {
		return fmt.Errorf("%s exists and is not a plain directory", dir)
	}
	if err := os.MkdirAll(dir, 0o710); err != nil {
		return err
	}
	if err := os.Lchown(dir, 0, a.GID); err != nil {
		return err
	}
	return os.Chmod(dir, 0o710)
}

// checkTraversable fails when an account could not reach dir: every parent
// needs the execute bit for others.
func checkTraversable(dir string) error {
	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		fi, err := os.Stat(d)
		if err != nil {
			return err
		}
		if fi.Mode().Perm()&0o001 == 0 {
			return fmt.Errorf("per-user isolation: the accounts cannot reach %s because %s is not traversable (mode %v); "+
				"move the data dir or set [isolation] users_dir to a path whose parents have o+x (e.g. /var/lib/longarm-users)", dir, d, fi.Mode().Perm())
		}
		if p := filepath.Dir(d); p == d {
			return nil
		}
	}
}
