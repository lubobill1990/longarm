//go:build windows

package main

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The tray and Windows' shutdown protocol: a log-off, a shutdown, or
// Restart Manager (another installer that must replace a file the tray
// holds - WebView2's runtime, say) sends WM_QUERYENDSESSION and
// WM_ENDSESSION (lParam ENDSESSION_CLOSEAPP from Restart Manager) to the
// app's top-level windows. A hidden window of our own answers them: yes,
// the tray may go, and it quits at once (as from its menu). The tray is
// registered for restart (RegisterApplicationRestart), so Restart Manager
// starts it again after the update - not after a crash, a hang or a reboot
// (the Run key starts it at logon). The MSI itself does not use Restart
// Manager (MSIRESTARTMANAGERCONTROL=Disable): it asks the tray to exit
// through its stop event and starts it again (internal/msiupgrade).

const (
	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
	wmClose           = 0x0010
	wmDestroy         = 0x0002
	restartNoCrash    = 1
	restartNoHang     = 2
	restartNoReboot   = 8
)

var (
	user32                         = windows.NewLazySystemDLL("user32.dll")
	kernel32                       = windows.NewLazySystemDLL("kernel32.dll")
	procRegisterClassExW           = user32.NewProc("RegisterClassExW")
	procCreateWindowExW            = user32.NewProc("CreateWindowExW")
	procDefWindowProcW             = user32.NewProc("DefWindowProcW")
	procGetMessageW                = user32.NewProc("GetMessageW")
	procTranslateMessage           = user32.NewProc("TranslateMessage")
	procDispatchMessageW           = user32.NewProc("DispatchMessageW")
	procPostMessageW               = user32.NewProc("PostMessageW")
	procDestroyWindow              = user32.NewProc("DestroyWindow")
	procPostQuitMessage            = user32.NewProc("PostQuitMessage")
	procRegisterApplicationRestart = kernel32.NewProc("RegisterApplicationRestart")
)

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   windows.Handle
	icon       windows.Handle
	cursor     windows.Handle
	background windows.Handle
	menuName   *uint16
	className  *uint16
	iconSm     windows.Handle
}

type msg struct {
	hwnd    windows.HWND
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	private uint32
}

var (
	endSessionMu   sync.Mutex
	endSessionQuit func()
	wndProcPtr     = syscall.NewCallback(endSessionWndProc)
)

func endSessionWndProc(hwnd windows.HWND, m uint32, wParam, lParam uintptr) uintptr {
	switch m {
	case wmQueryEndSession:
		return 1 // the tray may go
	case wmEndSession:
		if wParam != 0 {
			quitTray()
		}
		return 0
	case wmClose:
		quitTray()
		procDestroyWindow.Call(uintptr(hwnd))
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(m), wParam, lParam)
	return r
}

func quitTray() {
	endSessionMu.Lock()
	q := endSessionQuit
	endSessionMu.Unlock()
	if q != nil {
		q()
	}
}

// watchEndSession calls quit when Windows ends the session or Restart
// Manager closes the tray; the returned func stops watching.
func watchEndSession(quit func()) func() {
	endSessionMu.Lock()
	endSessionQuit = quit
	endSessionMu.Unlock()
	if procRegisterApplicationRestart.Find() == nil {
		empty, _ := windows.UTF16PtrFromString("")
		procRegisterApplicationRestart.Call(uintptr(unsafe.Pointer(empty)), restartNoCrash|restartNoHang|restartNoReboot)
	}
	hwndc := make(chan windows.HWND, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		name, _ := windows.UTF16PtrFromString("LongarmTrayEndSession")
		var inst windows.Handle
		windows.GetModuleHandleEx(0, nil, &inst)
		wc := wndClassEx{wndProc: wndProcPtr, instance: inst, className: name}
		wc.size = uint32(unsafe.Sizeof(wc))
		procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		// a hidden top-level window (not message-only: those get no
		// session messages)
		h, _, _ := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)), 0,
			0, 0, 0, 0, 0, 0, uintptr(inst), 0)
		hwndc <- windows.HWND(h)
		if h == 0 {
			return
		}
		var m msg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 {
				return
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
	h := <-hwndc
	return func() {
		endSessionMu.Lock()
		endSessionQuit = nil
		endSessionMu.Unlock()
		if h != 0 {
			procPostMessageW.Call(uintptr(h), wmClose, 0, 0)
		}
	}
}
