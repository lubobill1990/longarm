//go:build !windows

package longarmd

import (
	"errors"
	"os"
	"os/exec"
)

type cmdProc struct{ cmd *exec.Cmd }

func (p cmdProc) PID() int { return p.cmd.Process.Pid }
func (p cmdProc) Wait() (int, error) {
	err := p.cmd.Wait()
	if p.cmd.ProcessState != nil {
		return p.cmd.ProcessState.ExitCode(), err
	}
	return -1, err
}

// startSupervised starts longarmd for a SessionHost (EnvSupervised set).
func startSupervised(exe string, args []string) (Proc, error) {
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), EnvSupervised+"=1")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmdProc{cmd}, nil
}

var errSessionWindowsOnly = errors.New("the user-session worker task is Windows-only (Linux: a systemd user unit, macOS: a LaunchAgent)")

// Schtasks is Windows' Task Scheduler; elsewhere every call fails.
type Schtasks struct{}

func (Schtasks) Register(string, []byte) error { return errSessionWindowsOnly }
func (Schtasks) Delete(string) error           { return errSessionWindowsOnly }
func (Schtasks) Run(string) error              { return errSessionWindowsOnly }
func (Schtasks) End(string) error              { return errSessionWindowsOnly }
func (Schtasks) Exists(string) bool            { return false }

// CurrentUserSID is Windows-only.
func CurrentUserSID() (string, error) { return "", errSessionWindowsOnly }

// ProfileDirOfSID is Windows-only.
func ProfileDirOfSID(string) (string, error) { return "", errSessionWindowsOnly }

// TokenProfileDir is the user's home elsewhere.
func TokenProfileDir() (string, error) { return os.UserHomeDir() }
