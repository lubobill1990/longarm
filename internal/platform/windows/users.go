//go:build windows

package windows

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
)

// Per-user isolation (docs/security.md): every Longarm user gets a local
// account (la-<user>) that longarmd - running as LocalSystem - creates on
// first use with a random password nobody knows, no interactive, remote
// desktop, network or service logon rights, and only "Log on as a batch
// job" for the S4U logons workloads run under. Workloads get a primary
// token from LsaLogonUser(S4U) (no password needed, SeTcbPrivilege), the
// account's profile is loaded for them (HKCU, %APPDATA%) and they get a
// window station of their own logon session.

var (
	modnetapi32    = windows.NewLazySystemDLL("netapi32.dll")
	procNetUserAdd = modnetapi32.NewProc("NetUserAdd")
	procNetUserDel = modnetapi32.NewProc("NetUserDel")

	modsecur32                         = windows.NewLazySystemDLL("secur32.dll")
	procLsaRegisterLogonProcess        = modsecur32.NewProc("LsaRegisterLogonProcess")
	procLsaLookupAuthenticationPackage = modsecur32.NewProc("LsaLookupAuthenticationPackage")
	procLsaLogonUser                   = modsecur32.NewProc("LsaLogonUser")
	procLsaFreeReturnBuffer            = modsecur32.NewProc("LsaFreeReturnBuffer")
	procLsaDeregisterLogonProcess      = modsecur32.NewProc("LsaDeregisterLogonProcess")

	moduserenv              = windows.NewLazySystemDLL("userenv.dll")
	procLoadUserProfileW    = moduserenv.NewProc("LoadUserProfileW")
	procUnloadUserProfile   = moduserenv.NewProc("UnloadUserProfile")
	procDeleteProfileW      = moduserenv.NewProc("DeleteProfileW")
	procAllocLocallyUniqueI = modadvapi32.NewProc("AllocateLocallyUniqueId")
)

// userInfo1 is USER_INFO_1.
type userInfo1 struct {
	Name        *uint16
	Password    *uint16
	PasswordAge uint32
	Priv        uint32
	HomeDir     *uint16
	Comment     *uint16
	Flags       uint32
	ScriptPath  *uint16
}

const (
	userPrivUser         = 1
	ufScript             = 0x0001
	ufDontExpirePasswd   = 0x10000
	ufPasswdCantChange   = 0x0040
	ufNormalAccount      = 0x0200
	nerrUserNotFound     = 2221
	nerrUserExists       = 2224
	msv1_0S4ULogon       = 12
	logonTypeBatch       = 4
	piNoUI               = 1
	maxAccountNameLength = 20
)

// rights given to / taken from per-user accounts.
var (
	accountRights     = []string{"SeBatchLogonRight"}
	accountDenyRights = []string{"SeDenyInteractiveLogonRight", "SeDenyRemoteInteractiveLogonRight",
		"SeDenyNetworkLogonRight", "SeDenyServiceLogonRight"}
)

// accountMu serializes account creation/deletion.
var accountMu sync.Mutex

// randomPassword is a long random password meeting complexity rules; it is
// never stored or shown (S4U logons need none).
func randomPassword() string {
	const sets = "ABCDEFGHJKLMNPQRSTUVWXYZ" + "abcdefghijkmnopqrstuvwxyz" + "23456789" + "!#%+-.=?@_"
	b := make([]byte, 40)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(sets))))
		b[i] = sets[n.Int64()]
	}
	// one of each class
	return "Aa1!" + string(b)
}

// EnsureAccount creates the local account name (if missing) with no logon
// rights except batch, and returns it with its (created) profile directory.
func EnsureAccount(name string) (*ptypes.Account, error) {
	if name == "" || len(name) > maxAccountNameLength || strings.ContainsAny(name, `"/\[]:;|=,+*?<>@ `) {
		return nil, fmt.Errorf("invalid account name %q", name)
	}
	accountMu.Lock()
	defer accountMu.Unlock()
	if _, _, _, err := windows.LookupSID("", name); err != nil {
		pw := randomPassword()
		ui := userInfo1{Name: windows.StringToUTF16Ptr(name), Password: windows.StringToUTF16Ptr(pw), Priv: userPrivUser,
			Comment: windows.StringToUTF16Ptr("Longarm per-user isolation account (longarmd)"),
			Flags:   ufScript | ufDontExpirePasswd | ufPasswdCantChange | ufNormalAccount}
		var parmErr uint32
		r, _, _ := procNetUserAdd.Call(0, 1, uintptr(unsafe.Pointer(&ui)), uintptr(unsafe.Pointer(&parmErr)))
		if r != 0 && r != nerrUserExists {
			return nil, fmt.Errorf("NetUserAdd %s: %w (parameter %d)", name, windows.Errno(r), parmErr)
		}
	}
	sid, _, _, err := windows.LookupSID("", name)
	if err != nil {
		return nil, fmt.Errorf("lookup %s: %w", name, err)
	}
	for _, r := range accountRights {
		if err := addAccountRight(sid, r); err != nil {
			return nil, fmt.Errorf("grant %s to %s: %w", r, name, err)
		}
	}
	for _, r := range accountDenyRights {
		if err := addAccountRight(sid, r); err != nil {
			return nil, fmt.Errorf("set %s for %s: %w", r, name, err)
		}
	}
	acct := &ptypes.Account{Name: name, SID: sid.String()}
	// The first profile load creates C:\Users\<name> (owner, SYSTEM and
	// Administrators only).
	s, err := Logon(acct)
	if err != nil {
		return nil, err
	}
	acct.Home = s.Home
	s.Close()
	return acct, nil
}

// LookupAccount returns an existing account (its profile directory may be
// empty if it never logged on).
func LookupAccount(name string) (*ptypes.Account, error) {
	sid, _, typ, err := windows.LookupSID("", name)
	if err != nil {
		return nil, err
	}
	if typ != windows.SidTypeUser {
		return nil, fmt.Errorf("%s is not a user account", name)
	}
	acct := &ptypes.Account{Name: name, SID: sid.String()}
	acct.Home = profilePath(acct.SID)
	return acct, nil
}

func profilePath(sid string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList\`+sid, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	p, _, err := k.GetStringValue("ProfileImagePath")
	if err != nil {
		return ""
	}
	if exp, err := registry.ExpandString(p); err == nil {
		return exp
	}
	return p
}

// DeleteAccount deletes the account, its profile and its rights. Its
// processes must be gone (longarmd kills its jobs first).
func DeleteAccount(name string) error {
	accountMu.Lock()
	defer accountMu.Unlock()
	sid, _, _, err := windows.LookupSID("", name)
	if err != nil {
		return nil // gone already
	}
	var errs []error
	if procDeleteProfileW.Find() == nil {
		s, _ := windows.UTF16PtrFromString(sid.String())
		if r, _, e := procDeleteProfileW.Call(uintptr(unsafe.Pointer(s)), 0, 0); r == 0 && !errors.Is(e, windows.ERROR_FILE_NOT_FOUND) {
			errs = append(errs, fmt.Errorf("DeleteProfile: %w", e))
		}
	}
	removeAllAccountRights(sid)
	n, _ := windows.UTF16PtrFromString(name)
	if r, _, _ := procNetUserDel.Call(0, uintptr(unsafe.Pointer(n))); r != 0 && r != nerrUserNotFound {
		errs = append(errs, fmt.Errorf("NetUserDel %s: %w", name, windows.Errno(r)))
	}
	return errors.Join(errs...)
}

// ---- LSA account rights -------------------------------------------------

func lsaString16(s string) (lsaUnicodeString, []uint16) {
	u, _ := windows.UTF16FromString(s)
	return lsaUnicodeString{Length: uint16((len(u) - 1) * 2), MaximumLength: uint16(len(u) * 2), Buffer: &u[0]}, u
}

func openPolicy() (windows.Handle, error) {
	var attrs lsaObjectAttributes
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var policy windows.Handle
	st, _, _ := procLsaOpenPolicy.Call(0, uintptr(unsafe.Pointer(&attrs)),
		policyCreateAccount|policyLookupNames, uintptr(unsafe.Pointer(&policy)))
	if err := lsaErr(st); err != nil {
		return 0, fmt.Errorf("LsaOpenPolicy: %w", err)
	}
	return policy, nil
}

func addAccountRight(sid *windows.SID, right string) error {
	policy, err := openPolicy()
	if err != nil {
		return err
	}
	defer procLsaClose.Call(uintptr(policy))
	us, keep := lsaString16(right)
	st, _, _ := procLsaAddAccountRights.Call(uintptr(policy), uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(&us)), 1)
	_ = keep
	return lsaErr(st)
}

func removeAllAccountRights(sid *windows.SID) {
	policy, err := openPolicy()
	if err != nil {
		return
	}
	defer procLsaClose.Call(uintptr(policy))
	procLsaRemoveAccountRights.Call(uintptr(policy), uintptr(unsafe.Pointer(sid)), 1 /* AllRights */, 0, 0)
}

// ---- S4U logon -----------------------------------------------------------

// lsaString is LSA_STRING (ANSI).
type lsaString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *byte
}

func newLsaString(s string) (lsaString, []byte) {
	b := append([]byte(s), 0)
	return lsaString{Length: uint16(len(s)), MaximumLength: uint16(len(b)), Buffer: &b[0]}, b
}

// tokenSource is TOKEN_SOURCE.
type tokenSource struct {
	SourceName       [8]byte
	SourceIdentifier windows.LUID
}

// quotaLimits is QUOTA_LIMITS.
type quotaLimits struct {
	PagedPoolLimit        uintptr
	NonPagedPoolLimit     uintptr
	MinimumWorkingSetSize uintptr
	MaximumWorkingSetSize uintptr
	PagefileLimit         uintptr
	TimeLimit             int64
}

// s4uLogon is MSV1_0_S4U_LOGON (strings follow the struct in the same buffer).
type s4uLogon struct {
	MessageType       uint32
	Flags             uint32
	UserPrincipalName lsaUnicodeString
	DomainName        lsaUnicodeString
}

var (
	lsaOnce   sync.Once
	lsaHandle windows.Handle
	lsaPkg    uint32
	lsaErrV   error
	lsaMu     sync.Mutex
)

func lsaInit() error {
	lsaOnce.Do(func() {
		name, keep := newLsaString("longarmd")
		var mode uint32
		st, _, _ := procLsaRegisterLogonProcess.Call(uintptr(unsafe.Pointer(&name)), uintptr(unsafe.Pointer(&lsaHandle)), uintptr(unsafe.Pointer(&mode)))
		_ = keep
		if err := lsaErr(st); err != nil {
			lsaErrV = fmt.Errorf("LsaRegisterLogonProcess (per-user isolation needs LocalSystem): %w", err)
			return
		}
		pkg, keep2 := newLsaString("MICROSOFT_AUTHENTICATION_PACKAGE_V1_0")
		st, _, _ = procLsaLookupAuthenticationPackage.Call(uintptr(lsaHandle), uintptr(unsafe.Pointer(&pkg)), uintptr(unsafe.Pointer(&lsaPkg)))
		_ = keep2
		if err := lsaErr(st); err != nil {
			lsaErrV = fmt.Errorf("LsaLookupAuthenticationPackage: %w", err)
		}
	})
	return lsaErrV
}

// s4uToken logs name on with S4U (batch logon type) and returns a primary
// token.
func s4uToken(name string) (windows.Token, error) {
	if err := lsaInit(); err != nil {
		return 0, err
	}
	var buf [256]uint16
	n := uint32(len(buf))
	if err := windows.GetComputerName(&buf[0], &n); err != nil {
		return 0, err
	}
	domain := windows.UTF16ToString(buf[:n])
	user16 := windows.StringToUTF16(name)
	dom16 := windows.StringToUTF16(domain)
	ul, dl := (len(user16)-1)*2, (len(dom16)-1)*2
	size := int(unsafe.Sizeof(s4uLogon{})) + ul + dl + 4
	mem := make([]uint64, (size+7)/8+1) // 8-byte aligned
	base := unsafe.Pointer(&mem[0])
	hdr := (*s4uLogon)(base)
	hdr.MessageType = msv1_0S4ULogon
	off := unsafe.Sizeof(s4uLogon{})
	up := (*uint16)(unsafe.Add(base, off))
	copy(unsafe.Slice(up, len(user16)-1), user16[:len(user16)-1])
	hdr.UserPrincipalName = lsaUnicodeString{Length: uint16(ul), MaximumLength: uint16(ul), Buffer: up}
	dp := (*uint16)(unsafe.Add(base, off+uintptr(ul)))
	copy(unsafe.Slice(dp, len(dom16)-1), dom16[:len(dom16)-1])
	hdr.DomainName = lsaUnicodeString{Length: uint16(dl), MaximumLength: uint16(dl), Buffer: dp}

	origin, keep := newLsaString("longarmd")
	var src tokenSource
	copy(src.SourceName[:], "longarmd")
	procAllocLocallyUniqueI.Call(uintptr(unsafe.Pointer(&src.SourceIdentifier)))
	var profile uintptr
	var profileLen uint32
	var logonID windows.LUID
	var token windows.Token
	var quotas quotaLimits
	var sub int32
	lsaMu.Lock()
	st, _, _ := procLsaLogonUser.Call(uintptr(lsaHandle), uintptr(unsafe.Pointer(&origin)), logonTypeBatch, uintptr(lsaPkg),
		uintptr(base), uintptr(size), 0, uintptr(unsafe.Pointer(&src)), uintptr(unsafe.Pointer(&profile)), uintptr(unsafe.Pointer(&profileLen)),
		uintptr(unsafe.Pointer(&logonID)), uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&quotas)), uintptr(unsafe.Pointer(&sub)))
	lsaMu.Unlock()
	_ = keep
	if profile != 0 {
		procLsaFreeReturnBuffer.Call(profile)
	}
	if err := lsaErr(st); err != nil {
		return 0, fmt.Errorf("LsaLogonUser(S4U %s): %w (substatus 0x%x)", name, err, uint32(sub))
	}
	defer token.Close()
	var primary windows.Token
	if err := windows.DuplicateTokenEx(token, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenPrimary, &primary); err != nil {
		return 0, fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	return primary, nil
}

// profileInfo is PROFILEINFOW.
type profileInfo struct {
	Size        uint32
	Flags       uint32
	UserName    *uint16
	ProfilePath *uint16
	DefaultPath *uint16
	ServerName  *uint16
	PolicyPath  *uint16
	Profile     windows.Handle
}

// UserSession is a logged-on account: a primary token with the profile
// loaded. Close it when the processes started with it have exited.
type UserSession struct {
	Token   windows.Token
	Home    string
	Env     []string // the account's environment (CreateEnvironmentBlock)
	name    string
	profile windows.Handle
	once    sync.Once
}

var privOnce sync.Once

// enablePrivileges enables what loading profiles and creating processes as
// another user needs (present but disabled in LocalSystem's token).
func enablePrivileges() {
	privOnce.Do(func() {
		var tok windows.Token
		if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
			return
		}
		defer tok.Close()
		for _, p := range []string{"SeBackupPrivilege", "SeRestorePrivilege", "SeAssignPrimaryTokenPrivilege",
			"SeIncreaseQuotaPrivilege", "SeTcbPrivilege"} {
			var luid windows.LUID
			if windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(p), &luid) != nil {
				continue
			}
			tp := windows.Tokenprivileges{PrivilegeCount: 1}
			tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
			windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil)
		}
	})
}

// Logon starts a session for acct: S4U token + loaded profile.
func Logon(acct *ptypes.Account) (*UserSession, error) {
	enablePrivileges()
	tok, err := s4uToken(acct.Name)
	if err != nil {
		return nil, err
	}
	pi := profileInfo{Flags: piNoUI, UserName: windows.StringToUTF16Ptr(acct.Name)}
	pi.Size = uint32(unsafe.Sizeof(pi))
	if r, _, e := procLoadUserProfileW.Call(uintptr(tok), uintptr(unsafe.Pointer(&pi))); r == 0 {
		tok.Close()
		return nil, fmt.Errorf("LoadUserProfile %s: %w", acct.Name, e)
	}
	s := &UserSession{Token: tok, name: acct.Name, profile: pi.Profile}
	if dir, err := tok.GetUserProfileDirectory(); err == nil {
		s.Home = dir
	}
	if env, err := tok.Environ(false); err == nil {
		s.Env = env
	}
	return s, nil
}

// Close unloads the profile and closes the token.
func (s *UserSession) Close() {
	s.once.Do(func() {
		if s.profile != 0 {
			procUnloadUserProfile.Call(uintptr(s.Token), uintptr(s.profile))
		}
		s.Token.Close()
	})
}

// AccountEnv returns the base environment of acct's processes.
func AccountEnv(acct *ptypes.Account) ([]string, error) {
	s, err := Logon(acct)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.Env, nil
}

// TokenSID returns the user SID of a process ("" if unknown).
func ProcessUserSID(pid int) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return ""
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return ""
	}
	return u.User.Sid.String()
}

// CanIsolate reports whether this process can run workloads as other
// accounts (LocalSystem: SeTcbPrivilege for S4U logons, SeAssignPrimaryToken).
func CanIsolate() error {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !u.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return errors.New("per-user isolation needs longarmd to run as LocalSystem (install the service with SERVICE_ACCOUNT=LocalSystem / longarmd service install --system)")
	}
	return nil
}
