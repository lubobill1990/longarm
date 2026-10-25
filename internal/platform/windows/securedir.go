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
