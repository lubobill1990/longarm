//go:build windows

package windows

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetConsoleCP            = modkernel32.NewProc("GetConsoleCP")
	procSetConsoleCP            = modkernel32.NewProc("SetConsoleCP")
	procSetConsoleOutputCP      = modkernel32.NewProc("SetConsoleOutputCP")
	procAllocConsole            = modkernel32.NewProc("AllocConsole")
	procAllocConsoleWithOptions = modkernel32.NewProc("AllocConsoleWithOptions")
	procGetConsoleWindow        = modkernel32.NewProc("GetConsoleWindow")
	moduser32                   = windows.NewLazySystemDLL("user32.dll")
	procShowWindow              = moduser32.NewProc("ShowWindow")
)

const cpUTF8 = 65001

// HasConsole reports whether the process is attached to a console.
func HasConsole() bool {
	r, _, _ := procGetConsoleCP.Call()
	return r != 0
}

// SetupConsole gives a console-less process (service, detached shim) a
// hidden console whose input/output code pages are UTF-8. Job processes
// inherit it, so console-aware programs (.NET/pwsh, cmd, python) encode their
// redirected output as UTF-8 instead of the OEM/ANSI code page - the
// "explicit UTF-8" of DESIGN §1. A process that already has a console (longarmd
// run in a terminal) is left alone: we do not change the user's terminal.
func SetupConsole() error {
	if HasConsole() {
		return nil
	}
	if err := allocHiddenConsole(); err != nil {
		return err
	}
	procSetConsoleCP.Call(cpUTF8)
	procSetConsoleOutputCP.Call(cpUTF8)
	return nil
}

type allocConsoleOptions struct {
	mode          int32 // ALLOC_CONSOLE_MODE
	useShowWindow int32 // BOOL
	showWindow    uint16
}

func allocHiddenConsole() error {
	// Windows 11 24H2+: a console session that never has a window.
	if procAllocConsoleWithOptions.Find() == nil {
		const allocConsoleModeNoWindow = 2
		opts := allocConsoleOptions{mode: allocConsoleModeNoWindow}
		var result int32
		hr, _, _ := procAllocConsoleWithOptions.Call(uintptr(unsafe.Pointer(&opts)), uintptr(unsafe.Pointer(&result)))
		if hr == 0 {
			return nil
		}
	}
	if r, _, err := procAllocConsole.Call(); r == 0 {
		return err
	}
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 {
		const swHide = 0
		procShowWindow.Call(hwnd, swHide)
	}
	return nil
}

var procFreeConsole = modkernel32.NewProc("FreeConsole")

// FreeConsole detaches from the inherited console, if any.
func FreeConsole() {
	if HasConsole() {
		procFreeConsole.Call()
	}
}

// UTF8Console switches the attached console (e.g. a pseudo console) to UTF-8
// input/output code pages.
func UTF8Console() {
	procSetConsoleCP.Call(cpUTF8)
	procSetConsoleOutputCP.Call(cpUTF8)
}

// TerminalOutput prepares the console behind h (stdout) for VT output: it
// enables virtual terminal processing and UTF-8 output, returning a restore
// function.
func TerminalOutput(h windows.Handle) (func(), error) {
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return func() {}, err
	}
	oldCP, _, _ := procGetConsoleOutputCP.Call()
	nm := mode | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(h, nm); err != nil {
		return func() {}, err
	}
	procSetConsoleOutputCP.Call(cpUTF8)
	return func() {
		windows.SetConsoleMode(h, mode)
		if oldCP != 0 {
			procSetConsoleOutputCP.Call(oldCP)
		}
	}, nil
}

var procGetConsoleOutputCP = modkernel32.NewProc("GetConsoleOutputCP")
