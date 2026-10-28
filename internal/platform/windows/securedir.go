//go:build windows

package windows

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	if HasSDDL(dir, "O:"+sid+acl) {
		return nil // already: don't walk a big tree again
	}
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
	if user == "" && HasSDDL(dir, sddl) {
		return nil // already: don't walk a big tree again
	}
	if strings.HasPrefix(strings.ToUpper(user), "S-1-") {
		// a SID (the MSI's UserSID: Entra accounts have no usable name)
		sid, err := windows.StringToSid(user)
		if err != nil {
			return fmt.Errorf("account %q: %w", user, err)
		}
		sddl += "(A;OICI;FA;;;" + sid.String() + ")"
	} else if a := normalizeAccount(user); a != "" {
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

// HasSDDL reports whether dir's owner and DACL already are want (setting
// a protected DACL re-propagates it through the whole tree below).
func HasSDDL(dir, want string) bool {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	w, err := windows.SecurityDescriptorFromString(want)
	if err != nil {
		return false
	}
	return sd.String() == w.String()
}

// ResetInherited makes everything below dir (but not dir itself, and not
// what skip says) carry only the ACEs it inherits - explicitly, top-down,
// instead of trusting SetNamedSecurityInfo's propagation to have reached
// every entry (it didn't reliably for a running longarmd service).
func ResetInherited(dir string, skip func(path string) bool) error {
	esd, err := windows.SecurityDescriptorFromString("D:") // a DACL without ACEs
	if err != nil {
		return err
	}
	empty, _, err := esd.DACL()
	if err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		if skip != nil && skip(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, empty, nil); err != nil {
			return fmt.Errorf("reset ACL of %s: %w", p, err)
		}
		return nil
	})
}

// SecureTraverseDir creates dir with a protected DACL: full control for
// SYSTEM and Administrators (inherited), and for trustee (a SID or SDDL
// alias like BU) only "traverse + read attributes" on dir itself - enough
// to reach (and stat) what lies below without listing it.
func SecureTraverseDir(dir, trustee string) error {
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a plain directory (reparse point?)", dir)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sddl := TraverseSDDL(trustee)
	if HasSDDL(dir, sddl) {
		return nil
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

// TraverseSDDL is the security descriptor SecureTraverseDir sets.
func TraverseSDDL(trustee string) string {
	// 0x1000a0 = SYNCHRONIZE | FILE_READ_ATTRIBUTES | FILE_TRAVERSE
	return "O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;;0x1000a0;;;" + trustee + ")"
}
