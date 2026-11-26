//go:build windows

package screen

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The SYSTEM desktop agent's half (docs/remote-desktop.md "Worker: where
// capture runs"): a LocalSystem service starts the capturer in the console
// session as SYSTEM, which can open the Winlogon desktop (UAC prompts, the
// lock and Ctrl+Alt+Del screens) and send Ctrl+Alt+Del.

var (
	modKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	modSAS      = windows.NewLazySystemDLL("sas.dll")

	procWTSGetActiveConsoleSessionId = modKernel32.NewProc("WTSGetActiveConsoleSessionId")
	procSendSAS                      = modSAS.NewProc("SendSAS")
)

// ConsoleSession is the session attached to the physical (or virtual)
// console; 0xFFFFFFFF while it is switching.
func ConsoleSession() uint32 {
	r, _, _ := procWTSGetActiveConsoleSessionId.Call()
	return uint32(r)
}

// StartInSession starts cmdline as this process's account (SYSTEM, when a
// LocalSystem service calls it) in session, on winsta0\default.
func StartInSession(session uint32, cmdline string) (windows.Handle, error) {
	var self windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ALL_ACCESS, &self); err != nil {
		return 0, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer self.Close()
	var tok windows.Token
	if err := windows.DuplicateTokenEx(self, windows.MAXIMUM_ALLOWED, nil, windows.SecurityIdentification, windows.TokenPrimary, &tok); err != nil {
		return 0, fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	defer tok.Close()
	// Needs SeTcbPrivilege: only SYSTEM has it.
	if err := windows.SetTokenInformation(tok, windows.TokenSessionId, (*byte)(unsafe.Pointer(&session)), 4); err != nil {
		return 0, fmt.Errorf("SetTokenInformation(TokenSessionId) (not SYSTEM?): %w", err)
	}
	desk, _ := windows.UTF16PtrFromString(`winsta0\default`)
	cl, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return 0, err
	}
	si := windows.StartupInfo{Desktop: desk}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(tok, nil, cl, nil, nil, false, windows.CREATE_NO_WINDOW|windows.CREATE_UNICODE_ENVIRONMENT, nil, nil, &si, &pi); err != nil {
		return 0, fmt.Errorf("CreateProcessAsUser: %w", err)
	}
	windows.CloseHandle(pi.Thread)
	return pi.Process, nil
}

var (
	inputDeskMu sync.Mutex
	inputDesk   = map[uint32]uintptr{} // thread id -> its desktop handle
)

// FollowInputDesktop moves the calling thread (lock it to the goroutine)
// to the desktop that gets input now, so SendInput reaches UAC prompts and
// the Winlogon screens (SYSTEM only; elsewhere it stays where it is).
func FollowInputDesktop() {
	h, _, _ := procOpenInputDesktop.Call(0, 0, genericAll)
	if h == 0 {
		return
	}
	tid := windows.GetCurrentThreadId()
	inputDeskMu.Lock()
	defer inputDeskMu.Unlock()
	if r, _, _ := procSetThreadDesktop.Call(h); r == 0 {
		procCloseDesktop.Call(h)
		return
	}
	if old := inputDesk[tid]; old != 0 {
		procCloseDesktop.Call(old)
	}
	inputDesk[tid] = h
}

// SendSAS sends Ctrl+Alt+Del (the secure attention sequence). Only a
// service (SYSTEM) may, and only where the SoftwareSASGeneration policy
// allows services (HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies
// \System, 1 or 3).
func SendSAS() error {
	if err := procSendSAS.Find(); err != nil {
		return err
	}
	procSendSAS.Call(0) // AsUser = FALSE: from a service; returns nothing
	return nil
}
