//go:build linux || darwin

package unix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/creack/pty"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
)

// PTY is a process running on a pseudo terminal.
type PTY struct {
	f   *os.File // master
	cmd *exec.Cmd

	waitOnce sync.Once
	code     int
	waitErr  error
}

// StartPTY starts o.Argv on a new pty as a session leader (setsid; the pty
// becomes its controlling terminal), so its process group - pgid = pid - is
// the container.
func (c *Container) StartPTY(o ptypes.PTYOptions) (*PTY, error) {
	if len(o.Argv) == 0 {
		return nil, errors.New("empty argv")
	}
	cmd := exec.Command(o.Argv[0], o.Argv[1:]...)
	cmd.Dir, cmd.Env = o.Dir, o.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if c.PreStart != nil {
		c.PreStart(cmd)
	}
	cols, rows := o.Cols, o.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 25
	}
	f, err := pty.StartWithAttrs(cmd, &pty.Winsize{Cols: cols, Rows: rows}, cmd.SysProcAttr)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.pgid, c.sid = cmd.Process.Pid, cmd.Process.Pid
	c.mu.Unlock()
	return &PTY{f: f, cmd: cmd}, nil
}

// Read reads terminal output; EIO (all slave fds closed) reads as EOF.
func (p *PTY) Read(b []byte) (int, error) {
	n, err := p.f.Read(b)
	if err != nil && errors.Is(err, syscall.EIO) {
		err = io.EOF
	}
	return n, err
}

// Write sends keyboard input.
func (p *PTY) Write(b []byte) (int, error) { return p.f.Write(b) }

// Resize sets the window size (the kernel sends SIGWINCH).
func (p *PTY) Resize(cols, rows uint16) error {
	return pty.Setsize(p.f, &pty.Winsize{Cols: cols, Rows: rows})
}

// Pid of the process.
func (p *PTY) Pid() int { return p.cmd.Process.Pid }

// Wait waits for the process and returns its exit code (128+N for signals).
func (p *PTY) Wait() (int, error) {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		ps := p.cmd.ProcessState
		if ps == nil {
			p.code, p.waitErr = -1, err
			return
		}
		p.code = ps.ExitCode()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			p.code = 128 + int(ws.Signal())
		}
	})
	return p.code, p.waitErr
}

// Close is a no-op on Unix: Read returns EOF once every process holding
// the slave side is gone (the container kill takes care of that).
func (p *PTY) Close() error { return nil }

// Release closes the master.
func (p *PTY) Release() { p.f.Close() }

// ListenLocal listens on a unix socket in a private per-user directory
// (short path: sun_path is limited to ~104 bytes).
func ListenLocal(name string) (net.Listener, string, error) {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("longarm-%d", os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o077 != 0 {
		return nil, "", fmt.Errorf("%s is not a private directory of this user", dir)
	}
	addr := filepath.Join(dir, name+".sock")
	os.Remove(addr)
	ln, err := net.Listen("unix", addr)
	if err != nil {
		return nil, "", err
	}
	os.Chmod(addr, 0o600)
	return ln, addr, nil
}

// DialLocal connects to an address returned by ListenLocal.
func DialLocal(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", addr)
}
