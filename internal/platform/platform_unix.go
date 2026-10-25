//go:build linux || darwin

package platform

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"

	"github.com/lubobill1990/longarm/internal/platform/unix"
)

// KilledExitCode is the exit code recorded for killed jobs.
const KilledExitCode = unix.KilledExitCode

func KillProcess(pid int) error { return unix.KillProcess(pid) }

func StartDetached(newCmd func() *exec.Cmd) (*exec.Cmd, error) { return unix.StartDetached(newCmd) }

func LockFile(f *os.File) error                             { return unix.LockFile(f) }
func DiskUsage(path string) (free, total uint64, err error) { return unix.DiskUsage(path) }
func ReadFileShared(path string) ([]byte, error)            { return os.ReadFile(path) }
func OpenShared(path string) (*os.File, error)              { return os.Open(path) }
func ReplaceFile(src, dst string) error                     { return os.Rename(src, dst) }

// SetupConsole is Windows-only; on Unix, output encoding follows the locale.
func SetupConsole() error { return nil }

// OwnConsole is Windows-only.
func OwnConsole() error { return nil }

// KillProcessGroup SIGKILLs every process left in the job's process group
// (job processes lead their own group, so pgid = the job's main pid). Safe
// after the leader died: a pgid is not reused while the group has members.
func KillProcessGroup(pgid int) {
	if pgid > 0 {
		unix.KillGroup(pgid)
	}
}

// StartPTY starts a process on a pty inside ctr (as session leader).
func StartPTY(ctr Container, o PTYOptions) (PTY, error) {
	c, ok := ctr.(*unix.Container)
	if !ok {
		return nil, errors.New("StartPTY: not a platform container")
	}
	return c.StartPTY(o)
}

// ListenLocal listens on a unix socket (mode 0600) in a private per-user
// directory under the temp dir. The address is passed to DialLocal.
func ListenLocal(name string) (net.Listener, string, error) { return unix.ListenLocal(name) }

// DialLocal connects to a ListenLocal address.
func DialLocal(ctx context.Context, addr string) (net.Conn, error) { return unix.DialLocal(ctx, addr) }

// UTF8Console is Windows-only.
func UTF8Console() {}

// SetCmdLine is Windows-only.
func SetCmdLine(*exec.Cmd, string) error { return errors.New("raw command lines are Windows-only") }

// TerminalOutput is Windows-only (Unix terminals speak VT already).
func TerminalOutput(*os.File) (func(), error) { return func() {}, nil }
