//go:build windows

package windows

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The checks the MSI's elevated custom actions (longarmd install-helper)
// make before they act as SYSTEM: who asked (an administrator?), for whom
// (an existing user?), and whether the files they hand to SYSTEM live where
// only administrators can write.

// trustedInstallerSID is NT SERVICE\TrustedInstaller (owns Program Files).
const trustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

// writeMask: rights that let an account change a directory's or file's
// contents, ACL or owner.
const writeMask = 0x2 | 0x4 | 0x10 | 0x40 | 0x100 | // write/append data (add file/subdir), write EA, delete child, write attributes
	0x10000 | 0x40000 | 0x80000 | // DELETE, WRITE_DAC, WRITE_OWNER
	0x10000000 | 0x40000000 // GENERIC_ALL, GENERIC_WRITE

const inheritOnlyAce = 0x08

func adminSID(sid *windows.SID) bool {
	return sid != nil && (SystemOwnerSID(sid) || sid.String() == trustedInstallerSID)
}

// TrustedSystemPath checks that only SYSTEM, Administrators, TrustedInstaller
// (and the accounts in also, SID strings) can change path: no reparse point,
// owned by one of them, and no allow ACE giving anybody else write, delete,
// ACL or owner rights. Program Files qualifies; a directory a user created
// (in ProgramData, say) does not: its creator owns it.
func TrustedSystemPath(path string, also ...string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("%s is a reparse point", path)
	}
	sd, err := PathSD(path)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if !adminSID(owner) && !contains(also, owner.String()) {
		return fmt.Errorf("%s is owned by %s, not SYSTEM / Administrators", path, SIDName(owner))
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("%s has no DACL (full access for everybody)", path)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE || ace.Header.AceFlags&inheritOnlyAce != 0 {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("%s has an allow ACE of type %d", path, ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if adminSID(sid) || contains(also, sid.String()) || uint32(ace.Mask)&writeMask == 0 {
			continue
		}
		return fmt.Errorf("%s lets %s change it (access 0x%x)", path, SIDName(sid), uint32(ace.Mask))
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// AccountSID is the SID of an account name (".\name", "DOMAIN\name").
func AccountSID(name string) (string, error) {
	a := normalizeAccount(name)
	if a == "" {
		return "S-1-5-18", nil
	}
	sid, _, _, err := windows.LookupSID("", strings.TrimPrefix(a, `.\`))
	if err != nil {
		return "", err
	}
	return sid.String(), nil
}

// UserSID checks that sid names an existing user account (local, domain or
// Entra), not a group or a well-known account.
func UserSID(sid string) error {
	s, err := windows.StringToSid(sid)
	if err != nil {
		return fmt.Errorf("%q is not a SID: %w", sid, err)
	}
	_, _, use, err := s.LookupAccount("")
	if err != nil {
		return fmt.Errorf("no account %s: %w", sid, err)
	}
	if use != windows.SidTypeUser {
		return fmt.Errorf("%s is not a user account (type %d)", sid, use)
	}
	return nil
}

var (
	procNetLocalGroupGetMembers        = modnetapi32.NewProc("NetLocalGroupGetMembers")
	procNetUserGetLocalGroups          = modnetapi32.NewProc("NetUserGetLocalGroups")
	errNotAdminDetermined              = errors.New("could not determine the account's groups")
	maxPreferredLength          uint32 = 0xFFFFFFFF
)

// IsLocalAdmin reports whether the account sid is a member of the local
// Administrators group - directly (also Entra accounts, as on a Dev Box) or
// through a domain group - or is LocalSystem. It reads group memberships,
// not a token: the MSI's custom actions run as SYSTEM, not as the user who
// asked for the install.
func IsLocalAdmin(sid string) (bool, error) {
	s, err := windows.StringToSid(sid)
	if err != nil {
		return false, err
	}
	if s.IsWellKnown(windows.WinLocalSystemSid) {
		return true, nil
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, err
	}
	group, _, _, err := admins.LookupAccount("")
	if err != nil {
		return false, err
	}
	gname, _ := windows.UTF16PtrFromString(group)
	// direct members
	var buf *byte
	var read, total uint32
	var resume uintptr
	if r, _, _ := procNetLocalGroupGetMembers.Call(0, uintptr(unsafe.Pointer(gname)), 0, uintptr(unsafe.Pointer(&buf)),
		uintptr(maxPreferredLength), uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&resume))); r == 0 && buf != nil {
		members := unsafe.Slice((**windows.SID)(unsafe.Pointer(buf)), read)
		found := false
		for _, m := range members {
			if m != nil && windows.EqualSid(m, s) {
				found = true
				break
			}
		}
		windows.NetApiBufferFree(buf)
		if found {
			return true, nil
		}
	}
	// through groups (domain accounts)
	acct, dom, _, err := s.LookupAccount("")
	if err != nil {
		return false, err
	}
	name := acct
	if dom != "" {
		name = dom + `\` + acct
	}
	uname, _ := windows.UTF16PtrFromString(name)
	buf, read, total = nil, 0, 0
	const lgIncludeIndirect = 1
	r, _, _ := procNetUserGetLocalGroups.Call(0, uintptr(unsafe.Pointer(uname)), 0, lgIncludeIndirect, uintptr(unsafe.Pointer(&buf)),
		uintptr(maxPreferredLength), uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)))
	if r != 0 || buf == nil {
		return false, fmt.Errorf("%w (%s: NetUserGetLocalGroups %d)", errNotAdminDetermined, name, r)
	}
	defer windows.NetApiBufferFree(buf)
	for _, g := range unsafe.Slice((**uint16)(unsafe.Pointer(buf)), read) {
		if g != nil && strings.EqualFold(windows.UTF16PtrToString(g), group) {
			return true, nil
		}
	}
	return false, nil
}
