//go:build windows

package windows

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

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
	enablePrivileges() // SeRestorePrivilege: set another account as owner
	acl := "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + sid + ")"
	// skip if already: don't walk a big tree again
	return secureDirSDDL(dir, "O:"+sid+acl, "O:"+sid+acl, "O:BA"+acl)
}

// SecureDir creates dir and gives it Administrators as owner and a
// protected DACL: full control for SYSTEM, Administrators and user (if not
// ""), nothing for anybody else (ProgramData would otherwise let every local
// user read the node key). An existing dir that is a junction/symlink is
// refused, and taking ownership removes any rights a local user had as the
// creator of a pre-created dir.
func SecureDir(dir, user string) error {
	sddl := "O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	skip := sddl // already: don't walk a big tree again
	if strings.HasPrefix(strings.ToUpper(user), "S-1-") {
		// a SID (the MSI's UserSID: Entra accounts have no usable name)
		sid, err := windows.StringToSid(user)
		if err != nil {
			return fmt.Errorf("account %q: %w", user, err)
		}
		sddl += "(A;OICI;FA;;;" + sid.String() + ")"
		skip = ""
	} else if a := normalizeAccount(user); a != "" {
		sid, _, _, err := windows.LookupSID("", strings.TrimPrefix(a, `.\`))
		if err != nil {
			return fmt.Errorf("lookup account %q: %w", user, err)
		}
		sddl += "(A;OICI;FA;;;" + sid.String() + ")"
		skip = ""
	}
	return secureDirSDDL(dir, skip, sddl)
}

// secureDirSDDL gives dir the first of sddls that can be set, unless it
// already has skip (if not ""). Missing parents are created plainly; a
// missing dir gets its descriptor atomically from CreateDirectory, then
// (like an existing one) through a handle on the dir itself, refused if
// that is a junction/symlink - never by name, which a user owning the
// parent could swap for a junction to anywhere in between.
func secureDirSDDL(dir, skip string, sddls ...string) error {
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	for _, sddl := range sddls {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			return err
		}
		sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
		// set again below: CreateDirectory drops the DACL's auto-inherited
		// flag (HasSDDL wants it as SetSecurityInfo leaves it)
		err = windows.CreateDirectory(p, &sa)
		if err == nil || err == windows.ERROR_ALREADY_EXISTS {
			break
		}
	}
	// none could be set at creation (the owner?): the dir is still created,
	// as MkdirAll did, and setting the descriptor below says why it fails
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	// no FILE_SHARE_DELETE: it can't be renamed away while we hold it
	h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: dir, Err: err}
	}
	defer windows.CloseHandle(h)
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return &os.PathError{Op: "stat", Path: dir, Err: err}
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("%s exists and is not a plain directory (reparse point?)", dir)
	}
	if skip != "" {
		sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err == nil && sameSDDL(sd, skip) {
			return nil
		}
	}
	for i, sddl := range sddls {
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
		err = windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			owner, nil, dacl, nil)
		if err == nil || i == len(sddls)-1 {
			return err
		}
	}
	return nil
}

// HasSDDL reports whether dir's owner and DACL already are want (setting
// a protected DACL re-propagates it through the whole tree below).
func HasSDDL(dir, want string) bool {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	return err == nil && sameSDDL(sd, want)
}

func sameSDDL(sd *windows.SECURITY_DESCRIPTOR, want string) bool {
	w, err := windows.SecurityDescriptorFromString(want)
	return err == nil && sd.String() == w.String()
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
	sddl := TraverseSDDL(trustee)
	return secureDirSDDL(dir, sddl, sddl)
}

// TraverseSDDL is the security descriptor SecureTraverseDir sets.
func TraverseSDDL(trustee string) string {
	// 0x1000a0 = SYNCHRONIZE | FILE_READ_ATTRIBUTES | FILE_TRAVERSE
	return "O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;;0x1000a0;;;" + trustee + ")"
}
