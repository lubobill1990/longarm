//go:build windows

package updatesvc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/lubobill1990/longarm/internal/msiupgrade"
	"github.com/lubobill1990/longarm/internal/platform/ptypes"
	win "github.com/lubobill1990/longarm/internal/platform/windows"
	"github.com/lubobill1990/longarm/internal/update"
)

const (
	systemSID = "S-1-5-18"
	// pipeSDDL: SYSTEM full control; Administrators and authenticated
	// users read and write (0x12019b = generic read and write without
	// FILE_CREATE_PIPE_INSTANCE: nobody else can add a server instance of
	// the pipe); network logons denied (the pipe also rejects remote
	// clients).
	pipeSDDL = "D:P(D;;GA;;;NU)(A;;GA;;;SY)(A;;0x12019b;;;BA)(A;;0x12019b;;;AU)"
	// dirSDDL: the downloads: SYSTEM only, nothing inherited.
	dirSDDL = "O:SYD:P(A;OICI;FA;;;SY)"
)

var (
	advapi32                       = windows.NewLazySystemDLL("advapi32.dll")
	procImpersonateNamedPipeClient = advapi32.NewProc("ImpersonateNamedPipeClient")
)

// ListenPipe creates the service's pipe. go-winio creates the first
// instance with FILE_CREATE (the NT form of FILE_FLAG_FIRST_PIPE_INSTANCE):
// when another process holds the name already - a squatter - this fails
// instead of joining its pipe.
func ListenPipe() (net.Listener, error) { return listenPipe(PipeName, pipeSDDL) }

func listenPipe(name, sddl string) (net.Listener, error) {
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: sddl, InputBufferSize: 16 << 10, OutputBufferSize: 16 << 10})
	if err != nil {
		return nil, fmt.Errorf("creating %s (is another program holding the name?): %w", name, err)
	}
	return ln, nil
}

type fder interface{ Fd() uintptr }

// IdentifyPipeClient is the connected client's token: its user, whether it
// is an elevated administrator or SYSTEM (ImpersonateNamedPipeClient on
// this thread only; clients connect at identification level).
func IdentifyPipeClient(c net.Conn) (Caller, error) {
	f, ok := c.(fder)
	if !ok {
		return Caller{}, errors.New("not a pipe")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if r, _, e := procImpersonateNamedPipeClient.Call(f.Fd()); r == 0 {
		return Caller{}, fmt.Errorf("ImpersonateNamedPipeClient: %w", e)
	}
	var tok windows.Token
	err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &tok)
	if rerr := windows.RevertToSelf(); rerr != nil {
		panic("RevertToSelf: " + rerr.Error()) // never go on as the client
	}
	if err != nil {
		return Caller{}, fmt.Errorf("the client's token: %w", err)
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return Caller{}, err
	}
	sid := u.User.Sid.String()
	if sid == "S-1-5-7" { // anonymous: connect at identification level
		return Caller{}, errors.New("an anonymous client")
	}
	c2 := Caller{SID: sid, System: sid == systemSID}
	if admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid); err == nil {
		c2.Admin, _ = tok.IsMember(admins) // enabled only: an elevated token
	}
	return c2, nil
}

// VerifyPipeServer checks the pipe's server process: the installed
// longarm-update.exe (in the install dir the MSI recorded, which only
// administrators can change) running as SYSTEM. Anything else is a
// squatter: ErrImpostor.
func VerifyPipeServer(c net.Conn) error {
	f, ok := c.(fder)
	if !ok {
		return errors.New("not a pipe")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(f.Fd()), &pid); err != nil {
		return fmt.Errorf("the update pipe's server: %w", err)
	}
	dir, err := msiupgrade.RecordedInstallDir()
	if err != nil {
		return fmt.Errorf("%w (no install dir recorded: %v)", ErrImpostor, err)
	}
	return verifyServerProcess(pid, filepath.Join(dir, BinaryName))
}

func verifyServerProcess(pid uint32, want string) error {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return fmt.Errorf("%w (pid %d: %v)", ErrImpostor, pid, err)
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return fmt.Errorf("%w (pid %d: %v)", ErrImpostor, pid, err)
	}
	exe := windows.UTF16ToString(buf[:n])
	if !strings.EqualFold(filepath.Clean(exe), filepath.Clean(want)) {
		return fmt.Errorf("%w: pid %d is %s, not %s", ErrImpostor, pid, exe, want)
	}
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("%w (pid %d's token: %v)", ErrImpostor, pid, err)
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil || u.User.Sid.String() != systemSID {
		return fmt.Errorf("%w: pid %d does not run as SYSTEM", ErrImpostor, pid)
	}
	return nil
}

func dialService(ctx context.Context) (net.Conn, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	c, err := winio.DialPipeAccessImpLevel(ctx, PipeName, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelIdentification)
	if err == nil {
		return c, nil
	}
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, os.ErrNotExist) {
		if st, qerr := win.QueryService(ServiceName); qerr == nil && st != ptypes.ServiceNotInstalled {
			return nil, fmt.Errorf("the %s service is installed but not running (%v)", ServiceName, st)
		}
		return nil, ErrNoService
	}
	return nil, err
}

// Installed reports whether this machine has the service (an MSI install
// with LongarmUpdate). Any account may ask (win.QueryService needs only
// SERVICE_QUERY_STATUS): a user-session longarmd and the tray too.
func Installed() bool {
	st, err := win.QueryService(ServiceName)
	return err == nil && st != ptypes.ServiceNotInstalled
}

// ---------------------------------------------------------------- settings

// RegistrySettings: HKLM\Software\Longarm (Hub, SessionUserSID: written by
// the installer) and HKLM\Software\Longarm\Update (Auto, IntervalHours,
// Channel: written by this service only; longarmd reads Channel, see
// MachineChannel).
type RegistrySettings struct{}

const (
	regKey       = `Software\Longarm`
	regUpdateKey = `Software\Longarm\Update`
)

func regString(name string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, regKey, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, _ := k.GetStringValue(name)
	return strings.TrimSpace(v)
}

func (RegistrySettings) Hub() (string, error) {
	h := regString("Hub")
	if h == "" {
		return "", errors.New("no hub recorded")
	}
	return h, nil
}

func (RegistrySettings) Owner() string {
	sid := regString("SessionUserSID")
	if !msiupgrade.UserSID(sid) {
		return ""
	}
	return sid
}

func (RegistrySettings) Auto() (bool, int) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, regUpdateKey, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return false, DefaultAutoHours
	}
	defer k.Close()
	on, _, _ := k.GetIntegerValue("Auto")
	h, _, _ := k.GetIntegerValue("IntervalHours")
	return on == 1, normHours(int(h))
}

func (RegistrySettings) Channel() string { return MachineChannel() }

func (RegistrySettings) SetChannel(channel string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, regUpdateKey, registry.SET_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("Channel", channel)
}

// MachineChannel is the release channel this machine follows (set through
// the update service: the tray's Client tab), normalized; stable when unset.
func MachineChannel() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, regUpdateKey, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return update.ChannelStable
	}
	defer k.Close()
	v, _, _ := k.GetStringValue("Channel")
	if ch, ok := update.NormalizeChannel(v); ok {
		return ch
	}
	return update.ChannelStable
}

func (RegistrySettings) SetAuto(on bool, hours int) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, regUpdateKey, registry.SET_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return err
	}
	defer k.Close()
	v := uint32(0)
	if on {
		v = 1
	}
	if err := k.SetDWordValue("Auto", v); err != nil {
		return err
	}
	return k.SetDWordValue("IntervalHours", uint32(normHours(hours)))
}

// ---------------------------------------------------------------- files

type winDirOps struct{}

func reparse(fi os.FileInfo) bool {
	if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return true
	}
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok && d.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return true
	}
	return false
}

func (winDirOps) Inspect(path string) (dirInfo, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return dirInfo{}, nil
	}
	if err != nil {
		return dirInfo{}, err
	}
	in := dirInfo{Exists: true, Dir: fi.IsDir(), Reparse: reparse(fi)}
	if in.Dir && !in.Reparse {
		in.Secured = systemOnly(path)
	}
	return in, nil
}

// systemOnly: owned by SYSTEM (or Administrators), a protected DACL with
// one ACE: full control for SYSTEM.
func systemOnly(path string) bool {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !win.SystemOwnerSID(owner) {
		return false
	}
	ctl, _, err := sd.Control()
	if err != nil || ctl&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		return false
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		return false
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	return sid.String() == systemSID && ace.Mask&0x1f01ff == 0x1f01ff
}

func (winDirOps) Create(path string) error {
	sd, err := windows.SecurityDescriptorFromString(dirSDDL)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	return windows.CreateDirectory(p, sa)
}

func (winDirOps) Remove(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if reparse(fi) || !fi.IsDir() {
		return retryRemove(func() error { return os.Remove(path) }) // the link itself, never its target
	}
	return retryRemove(func() error { return os.RemoveAll(path) })
}

// retryRemove retries a removal for a few seconds: a file another process
// still has open (msiexec closing the package, an anti-virus scan) can't be
// deleted for a moment. Missing = done.
func retryRemove(rm func() error) error {
	var err error
	for i := 0; i < 60; i++ {
		if err = rm(); err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

func (winDirOps) Trusted(path string) error { return win.TrustedSystemPath(path) }

func (winDirOps) Lock(path string) (io.ReadCloser, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// read sharing only: nobody can write, rename or delete it meanwhile
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var bi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bi); err != nil || bi.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || bi.NumberOfLinks != 1 {
		windows.CloseHandle(h)
		if err == nil {
			err = errors.New("not a plain file (reparse point, directory or hard link)")
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return os.NewFile(uintptr(h), path), nil
}

// ---------------------------------------------------------------- launcher

// TaskLauncher runs msiexec through the one-shot scheduled task
// InstallTaskName.
type TaskLauncher struct{}

func system32(name string) (string, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(sys, name), nil
}

func schtasks(args ...string) error {
	exe, err := system32("schtasks.exe")
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (TaskLauncher) Launch(msi, log string) error {
	msiexec, err := system32("msiexec.exe")
	if err != nil {
		return err
	}
	sc, err := system32("sc.exe")
	if err != nil {
		return err
	}
	x, err := installTaskXML(msiexec, sc, msi, log)
	if err != nil {
		return err
	}
	// schtasks /XML wants UTF-16
	u := utf16.Encode([]rune(x))
	b := make([]byte, 2+2*len(u))
	b[0], b[1] = 0xff, 0xfe
	for i, c := range u {
		b[2+2*i], b[3+2*i] = byte(c), byte(c>>8)
	}
	file := filepath.Join(filepath.Dir(log), "install-task.xml")
	os.Remove(file)
	if err := os.WriteFile(file, b, 0o600); err != nil {
		return err
	}
	defer os.Remove(file)
	if err := schtasks("/Create", "/TN", InstallTaskName, "/XML", file, "/F"); err != nil {
		return err
	}
	return schtasks("/Run", "/TN", InstallTaskName)
}

func (TaskLauncher) Outcome(log string) (int, bool, error) {
	b, err := win.ReadFileShared(log)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	code, done := msiLogOutcome(b)
	return code, done, nil
}

func (TaskLauncher) Forget() { schtasks("/Delete", "/TN", InstallTaskName, "/F") }

// ---------------------------------------------------------------- the service

// WindowsConfig is the service's Config on this machine: the workspace
// below ProgramData\Longarm, the registry settings, the hub's HTTPS
// release endpoints, the task launcher.
func WindowsConfig(version, publicKey string, log *slog.Logger) (Config, string, error) {
	root, err := msiupgrade.StateDir()
	if err != nil {
		return Config{}, "", err
	}
	ws := &FSWorkspace{Root: root, ops: winDirOps{}}
	up, err := ws.updatesDir()
	if err != nil {
		return Config{}, "", err
	}
	return Config{
		Version: version, Arch: runtime.GOARCH, PublicKey: publicKey,
		Settings:  RegistrySettings{},
		NewSource: func(hub string) (Source, error) { return NewHTTPSource(hub) },
		Workspace: ws, Launcher: TaskLauncher{}, Store: FileStore{Path: filepath.Join(up, "state.json")},
		Log: log,
	}, up, nil
}

// RunAsService runs fn as the Windows service name (false when this
// process is not a service).
func RunAsService(fn func(ctx context.Context) error) (bool, error) {
	return win.RunAsService(ServiceName, fn)
}

// RunningAsSystem reports whether this process is LocalSystem.
func RunningAsSystem() bool {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && u.User.Sid.String() == systemSID
}

// MayChangeAuto reports whether this process may turn automatic updates
// on or off (the service decides; this only greys out a switch): the
// machine's owner (the MSI's SessionUserSID) or an elevated administrator.
func MayChangeAuto() bool {
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		return false
	}
	if owner := (RegistrySettings{}).Owner(); owner != "" && strings.EqualFold(owner, u.User.Sid.String()) {
		return true
	}
	return tok.IsElevated()
}
