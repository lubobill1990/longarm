//go:build windows

package screen

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Privacy mode: a black window over every monitor of this machine that the
// screen capture does not see (WDA_EXCLUDEFROMCAPTURE, Windows 10 2004+):
// somebody at the machine sees black, the viewer sees the desktop under
// it. It lets the mouse through (layered + transparent), so the viewer's
// clicks reach the programs below, and it stays on top.

var (
	procSetWindowDisplayAffinity   = modUser32.NewProc("SetWindowDisplayAffinity")
	procSetLayeredWindowAttributes = modUser32.NewProc("SetLayeredWindowAttributes")
	procLockWorkStation            = modUser32.NewProc("LockWorkStation")
	modWtsapi32                    = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSDisconnectSession       = modWtsapi32.NewProc("WTSDisconnectSession")
)

const (
	wdaExcludeFromCapture = 0x11
	wsExLayered           = 0x00080000
	wsExTransparent       = 0x00000020
	wsExTopmost           = 0x00000008
	wmDisplayChange       = 0x007E
	lwaAlpha              = 0x2
	hwndTopmost           = ^uintptr(0) // -1
)

type winCurtain struct {
	hwnd   uintptr
	thread uint32
	text   string
}

var theCurtain *winCurtain

func newCurtain(text string) (Curtain, error) {
	if theCurtain != nil {
		return nil, errors.New("screen: privacy mode is on already")
	}
	c := &winCurtain{text: text}
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		FollowInputDesktop() // the desktop people see (the agent runs as SYSTEM)
		cls, _ := windows.UTF16PtrFromString("LongarmPrivacyCurtain")
		wc := wndClassEx{WndProc: windows.NewCallback(curtainProc), ClassName: cls}
		wc.Size = uint32(unsafe.Sizeof(wc))
		procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		x, y, w, h := metric(smXVirtualScreen), metric(smYVirtualScreen), metric(smCXVirtualScreen), metric(smCYVirtualScreen)
		hwnd, _, err := procCreateWindowExW.Call(wsExTopmost|wsExToolWin|wsExLayered|wsExTransparent|wsExNoActive,
			uintptr(unsafe.Pointer(cls)), 0, wsPopup, uintptr(x), uintptr(y), uintptr(w), uintptr(h), 0, 0, 0, 0)
		if hwnd == 0 {
			ready <- fmt.Errorf("privacy window: %w", err)
			return
		}
		// Not in any capture: without this the viewer would see black too.
		if r, _, err := procSetWindowDisplayAffinity.Call(hwnd, wdaExcludeFromCapture); r == 0 {
			procDestroyWindow.Call(hwnd)
			ready <- fmt.Errorf("privacy mode needs Windows 10 2004 or newer (SetWindowDisplayAffinity: %w)", err)
			return
		}
		procSetLayeredWindowAttributes.Call(hwnd, 0, 255, lwaAlpha)
		c.hwnd, c.thread = hwnd, windows.GetCurrentThreadId()
		theCurtain = c
		procShowWindow.Call(hwnd, swShowNA)
		procSetTimer.Call(hwnd, 1, 300, 0) // stay above other topmost windows
		ready <- nil
		var m msg
		for {
			if r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) <= 0 {
				break
			}
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
		procDestroyWindow.Call(hwnd)
	}()
	if err := <-ready; err != nil {
		return nil, err
	}
	return c, nil
}

func (c *winCurtain) Close() {
	procPostThreadMessageW.Call(uintptr(c.thread), wmQuit, 0, 0)
	theCurtain = nil
}

func curtainProc(hwnd, message, wParam, lParam uintptr) uintptr {
	c := theCurtain
	if c != nil && hwnd == c.hwnd {
		switch message {
		case wmTimer:
			const swpNoMove, swpNoSize, swpNoActivate = 0x2, 0x1, 0x10
			procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
			return 0
		case wmDisplayChange:
			x, y, w, h := metric(smXVirtualScreen), metric(smYVirtualScreen), metric(smCXVirtualScreen), metric(smCYVirtualScreen)
			procSetWindowPos.Call(hwnd, hwndTopmost, uintptr(x), uintptr(y), uintptr(w), uintptr(h), 0x10)
			return 0
		case wmEraseBkgnd:
			return 1
		case wmPaint:
			var ps paintStruct
			dc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
			var r rect
			procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
			br, _, _ := procCreateSolidBrush.Call(0)
			procFillRect.Call(dc, uintptr(unsafe.Pointer(&r)), br)
			procDeleteObject.Call(br)
			procSetBkMode.Call(dc, 1)
			procSetTextColor.Call(dc, 0x808080)
			u, _ := windows.UTF16FromString(c.text)
			const dtCenter, dtVCenter, dtSingle = 0x1, 0x4, 0x20
			procDrawTextW.Call(dc, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&r)), dtCenter|dtVCenter|dtSingle)
			procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
			return 0
		}
	}
	r, _, _ := syscall.SyscallN(procDefWindowProcW.Addr(), hwnd, message, wParam, lParam)
	return r
}

// lockSession locks this session: LockWorkStation where the process may
// (the signed-in user's), else disconnecting the console session (SYSTEM's
// agent), which shows the lock screen as well.
func lockSession() error {
	if r, _, _ := procLockWorkStation.Call(); r != 0 {
		return nil
	}
	sid := ConsoleSession()
	if r, _, err := procWTSDisconnectSession.Call(0, uintptr(sid), 0); r == 0 {
		return fmt.Errorf("lock: %w", err)
	}
	return nil
}
