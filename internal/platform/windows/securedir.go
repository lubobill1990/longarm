//go:build windows

package windows

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// GrantServiceLogon gives account "Log on as a service" (MSI installs that
// create the service themselves).
func GrantServiceLogon(account string) error {
	if a := normalizeAccount(account); a != "" {
		return grantServiceLogonRight(a)
	}
	return nil
}

// SecureUserDir creates dir for a per-user account (SID sid): the account
// owns it and a protected DACL gives full control to it, SYSTEM and
// Administrators only - inherited by everything below, so other accounts
// can neither list nor read it.
func SecureUserDir(dir, sid string) error {
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a plain directory (reparse point?)", dir)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	enablePrivileges() // SeRestorePrivilege: set another account as owner
	acl := "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + sid + ")"
	for _, sddl := range []string{"O:" + sid + acl, "O:BA" + acl} {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			return err
		}
		owner, _, err := sd.Owner()
		if err != nil {
			return err
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			return err
		}
		err = windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			owner, nil, dacl, nil)
		if err == nil {
			return nil
		}
		if strings.HasPrefix(sddl, "O:BA") {
			return err
		}
	}
	return nil
}

// SecureDir creates dir and gives it Administrators as owner and a
// protected DACL: full control for SYSTEM, Administrators and user (if not
// ""), nothing for anybody else (ProgramData would otherwise let every local
// user read the node key). An existing dir that is a junction/symlink is
// refused, and taking ownership removes any rights a local user had as the
// creator of a pre-created dir.
func SecureDir(dir, user string) error {
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a plain directory (reparse point?)", dir)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sddl := "O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	if a := normalizeAccount(user); a != "" {
		sid, _, _, err := windows.LookupSID("", strings.TrimPrefix(a, `.\`))
		if err != nil {
			return fmt.Errorf("lookup account %q: %w", user, err)
		}
		sddl += "(A;OICI;FA;;;" + sid.String() + ")"
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil)
}
