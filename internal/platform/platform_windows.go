//go:build windows

package platform

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"

	win "github.com/lubobill1990/longarm/internal/platform/windows"
)

// KilledExitCode is the exit code recorded for killed jobs.
const KilledExitCode = win.KilledExitCode

func NewContainer(opts ContainerOptions) (Container, error) { return win.NewContainer(opts) }

func ProcessStartTime(pid int) (uint64, error) { return win.ProcessStartTime(pid) }
func ProcessAlive(pid int, start uint64) bool  { return win.ProcessAlive(pid, start) }
func KillProcess(pid int) error                { return win.KillProcess(pid) }

func StartDetached(newCmd func() *exec.Cmd) (*exec.Cmd, error) { return win.StartDetached(newCmd) }

func LockFile(f *os.File) error                             { return win.LockFile(f) }
func DiskUsage(path string) (free, total uint64, err error) { return win.DiskUsage(path) }
func ReadFileShared(path string) ([]byte, error)            { return win.ReadFileShared(path) }
func OpenShared(path string) (*os.File, error)              { return win.OpenShared(path) }
func ReplaceFile(src, dst string) error                     { return win.ReplaceFile(src, dst) }

func SampleSystem() (SysSample, error)     { return win.SampleSystem() }
func ListProcesses() ([]ProcSample, error) { return win.ListProcesses() }

func InstallService(cfg ServiceConfig) error          { return win.InstallService(cfg) }
func UninstallService(name string) error              { return win.UninstallService(name) }
func StartService(name string) error                  { return win.StartService(name) }
func StopService(name string) error                   { return win.StopService(name) }
func QueryService(name string) (ServiceStatus, error) { return win.QueryService(name) }
func RunAsService(name string, fn func(context.Context) error) (bool, error) {
	return win.RunAsService(name, fn)
}

// SetupConsole gives a console-less process a hidden UTF-8 console that job
// processes inherit (see windows.SetupConsole). No-op elsewhere.
func SetupConsole() error { return win.SetupConsole() }

// OwnConsole detaches from any inherited console and sets up a private
// hidden UTF-8 one (used by `longarmd conhost`).
func OwnConsole() error {
	win.FreeConsole()
	return win.SetupConsole()
}

// KillProcessGroup is a no-op on Windows: the Job Object (KILL_ON_JOB_CLOSE)
// already took the whole tree down with the shim.
func KillProcessGroup(int) {}

// StartPTY starts a process on a pseudo console inside ctr.
func StartPTY(ctr Container, o PTYOptions) (PTY, error) {
	c, ok := ctr.(*win.Container)
	if !ok {
		return nil, errors.New("StartPTY: not a platform container")
	}
	return c.StartPTY(o)
}

// ListenLocal listens on a local stream channel only the current user can
// reach (Windows: named pipe \.\pipe\longarm-<name> with an owner-only
// DACL). The returned address is passed to DialLocal.
func ListenLocal(name string) (net.Listener, string, error) { return win.ListenLocal(name) }

// DialLocal connects to a ListenLocal address.
func DialLocal(ctx context.Context, addr string) (net.Conn, error) { return win.DialLocal(ctx, addr) }

// UTF8Console sets the attached console's code pages to UTF-8 (no-op
// elsewhere).
func UTF8Console() { win.UTF8Console() }

// SetCmdLine makes cmd pass line verbatim as its command line (Windows;
// needed for cmd.exe /c with a raw ssh command). Unsupported elsewhere.
func SetCmdLine(cmd *exec.Cmd, line string) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CmdLine = line
	return nil
}

// TerminalOutput enables VT processing and UTF-8 on the console behind f
// (longarm attach); returns a restore function. No-op elsewhere.
func TerminalOutput(f *os.File) (func(), error) {
	return win.TerminalOutput(windows.Handle(f.Fd()))
}

// SecureDir creates dir readable only by SYSTEM, Administrators and user.
func SecureDir(dir, user string) error { return win.SecureDir(dir, user) }

// GrantServiceLogon gives account "Log on as a service".
func GrantServiceLogon(account string) error { return win.GrantServiceLogon(account) }
