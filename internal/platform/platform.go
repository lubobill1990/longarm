// Package platform is the OS abstraction used by longarmd and longarm. The per-OS
// implementations live in internal/platform/{windows,linux,darwin} (plus
// internal/platform/unix for code shared by Linux and macOS) behind build
// tags; this package re-exports them under one API.
//
//	capability                 Windows                 Linux                   macOS
//	process-tree containment   Job Object (KILL_ON_    process group +         process group
//	                           JOB_CLOSE, mem limit)   Pdeathsig (no mem cap)  (no mem cap)
//	service                    SCM as a user account   systemd unit            launchd daemon
//	metrics                    kernel32                /proc                   gopsutil
package platform

import (
	"os/exec"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
)

type (
	SysSample        = ptypes.SysSample
	ProcSample       = ptypes.ProcSample
	ContainerOptions = ptypes.ContainerOptions
	ServiceConfig    = ptypes.ServiceConfig
	ServiceStatus    = ptypes.ServiceStatus
)

// Container owns a job's whole process tree.
type Container interface {
	// Start starts cmd inside the container (cmd.SysProcAttr may be modified).
	Start(cmd *exec.Cmd) error
	// Kill terminates every process in the container.
	Kill() error
	// PIDs lists processes in the container when the platform can tell
	// (Windows); nil otherwise.
	PIDs() ([]int, error)
	// Close releases the container; anything still running is killed.
	Close() error
}

// Service states.
const (
	ServiceNotInstalled = ptypes.ServiceNotInstalled
	ServiceStopped      = ptypes.ServiceStopped
	ServiceRunning      = ptypes.ServiceRunning
	ServicePending      = ptypes.ServicePending
	ServiceUnknown      = ptypes.ServiceUnknown
)

// PTYOptions configures StartPTY.
type PTYOptions = ptypes.PTYOptions

// PTY is a process running on a pseudo terminal (ConPTY on Windows, a pty
// on Unix) inside a Container.
type PTY interface {
	// Read returns terminal output (VT sequences, UTF-8); io.EOF after the
	// process tree is gone (Unix) or after Close (Windows).
	Read(p []byte) (int, error)
	// Write sends keyboard input.
	Write(p []byte) (int, error)
	Resize(cols, rows uint16) error
	Pid() int
	// Wait waits for the process and returns its exit code.
	Wait() (int, error)
	// Close shuts the terminal down so that Read drains and hits EOF.
	Close() error
	// Release frees the remaining handles after the last Read.
	Release()
}
