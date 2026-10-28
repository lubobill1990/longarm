package tray

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

// hiddenCommand runs a console program without flashing a console window.
func hiddenCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd
}

// OpenURL opens an http(s) URL in the default browser.
func OpenURL(u string) error {
	cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", u)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// OpenTerminal runs argv in a new terminal window: Windows Terminal (wt)
// when installed, else a console window.
func OpenTerminal(argv []string) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}
	if wt, err := exec.LookPath("wt.exe"); err == nil {
		return exec.Command(wt, append([]string{"-w", "new"}, argv...)...).Start() // a new Windows Terminal window
	}
	// a new console window of its own
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000010} // CREATE_NEW_CONSOLE
	return cmd.Start()
}

// RunElevated starts exe elevated (the UAC prompt; "runas"), without
// waiting for it.
func RunElevated(exe string, args ...string) error {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = syscall.EscapeArg(a)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	params, err := windows.UTF16PtrFromString(strings.Join(q, " "))
	if err != nil {
		return err
	}
	const swHide = 0
	return windows.ShellExecute(0, verb, file, params, nil, swHide)
}

// ErrElevationCanceled: the user declined the administrator prompt.
var ErrElevationCanceled = errors.New("canceled at the administrator prompt")

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         windows.Handle
	verb         *uint16
	file         *uint16
	parameters   *uint16
	directory    *uint16
	show         int32
	instApp      windows.Handle
	idList       uintptr
	class        *uint16
	keyClass     windows.Handle
	hotKey       uint32
	iconOrMonitr windows.Handle
	process      windows.Handle
}

var procShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

// RunElevatedWait runs exe elevated (the UAC prompt) and waits for it:
// ErrElevationCanceled when the prompt was declined, an error naming the
// exit code when it failed.
func RunElevatedWait(exe string, args ...string) error {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = syscall.EscapeArg(a)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	params, err := windows.UTF16PtrFromString(strings.Join(q, " "))
	if err != nil {
		return err
	}
	const seeMaskNoCloseProcess, seeMaskNoAsync, swHide = 0x40, 0x100, 0
	info := shellExecuteInfo{fMask: seeMaskNoCloseProcess | seeMaskNoAsync, verb: verb, file: file, parameters: params, show: swHide}
	info.cbSize = uint32(unsafe.Sizeof(info))
	if r, _, e := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		if errors.Is(e, windows.ERROR_CANCELLED) {
			return ErrElevationCanceled
		}
		return e
	}
	if info.process == 0 {
		return errors.New("the elevated process did not start")
	}
	defer windows.CloseHandle(info.process)
	if _, err := windows.WaitForSingleObject(info.process, windows.INFINITE); err != nil {
		return err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.process, &code); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit code %d", code)
	}
	return nil
}
