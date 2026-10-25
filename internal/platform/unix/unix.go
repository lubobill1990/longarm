//go:build linux || darwin

// Package unix holds the code shared by the Linux and macOS platform
// implementations: process-group containment, detaching, flock, statfs.
package unix

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"

	xunix "golang.org/x/sys/unix"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
)

// KilledExitCode is recorded for jobs terminated through Kill (128+SIGKILL).
const KilledExitCode = 137

// Container contains a job's process tree in its own process group.
//
// Limits (documented P0a gaps): a descendant that calls setsid()/setpgid()
// escapes the group; there is no memory limit. cgroup v2 containment is the
// planned upgrade on Linux.
type Container struct {
	mu   sync.Mutex
	pgid int
	opts ptypes.ContainerOptions
	// PreStart lets the OS-specific package add SysProcAttr fields
	// (e.g. Pdeathsig on Linux).
	PreStart func(*exec.Cmd)
	// ListPIDs lists all processes (set by the OS-specific package); used to
	// kill a terminal session, whose shell puts every job into its own
	// process group.
	ListPIDs func() []int
	// GroupLive reports whether process group pgid has a member that is
	// not a zombie (optional; macOS answers kill(-pgid) with EPERM when
	// all members are zombies, i.e. already dead).
	GroupLive func(pgid int) bool
	// sid is the session id of a StartPTY container (= the leader's pid).
	sid int
}

// NewContainer returns an empty container.
func NewContainer(opts ptypes.ContainerOptions) *Container {
	return &Container{opts: opts}
}

// Start starts cmd as the leader of a new process group.
func (c *Container) Start(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if c.PreStart != nil {
		c.PreStart(cmd)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	c.mu.Lock()
	c.pgid = cmd.Process.Pid
	c.mu.Unlock()
	return nil
}

// Kill sends SIGKILL to the whole process group and, for a PTY container,
// to every process of its terminal session (background jobs of an
// interactive shell run in their own process groups).
func (c *Container) Kill() error {
	c.mu.Lock()
	pgid, sid := c.pgid, c.sid
	c.mu.Unlock()
	if sid > 0 && c.ListPIDs != nil {
		for round := 0; round < 3; round++ { // catch processes forked meanwhile
			for _, pid := range c.ListPIDs() {
				if s, err := xunix.Getsid(pid); err == nil && s == sid && pid != os.Getpid() {
					syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
	}
	if pgid <= 0 {
		return nil
	}
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if errors.Is(err, syscall.EPERM) && c.GroupLive != nil && !c.GroupLive(pgid) {
		return nil // only unreaped zombies left
	}
	return err
}

// PIDs is not available for process groups without scanning; returns nil.
func (c *Container) PIDs() ([]int, error) { return nil, nil }

// Close kills whatever is left in the group, mirroring KILL_ON_JOB_CLOSE.
func (c *Container) Close() error {
	_ = c.Kill()
	c.mu.Lock()
	c.pgid = 0
	c.sid = 0
	c.mu.Unlock()
	return nil
}

// Alive reports whether pid exists (signal 0 succeeds or is merely denied).
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// KillProcess SIGKILLs the process group led by pid (job processes are group
// leaders) and the process itself.
func KillProcess(pid int) error {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	err := syscall.Kill(pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// StartDetached starts cmd in a new session so it survives the parent and
// its terminal/process group. (Under systemd, the unit needs KillMode=process
// so stopping longarmd does not kill the shims; launchd: AbandonProcessGroup.)
func StartDetached(newCmd func() *exec.Cmd) (*exec.Cmd, error) {
	cmd := newCmd()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, cmd.Start()
}

// LockFile takes an exclusive, non-blocking flock on f.
func LockFile(f *os.File) error {
	return xunix.Flock(int(f.Fd()), xunix.LOCK_EX|xunix.LOCK_NB)
}

// DiskUsage returns available and total bytes of the filesystem at path.
func DiskUsage(path string) (free, total uint64, err error) {
	var st xunix.Statfs_t
	if err := xunix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil
}

// KillGroup SIGKILLs process group pgid.
func KillGroup(pgid int) {
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
