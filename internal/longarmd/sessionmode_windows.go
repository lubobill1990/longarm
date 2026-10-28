//go:build windows

package longarmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const createNoWindow = 0x08000000

type cmdProc struct{ cmd *exec.Cmd }

func (p cmdProc) PID() int { return p.cmd.Process.Pid }
func (p cmdProc) Wait() (int, error) {
	err := p.cmd.Wait()
	if p.cmd.ProcessState != nil {
		return p.cmd.ProcessState.ExitCode(), err
	}
	return -1, err
}

// startSupervised starts longarmd for a SessionHost: without a console
// window (CREATE_NO_WINDOW: a console of its own that is never shown, which
// job processes inherit), with EnvSupervised set.
func startSupervised(exe string, args []string) (Proc, error) {
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), EnvSupervised+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmdProc{cmd}, nil
}

// Schtasks is the TaskScheduler of Windows: schtasks.exe.
type Schtasks struct{}

func schtasks(args ...string) error {
	exe := filepath.Join(os.Getenv("SystemRoot"), "System32", "schtasks.exe")
	if os.Getenv("SystemRoot") == "" {
		exe = "schtasks.exe"
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (Schtasks) Register(name string, def []byte) error {
	f, err := os.CreateTemp("", "longarm-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(TaskXMLFile(def))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return schtasks("/Create", "/TN", name, "/XML", f.Name(), "/F")
}

func (Schtasks) Delete(name string) error { return schtasks("/Delete", "/TN", name, "/F") }
func (Schtasks) Run(name string) error    { return schtasks("/Run", "/TN", name) }
func (Schtasks) End(name string) error    { return schtasks("/End", "/TN", name) }
func (Schtasks) Exists(name string) bool  { return schtasks("/Query", "/TN", name) == nil }

// CurrentUserSID is this process's user (its token's).
func CurrentUserSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// ProfileDirOfSID is a user's profile dir (the ProfileList in the
// registry): the MSI's custom actions run as SYSTEM and find the
// installing user's data dir with it.
func ProfileDirOfSID(sid string) (string, error) {
	if err := ValidUserSID(sid); err != nil {
		return "", err
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList\`+sid, registry.QUERY_VALUE)
	if err != nil {
		return "", fmt.Errorf("no profile for %s: %w", sid, err)
	}
	defer k.Close()
	p, _, err := k.GetStringValue("ProfileImagePath")
	if err != nil {
		return "", err
	}
	p, err = registry.ExpandString(p)
	if err != nil || p == "" {
		return "", errors.New("profile of " + sid + " has no path")
	}
	return p, nil
}

// TokenProfileDir is the profile dir of this process's user (its token's,
// not %USERPROFILE%: the MSI's impersonated custom actions may not have the
// user's environment).
func TokenProfileDir() (string, error) {
	return windows.GetCurrentProcessToken().GetUserProfileDirectory()
}
