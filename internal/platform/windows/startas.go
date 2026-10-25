//go:build windows

package windows

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// emptyDesktop is lpDesktop = "": the new process connects to the window
// station of its own logon session (Service-0x0-<luid>$, created on
// demand), not to longarmd's, which the account may not use - otherwise
// user32 fails to initialize (STATUS_DLL_INIT_FAILED) in the child. It also
// keeps each user's windows, clipboard and hooks apart.
var emptyDesktop = []uint16{0}

// startAs starts cmd as the container's account: CreateProcessAsUser with
// the S4U token, suspended, assigned to the Job Object, then resumed - like
// Start. Only the standard handles are inherited (an explicit handle list),
// and they must be *os.File or nil (NUL). The child gets a console of its
// own (CREATE_NO_WINDOW): consoles of LocalSystem are not shared with user
// processes. cmd.Process is set, so cmd.Wait works as usual.
func (c *Container) startAs(cmd *exec.Cmd) error {
	if cmd.Process != nil {
		return errors.New("exec: already started")
	}
	env := cmd.Env
	if len(env) == 0 { // never LocalSystem's environment
		env = c.user.Env
	}
	app := cmd.Path
	if p, err := lookPath(cmd.Args[0], env); err == nil {
		app = p
	} else if cmd.Err != nil {
		return cmd.Err
	}
	var line string
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.CmdLine != "" {
		line = cmd.SysProcAttr.CmdLine
	} else {
		line = windows.ComposeCommandLine(cmd.Args)
	}
	appPtr, err := windows.UTF16PtrFromString(app)
	if err != nil {
		return err
	}
	cmdline, err := windows.UTF16PtrFromString(line)
	if err != nil {
		return err
	}
	var dirPtr *uint16
	if cmd.Dir != "" {
		if dirPtr, err = windows.UTF16PtrFromString(cmd.Dir); err != nil {
			return err
		}
	}

	// Standard handles: inheritable duplicates, closed after the start.
	self := windows.CurrentProcess()
	var std [3]windows.Handle
	var closers []windows.Handle
	defer func() {
		for _, h := range closers {
			windows.CloseHandle(h)
		}
	}()
	var nul *os.File
	for i, s := range []any{cmd.Stdin, cmd.Stdout, cmd.Stderr} {
		var f *os.File
		switch v := s.(type) {
		case nil:
			if nul == nil {
				mode := os.O_RDWR
				if nul, err = os.OpenFile(os.DevNull, mode, 0); err != nil {
					return err
				}
				defer nul.Close()
			}
			f = nul
		case *os.File:
			f = v
		default:
			return fmt.Errorf("start as %s: standard handle %d must be a file or pipe, not %T", c.user.name, i, s)
		}
		var dup windows.Handle
		if err := windows.DuplicateHandle(self, windows.Handle(f.Fd()), self, &dup, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return fmt.Errorf("DuplicateHandle: %w", err)
		}
		closers = append(closers, dup)
		std[i] = dup
	}
	handles := uniqueHandles(std[:])
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return err
	}
	defer attrs.Delete()
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		return fmt.Errorf("UpdateProcThreadAttribute: %w", err)
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW
	si.ShowWindow = windows.SW_HIDE
	si.StdInput, si.StdOutput, si.StdErr = std[0], std[1], std[2]
	si.Desktop = &emptyDesktop[0]

	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | createSuspended |
		createNewProcessGroup | createNoWindow)
	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(c.user.Token, appPtr, cmdline, nil, nil, true, flags, envBlock(env), dirPtr,
		&si.StartupInfo, &pi); err != nil {
		return &os.PathError{Op: "CreateProcessAsUser", Path: app, Err: err}
	}
	defer windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)
	if err := c.assign(pi.ProcessId); err != nil {
		windows.TerminateProcess(pi.Process, KilledExitCode)
		return err
	}
	// While pi.Process is open the pid cannot be reused, so FindProcess
	// gets this very process.
	p, err := os.FindProcess(int(pi.ProcessId))
	if err != nil {
		windows.TerminateProcess(pi.Process, KilledExitCode)
		return err
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		windows.TerminateProcess(pi.Process, KilledExitCode)
		p.Release()
		return fmt.Errorf("resume: %w", err)
	}
	cmd.Process = p
	return nil
}

func uniqueHandles(hs []windows.Handle) []windows.Handle {
	var out []windows.Handle
	for _, h := range hs {
		dup := false
		for _, o := range out {
			dup = dup || o == h
		}
		if !dup {
			out = append(out, h)
		}
	}
	return out
}
