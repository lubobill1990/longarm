//go:build windows

package windows

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// traverseMask is what a locked system dir may grant anybody besides
// SYSTEM and Administrators: SYNCHRONIZE | FILE_READ_ATTRIBUTES |
// FILE_TRAVERSE (SecureTraverseDir).
const traverseMask = 0x1000a0

// broadSIDs are well-known groups every local user is a member of (or that
// stand for whoever creates something): a system dir granting them more
// than traverse rights is open to every local user.
var broadSIDs = []windows.WELL_KNOWN_SID_TYPE{
	windows.WinWorldSid, windows.WinBuiltinUsersSid, windows.WinAuthenticatedUserSid,
	windows.WinInteractiveSid, windows.WinBuiltinGuestsSid, windows.WinAnonymousSid,
	windows.WinCreatorOwnerSid, windows.WinCreatorGroupSid, windows.WinNetworkSid,
	windows.WinLocalSid, windows.WinBatchSid, windows.WinServiceSid, windows.WinRemoteLogonIdSid,
	windows.WinThisOrganizationSid, windows.WinBuiltinRemoteDesktopUsersSid, windows.WinConsoleLogonSid,
}

// SystemOwnerSID reports whether sid is SYSTEM or Administrators: an owner
// only LocalSystem or an elevated administrator can have set.
func SystemOwnerSID(sid *windows.SID) bool {
	return sid != nil && (sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
}

// TrustedOwnerSID reports whether sid is SYSTEM, Administrators or the
// account this process runs as (an elevated administrator's files carry
// Administrators or the account itself as owner).
func TrustedOwnerSID(sid *windows.SID) bool {
	if SystemOwnerSID(sid) {
		return true
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && sid != nil && windows.EqualSid(sid, u.User.Sid)
}

// SIDName is sid's account name (DOMAIN\name), else its string form.
func SIDName(sid *windows.SID) string {
	if sid == nil {
		return "nobody"
	}
	if a, d, _, err := sid.LookupAccount(""); err == nil {
		if d != "" {
			return d + `\` + a
		}
		return a
	}
	return sid.String()
}

// PathSD returns path's owner and DACL (a final reparse point is followed:
// callers check with Lstat first).
func PathSD(path string) (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
}

// PathOwner returns the owner of path (see PathSD).
func PathOwner(path string) (*windows.SID, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	o, _, err := sd.Owner()
	return o, err
}

// FileOwner returns the owner of an open file.
func FileOwner(f *os.File) (*windows.SID, error) {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	o, _, err := sd.Owner()
	return o, err
}

// LockedDACL reports whether sd's DACL is protected (nothing inherited from
// the parent: ProgramData lets every user create entries, and their
// creator owns them) and grants no broad group (Everyone, Users,
// Authenticated Users, CREATOR OWNER, ...) more than traverse rights.
// Explicit grants to SYSTEM, Administrators or individual accounts (e.g. a
// service account) are fine: only the owner - checked separately - or an
// administrator can have added them. why says what is wrong.
func LockedDACL(sd *windows.SECURITY_DESCRIPTOR) (locked bool, why string) {
	ctl, _, err := sd.Control()
	if err != nil {
		return false, err.Error()
	}
	if ctl&windows.SE_DACL_PROTECTED == 0 {
		return false, "its ACL inherits from the parent"
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false, "it has no DACL (full access for everybody)"
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, err.Error()
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE, 6 /* object */, 10 /* callback */, 12 /* callback object */ :
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return false, fmt.Sprintf("it has an allow ACE of type %d", ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if SystemOwnerSID(sid) || uint32(ace.Mask)&^traverseMask == 0 {
			continue
		}
		for _, w := range broadSIDs {
			if sid.IsWellKnown(w) {
				return false, fmt.Sprintf("it grants %s access 0x%x", SIDName(sid), uint32(ace.Mask))
			}
		}
	}
	return true, ""
}
