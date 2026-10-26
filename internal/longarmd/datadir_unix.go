//go:build !windows

package longarmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"syscall"

	"github.com/lubobill1990/longarm/internal/platform"
)

func currentDataDirEnv() dataDirEnv {
	home, _ := os.UserHomeDir()
	system := os.Geteuid() == 0
	if system && os.Getenv(SystemDataDirEnv) == "" {
		// root's home from the passwd database, not $HOME: sudo -E (or a
		// sudoers env_keep) keeps the invoking user's, and root must not
		// adopt (or migrate) an enrollment from there.
		if u, err := user.LookupId(strconv.Itoa(os.Geteuid())); err == nil && u.HomeDir != "" {
			home = u.HomeDir
		}
	}
	return dataDirEnv{
		getenv: os.Getenv,
		home:   home,
		system: system,
		sysDir: systemDataDirFor(runtime.GOOS, os.Getenv, ""),
		inUse:  DataDirInUse,
	}
}

// secureSystemDir creates the system data dir (/var/lib/longarmd) 0700, or
// checks an existing one: a plain dir owned by root (this account), not
// writable by group or others - anything else is refused, as somebody
// else could have planted (or could still swap) its contents.
func secureSystemDir(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return platform.SecureDir(dir, "")
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s exists and is not a plain directory: remove it and retry", dir)
	}
	return ownedAndPrivate(dir, fi, "the system data dir")
}

// ownedAndPrivate fails unless fi (of path) is owned by root or this
// account and not writable by group or others.
func ownedAndPrivate(path string, fi fs.FileInfo, what string) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no owner information", path)
	}
	if uid := int(st.Uid); uid != 0 && uid != os.Geteuid() {
		return fmt.Errorf("%s (%s) is owned by uid %d, not root: check and remove it, then retry", path, what, uid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s (%s) is writable by group or others (mode %v): check it, then chmod go-w it and retry", path, what, fi.Mode().Perm())
	}
	return nil
}

// checkSourceTrusted fails unless the open migration source f (file or
// dir; fi its fstat) is owned by root (this account) and not writable by
// group or others.
func checkSourceTrusted(f *os.File, fi fs.FileInfo) error {
	if err := ownedAndPrivate(f.Name(), fi, "migration source"); err != nil {
		return fmt.Errorf("%w; not copying an enrollment somebody else may have planted", err)
	}
	return nil
}
