//go:build windows

package msiupgrade

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/stopsignal"
)

// WindowsEnv is the helper's Env on Windows.
func WindowsEnv() Env {
	return Env{
		InstallDir:   RecordedInstallDir,
		CurrentSID:   currentSID,
		IsAdmin:      platform.IsLocalAdmin,
		ServiceNames: workerServices,
		StateDir:     StateDir,
		System:       WindowsSystem{},
		Services:     SCM{},
		Tasks:        Schtasks{},
	}
}

// RecordedInstallDir is the install dir the MSI recorded in HKLM (a value
// only administrators can write), which must be writable by administrators
// only: never a path from a property.
func RecordedInstallDir() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Longarm`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", err
	}
	defer k.Close()
	dir, _, err := k.GetStringValue("InstallDir")
	if err != nil {
		return "", err
	}
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) || strings.Contains(dir, "..") || strings.HasPrefix(dir, `\\`) {
		return "", fmt.Errorf("recorded install dir %q is not a local absolute path", dir)
	}
	if err := platform.TrustedSystemPath(dir); err != nil {
		return "", fmt.Errorf("install dir %s: %w", dir, err)
	}
	return dir, nil
}

// StateDir is ProgramData\Longarm, secured first when someone else made it.
func StateDir() (string, error) {
	pd, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return "", err
	}
	base := filepath.Join(pd, "Longarm")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	if platform.TrustedSystemPath(base) != nil {
		if err := platform.SecureDir(base, ""); err != nil {
			return "", err
		}
	}
	return base, nil
}

func currentSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// workerServices: longarmd-admin, LongarmUpdate and the RUN=service longarmd (the
// recorded ServiceName, when RunMode is service).
func workerServices() []string {
	// LongarmUpdate too: an install it started stops it (the installer runs
	// in its own scheduled task), and a rollback must start it again
	names := []string{"longarmd-admin", "LongarmUpdate"}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Longarm`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return names
	}
	defer k.Close()
	run, _, _ := k.GetStringValue("RunMode")
	acct, _, _ := k.GetStringValue("ServiceAccount")
	name, _, _ := k.GetStringValue("ServiceName")
	if name == "" {
		name = "longarmd"
	}
	if (run == "service" || run == "" && acct != "") && ServiceName(name) && !strings.EqualFold(name, "longarmd-admin") {
		names = append(names, name)
	}
	return names
}

// WindowsSystem is System on Windows.
type WindowsSystem struct{}

func (WindowsSystem) Processes() ([]Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var out []Proc
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		name := strings.ToLower(windows.UTF16ToString(pe.ExeFile[:]))
		if name != "longarm-tray.exe" && name != "longarmd.exe" {
			continue // the stopped kinds only: cheap
		}
		if p, ok := describe(int(pe.ProcessID)); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func describe(pid int) (Proc, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return Proc{}, false
	}
	defer windows.CloseHandle(h)
	p := Proc{PID: pid}
	exe, err := imagePath(h)
	if err != nil {
		return Proc{}, false
	}
	p.Exe = exe
	p.Start, _ = startTime(h)
	windows.ProcessIdToSessionId(uint32(pid), &p.Session)
	var tok windows.Token
	if windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok) == nil {
		if u, err := tok.GetTokenUser(); err == nil {
			p.SID = u.User.Sid.String()
		}
		tok.Close()
	}
	if line, err := commandLine(h); err == nil {
		if args, err := windows.DecomposeCommandLine(line); err == nil && len(args) > 0 {
			p.Args = append([]string{}, args[1:]...)
		}
	}
	return p, true
}

func imagePath(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

func startTime(h windows.Handle) (uint64, error) {
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0, err
	}
	return uint64(c.HighDateTime)<<32 | uint64(c.LowDateTime), nil
}

// commandLine reads another process's command line
// (ProcessCommandLineInformation: PROCESS_QUERY_LIMITED_INFORMATION is
// enough).
func commandLine(h windows.Handle) (string, error) {
	var need uint32
	buf := make([]byte, 4096)
	for i := 0; i < 2; i++ {
		err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&buf[0]), uint32(len(buf)), &need)
		if err == nil {
			us := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0]))
			return us.String(), nil
		}
		if need <= uint32(len(buf)) || need > 1<<20 {
			return "", err
		}
		buf = make([]byte, need)
	}
	return "", errors.New("command line: buffer")
}

// open opens p for waiting and ending, checking it is still the same
// process (start time) with its image in the same place.
func open(p Proc, access uint32) (windows.Handle, error) {
	h, err := windows.OpenProcess(access|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(p.PID))
	if err != nil {
		return 0, err
	}
	if st, err := startTime(h); err != nil || (p.Start != 0 && st != p.Start) {
		windows.CloseHandle(h)
		return 0, errors.New("not the same process any more")
	}
	if exe, err := imagePath(h); err != nil || !strings.EqualFold(exe, p.Exe) {
		windows.CloseHandle(h)
		return 0, errors.New("its image moved")
	}
	return h, nil
}

func (WindowsSystem) Signal(p Proc) bool {
	ok, _ := stopsignal.Signal(p.PID)
	return ok
}

func (WindowsSystem) WaitExit(p Proc, d time.Duration) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(p.PID))
	if err != nil {
		return true // gone
	}
	defer windows.CloseHandle(h)
	if st, err := startTime(h); err == nil && p.Start != 0 && st != p.Start {
		return true // the pid is somebody else's now
	}
	r, err := windows.WaitForSingleObject(h, uint32(d/time.Millisecond))
	return err == nil && r == windows.WAIT_OBJECT_0
}

// Terminate ends p with exit code 0 (a scheduled task's "restart on
// failure" does not start a worker host again in the middle of the
// upgrade).
func (WindowsSystem) Terminate(p Proc) error {
	h, err := open(p, windows.PROCESS_TERMINATE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 0); err != nil {
		return err
	}
	windows.WaitForSingleObject(h, 5000)
	return nil
}

// InUse reports whether a process holds path as its image (a running
// image can't be opened for writing).
func InUse(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED) && !readOnly(path)
	}
	windows.CloseHandle(h)
	return false
}

func readOnly(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().Perm()&0o200 == 0
}

// SCM is Services on Windows.
type SCM struct{}

func (SCM) Running(name string) (bool, error) {
	st, err := platform.QueryService(name)
	if err != nil {
		return false, err
	}
	return st == platform.ServiceRunning || st == platform.ServicePending, nil
}
func (SCM) Stop(name string) error  { return platform.StopService(name) }
func (SCM) Start(name string) error { return platform.StartService(name) }

// Schtasks is Tasks on Windows (System32\schtasks.exe).
type Schtasks struct{}

func schtasks(args ...string) error {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	cmd := exec.Command(filepath.Join(sys, "schtasks.exe"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (Schtasks) Exists(name string) bool { return schtasks("/Query", "/TN", name) == nil }
func (Schtasks) Run(name string) error   { return schtasks("/Run", "/TN", name) }

// RawCommandLine is this process's command line as Windows Installer
// passed it (the data after DataMarker is taken verbatim).
func RawCommandLine() string { return windows.UTF16PtrToString(windows.GetCommandLine()) }
