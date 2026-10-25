//go:build windows

// Package windows implements the Windows side of internal/platform: Job
// Object process-tree containment, process identity, the SCM service, and
// kernel32-based metrics sampling.
package windows

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
)

// KilledExitCode is the exit code given to processes terminated through Kill,
// chosen to match the conventional 128+SIGKILL on Unix.
const KilledExitCode = 137

// Container is a Job Object holding a job's whole process tree.
//
// The job is created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so when the
// owner (the per-job shim) exits for any reason - including being killed - the
// kernel terminates every process in the tree. The handle is not inheritable,
// so children cannot keep the job alive.
type Container struct {
	mu  sync.Mutex
	job windows.Handle
	// user: processes start as this logged-on account (per-user isolation).
	user *UserSession
}

// NewContainer creates the Job Object.
func NewContainer(opts ptypes.ContainerOptions) (*Container, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if opts.MemLimitBytes > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(opts.MemLimitBytes)
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	if opts.CPULimit > 0 {
		if err := setCPULimit(job, opts.CPULimit); err != nil {
			windows.CloseHandle(job)
			return nil, err
		}
	}
	c := &Container{job: job}
	if opts.RunAs != nil {
		s, err := LogonUser(opts.RunAs, false)
		if err != nil {
			windows.CloseHandle(job)
			return nil, err
		}
		c.user = s
	}
	return c, nil
}

// cpuRateControl is JOBOBJECT_CPU_RATE_CONTROL_INFORMATION (CpuRate form).
type cpuRateControl struct {
	ControlFlags uint32
	CpuRate      uint32
}

// setCPULimit hard-caps the job at cores worth of CPU (CpuRate is in 1/100 %
// of the whole machine).
func setCPULimit(job windows.Handle, cores float64) error {
	const enable, hardCap = 0x1, 0x4
	rate := uint32(cores / float64(runtime.NumCPU()) * 10000)
	if rate < 1 {
		rate = 1
	}
	if rate >= 10000 {
		return nil // at least the whole machine: no cap
	}
	info := cpuRateControl{ControlFlags: enable | hardCap, CpuRate: rate}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectCpuRateControlInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("job CPU limit: %w", err)
	}
	return nil
}

// UserEnv is the environment of the container's account (nil without one).
func (c *Container) UserEnv() []string {
	if c.user == nil {
		return nil
	}
	return append([]string(nil), c.user.Env...)
}

// UserHome is the profile directory of the container's account.
func (c *Container) UserHome() string {
	if c.user == nil {
		return ""
	}
	return c.user.Home
}

const (
	createSuspended       = 0x00000004
	createNoWindow        = 0x08000000
	createNewProcessGroup = 0x00000200
)

// Start starts cmd inside the job. The process is created suspended, assigned
// to the job and only then resumed, so not even its first grandchild can
// escape the job. Output goes to whatever cmd.Stdout/Stderr are, as raw
// bytes. The child inherits the caller's console (see SetupConsole: hidden,
// UTF-8) so console programs pick UTF-8; without one, CREATE_NO_WINDOW keeps
// console programs from popping up their own window. The new process group
// keeps Ctrl+C in an interactive longarmd from reaching jobs.
func (c *Container) Start(cmd *exec.Cmd) error {
	if c.user != nil {
		return c.startAs(cmd)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createSuspended | createNewProcessGroup
	if !HasConsole() {
		cmd.SysProcAttr.CreationFlags |= createNoWindow
	}
	cmd.SysProcAttr.HideWindow = true
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := uint32(cmd.Process.Pid)
	if err := c.assign(pid); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return err
	}
	if err := resumeProcess(pid); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return fmt.Errorf("resume: %w", err)
	}
	return nil
}

func (c *Container) assign(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer windows.CloseHandle(h)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job == 0 {
		return errors.New("container closed")
	}
	if err := windows.AssignProcessToJobObject(c.job, h); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// resumeProcess resumes every thread of a process created with
// CREATE_SUSPENDED (os/exec does not expose the primary thread handle).
func resumeProcess(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if err != nil {
			return err
		}
		_, rerr := windows.ResumeThread(th)
		windows.CloseHandle(th)
		if rerr != nil {
			return rerr
		}
		resumed++
	}
	if resumed == 0 {
		return fmt.Errorf("no threads found for pid %d", pid)
	}
	return nil
}

// Kill terminates every process in the job.
func (c *Container) Kill() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(c.job, KilledExitCode)
}

// PIDs lists the processes currently in the job.
func (c *Container) PIDs() ([]int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job == 0 {
		return nil, nil
	}
	// JOBOBJECT_BASIC_PROCESS_ID_LIST: two DWORDs then ULONG_PTR ids.
	const max = 4096
	buf := make([]uintptr, 2+max)
	if err := windows.QueryInformationJobObject(c.job, windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&buf[0])), uint32(len(buf))*uint32(unsafe.Sizeof(buf[0])), nil); err != nil {
		return nil, err
	}
	hdr := (*[2]uint32)(unsafe.Pointer(&buf[0]))
	n := int(hdr[1])
	first := 8 / int(unsafe.Sizeof(uintptr(0))) // ids follow the two DWORDs
	out := make([]int, 0, n)
	for i := 0; i < n && first+i < len(buf); i++ {
		out = append(out, int(buf[first+i]))
	}
	return out, nil
}

// Close releases the job handle. Because of KILL_ON_JOB_CLOSE this also kills
// whatever is still running in the job.
func (c *Container) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.user != nil {
		defer c.user.Close()
	}
	if c.job == 0 {
		return nil
	}
	err := windows.CloseHandle(c.job)
	c.job = 0
	return err
}
