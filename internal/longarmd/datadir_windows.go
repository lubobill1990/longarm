//go:build windows

package longarmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/lubobill1990/longarm/internal/platform"
	win "github.com/lubobill1990/longarm/internal/platform/windows"
	"github.com/lubobill1990/longarm/internal/userspace"
)

func currentDataDirEnv() dataDirEnv {
	home, _ := os.UserHomeDir()
	pd, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		pd = os.Getenv("ProgramData")
	}
	system := isLocalSystem()
	if system && os.Getenv(SystemDataDirEnv) == "" {
		// LocalSystem's own profile, not %USERPROFILE% from the
		// environment (whoever started us may have set that).
		if h, err := windows.GetCurrentProcessToken().GetUserProfileDirectory(); err == nil && h != "" {
			home = h
		}
	}
	return dataDirEnv{
		getenv:   os.Getenv,
		home:     home,
		system:   system,
		elevated: !system && platform.Elevated(),
		sysDir:   systemDataDirFor("windows", os.Getenv, pd),
		inUse:    DataDirInUse,
	}
}

// isLocalSystem reports whether this process runs as LocalSystem (the
// per-user service, the MSI's custom actions).
func isLocalSystem() bool {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	return u.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
}

// secureSystemDir creates the system data dir (and the MSI's parent,
// ...\Longarm) with a protected DACL for SYSTEM and Administrators, or
// checks an existing one. ProgramData lets every local user create
// directories, so either may have been pre-created by somebody else:
//   - owned by anybody but SYSTEM, Administrators or this account: refused
//     (the owner could re-open it at any time);
//   - owned by this account (an elevated admin's mkdir), with a DACL that is
//     not protected, or one that gives Everyone, Users, ... more than
//     traverse rights: locked down again (owner Administrators, SYSTEM and
//     Administrators only); everything below must be owned by SYSTEM,
//     Administrators or this account (else refused) and gets the inherited
//     ACL only.
//
// A locked DACL with extra grants for individual accounts (a shared-mode
// service account) or per-user isolation's traverse right is kept.
func secureSystemDir(dir string) error {
	if parent := filepath.Dir(dir); strings.EqualFold(filepath.Base(parent), "longarm") {
		if err := secureSystemDirOne(parent, false); err != nil {
			return err
		}
	}
	return secureSystemDirOne(dir, true)
}

func secureSystemDirOne(dir string, contents bool) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := platform.SecureDir(dir, ""); err != nil {
			return fmt.Errorf("secure %s: %w", dir, err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !fi.IsDir() {
		return fmt.Errorf("%s exists and is not a plain directory (reparse point?): remove it and retry", dir)
	}
	sd, err := win.PathSD(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	if !win.TrustedOwnerSID(owner) {
		return fmt.Errorf("%s exists but is owned by %s, not SYSTEM or Administrators: "+
			"somebody else created it (every local user may create directories in ProgramData); "+
			"check and remove it, then retry", dir, win.SIDName(owner))
	}
	locked, why := win.LockedDACL(sd)
	if !win.SystemOwnerSID(owner) {
		// Our own account's (an elevated admin created it): taken over by
		// Administrators like a fresh one.
		locked, why = false, "owned by "+win.SIDName(owner)
	}
	if locked {
		return nil
	}
	if contents {
		if err := checkSystemOwned(dir); err != nil {
			return err
		}
	}
	if err := platform.SecureDir(dir, ""); err != nil {
		return fmt.Errorf("secure %s (%s): %w", dir, why, err)
	}
	if contents {
		users := filepath.Join(dir, userspace.UsersDir)
		if err := win.ResetInherited(dir, func(p string) bool { return strings.EqualFold(p, users) }); err != nil {
			return err
		}
	}
	return nil
}

// checkSystemOwned fails when anything below dir (but the users dir's
// contents, which belong to the accounts) is a reparse point or not owned
// by SYSTEM, Administrators or this account.
func checkSystemOwned(dir string) error {
	users := filepath.Join(dir, userspace.UsersDir)
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return fmt.Errorf("%s is a reparse point in the system data dir %s, which every local user could write: check and remove it, then retry", p, dir)
		}
		owner, err := win.PathOwner(p)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if !win.TrustedOwnerSID(owner) {
			return fmt.Errorf("%s is owned by %s: the system data dir %s was open to other local users; check and remove it, then retry", p, win.SIDName(owner), dir)
		}
		if d.IsDir() && strings.EqualFold(p, users) {
			return filepath.SkipDir
		}
		return nil
	})
}

// checkSourceTrusted fails unless the open migration source f (file or
// dir) is owned by SYSTEM, Administrators or this account.
func checkSourceTrusted(f *os.File, _ fs.FileInfo) error {
	owner, err := win.FileOwner(f)
	if err != nil {
		return fmt.Errorf("%s: %w", f.Name(), err)
	}
	if !win.TrustedOwnerSID(owner) {
		return fmt.Errorf("%s is owned by %s, not by SYSTEM, Administrators or this account: not copying an enrollment somebody else may have planted", f.Name(), win.SIDName(owner))
	}
	return nil
}
