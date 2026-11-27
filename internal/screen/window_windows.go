//go:build windows

package screen

import (
	"errors"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Window is the viewer's native window: a Win32 window whose client area
// a Presenter draws into. While it is in front, a low-level keyboard hook
// takes every key - Win, Alt+Tab, Ctrl+Esc included - and hands it to
// OnKey instead of the local system; Ctrl+Alt+Enter toggles full screen
// and Ctrl+Alt+End asks for Ctrl+Alt+Del on the other side (what Remote
// Desktop does). One Window per process; run it on one locked thread.

var (
	procRegisterClassExW      = modUser32.NewProc("RegisterClassExW")
	procCreateWindowExW       = modUser32.NewProc("CreateWindowExW")
	procDefWindowProcW        = modUser32.NewProc("DefWindowProcW")
	procPeekMessageW          = modUser32.NewProc("PeekMessageW")
	procTranslateMessage      = modUser32.NewProc("TranslateMessage")
	procDispatchMessageW      = modUser32.NewProc("DispatchMessageW")
	procPostQuitMessage       = modUser32.NewProc("PostQuitMessage")
	procShowWindow            = modUser32.NewProc("ShowWindow")
	procGetClientRect         = modUser32.NewProc("GetClientRect")
	procGetWindowRect         = modUser32.NewProc("GetWindowRect")
	procSetWindowLongPtrW     = modUser32.NewProc("SetWindowLongPtrW")
	procSetWindowPos          = modUser32.NewProc("SetWindowPos")
	procMonitorFromWindow     = modUser32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW       = modUser32.NewProc("GetMonitorInfoW")
	procSetWindowsHookExW     = modUser32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx   = modUser32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx        = modUser32.NewProc("CallNextHookEx")
	procGetForegroundWindow   = modUser32.NewProc("GetForegroundWindow")
	procMsgWaitForMultipleObj = modUser32.NewProc("MsgWaitForMultipleObjectsEx")
	procLoadCursorW           = modUser32.NewProc("LoadCursorW")
	procSetWindowTextW        = modUser32.NewProc("SetWindowTextW")
	procSetCapture            = modUser32.NewProc("SetCapture")
	procReleaseCapture        = modUser32.NewProc("ReleaseCapture")
	procDestroyWindow         = modUser32.NewProc("DestroyWindow")
	procSetCursor             = modUser32.NewProc("SetCursor")
	procGetSystemMenu         = modUser32.NewProc("GetSystemMenu")
	procAppendMenuW           = modUser32.NewProc("AppendMenuW")
)

const (
	wmDestroy    = 0x0002
	wmSize       = 0x0005
	wmKillFocus  = 0x0008
	wmClose      = 0x0010
	wmEraseBkgnd = 0x0014
	wmSetCursor  = 0x0020
	wmQuit       = 0x0012
	wmKeyDown    = 0x0100
	wmKeyUp      = 0x0101
	wmSysKeyDown = 0x0104
	wmSysKeyUp   = 0x0105
	wmSysChar    = 0x0106
	wmSysCommand = 0x0112
	scMaximize   = 0xF030
	// menuFullscreen: our item in the window menu (system command ids are
	// below 0xF000 and multiples of 16)
	menuFullscreen = 0x0010
	wmMouseMove    = 0x0200
	wmLButtonDown  = 0x0201
	wmLButtonUp    = 0x0202
	wmRButtonDown  = 0x0204
	wmRButtonUp    = 0x0205
	wmMButtonDown  = 0x0207
	wmMButtonUp    = 0x0208
	wmMouseWheel   = 0x020A
	wmMouseHWheel  = 0x020E
	wsOverlappedWn = 0x00CF0000
	wsPopup        = 0x80000000
	wsVisible      = 0x10000000
	cwUseDefault   = 0x80000000
	gwlStyle       = ^uintptr(15) // -16
	swpFrameChange = 0x0020
	swpNoOwnerZ    = 0x0200
	swShow         = 5
	pmRemove       = 1
	qsAllInput     = 0x04FF
	mwmoInputAvail = 0x0004
	whKeyboardLL   = 13
	llkhfExtended  = 0x01
	llkhfInjected  = 0x10
	llkhfUp        = 0x80
	idcArrow       = 32512

	scanCtrl, scanAlt, scanEnter, scanEnd = 0x1D, 0x38, 0x1C, 0x4F
)

type wndClassEx struct {
	Size, Style                   uint32
	WndProc                       uintptr
	ClsExtra, WndExtra            int32
	Instance, Icon, Cursor, Brush uintptr
	MenuName, ClassName           *uint16
	IconSm                        uintptr
}

type msg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	X, Y    int32
	_       uint32
}

type kbdLL struct {
	VK, Scan, Flags, Time uint32
	Extra                 uintptr
}

type monitorInfo struct {
	Size          uint32
	Monitor, Work rect
	Flags         uint32
}

// Mouse event kinds for Window.OnMouse.
const (
	MouseMoved = iota
	MouseDown
	MouseUp
	MouseWheeled
)

// Window is the viewer window (see the comment above).
type Window struct {
	HWND windows.HWND

	// Callbacks, on the window's thread. x and y are client coordinates.
	OnMouse  func(kind, x, y, button, dx, dy int)
	OnKey    func(scan uint16, down bool) // set-1 scan code, 0xE0xx extended
	OnResize func()
	OnSAS    func() // Ctrl+Alt+End
	// OnCursor: the cursor to show over the client area (0: the arrow).
	OnCursor func() uintptr

	hook       uintptr
	fullscreen bool
	saved      rect
	down       map[uint16]bool // keys held on the other side
	ctrl, alt  bool
}

var theWindow *Window

// NewWindow opens a window with a w×h client area (0: default size);
// takeKeys installs the keyboard hook (a view-only window leaves keys alone).
func NewWindow(title string, w, h int, takeKeys bool) (*Window, error) {
	if theWindow != nil {
		return nil, errors.New("screen: one viewer window per process")
	}
	runtime.LockOSThread()
	win := &Window{down: map[uint16]bool{}}
	theWindow = win
	cls, _ := windows.UTF16PtrFromString("LongarmDesktopViewer")
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	wc := wndClassEx{Style: 0x3 /* CS_HREDRAW|CS_VREDRAW */, WndProc: windows.NewCallback(wndProc), Cursor: cursor, ClassName: cls}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 && err != windows.ERROR_CLASS_ALREADY_EXISTS {
		return nil, err
	}
	t, _ := windows.UTF16PtrFromString(title)
	ww, wh := uintptr(cwUseDefault), uintptr(cwUseDefault)
	if w > 0 && h > 0 {
		ww, wh = uintptr(w), uintptr(h)
	}
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(t)), wsOverlappedWn,
		cwUseDefault, cwUseDefault, ww, wh, 0, 0, 0, 0)
	if hwnd == 0 {
		return nil, err
	}
	win.HWND = windows.HWND(hwnd)
	// The window menu (Alt+Space, the title bar's icon) offers full screen too.
	if m, _, _ := procGetSystemMenu.Call(hwnd, 0); m != 0 {
		label, _ := windows.UTF16PtrFromString("Full screen	Ctrl+Alt+Enter")
		procAppendMenuW.Call(m, 0x0800 /* MF_SEPARATOR */, 0, 0)
		procAppendMenuW.Call(m, 0, menuFullscreen, uintptr(unsafe.Pointer(label)))
	}
	// Twice: the first ShowWindow of a process takes its start-up show state
	// (a launcher's SW_HIDE, say), the second does what it is asked.
	procShowWindow.Call(hwnd, swShow)
	procShowWindow.Call(hwnd, swShow)
	if takeKeys {
		win.hook, _, _ = procSetWindowsHookExW.Call(whKeyboardLL, windows.NewCallback(keyboardHook), 0, 0)
	}
	return win, nil
}

// SetTitle sets the window's title.
func (w *Window) SetTitle(s string) {
	t, _ := windows.UTF16PtrFromString(s)
	procSetWindowTextW.Call(uintptr(w.HWND), uintptr(unsafe.Pointer(t)))
}

// Run handles the window's messages until it is closed, calling wake
// whenever ev is signaled (frames arrived) and after every batch of messages.
func (w *Window) Run(ev windows.Handle, wake func()) {
	defer func() {
		if w.hook != 0 {
			procUnhookWindowsHookEx.Call(w.hook)
		}
	}()
	for {
		procMsgWaitForMultipleObj.Call(1, uintptr(unsafe.Pointer(&ev)), windows.INFINITE, qsAllInput, mwmoInputAvail)
		var m msg
		for {
			r, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, pmRemove)
			if r == 0 {
				break
			}
			if m.Message == wmQuit {
				return
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
		wake()
	}
}

// Close closes the window (Run returns).
func (w *Window) Close() { procDestroyWindow.Call(uintptr(w.HWND)) }

// ToggleFullscreen switches between a borderless window over the whole
// monitor and the normal window.
func (w *Window) ToggleFullscreen() {
	h := uintptr(w.HWND)
	if !w.fullscreen {
		procGetWindowRect.Call(h, uintptr(unsafe.Pointer(&w.saved)))
		mon, _, _ := procMonitorFromWindow.Call(h, 2)
		mi := monitorInfo{}
		mi.Size = uint32(unsafe.Sizeof(mi))
		procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi)))
		procSetWindowLongPtrW.Call(h, gwlStyle, wsPopup|wsVisible)
		r := mi.Monitor
		procSetWindowPos.Call(h, 0, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), swpFrameChange|swpNoOwnerZ)
	} else {
		procSetWindowLongPtrW.Call(h, gwlStyle, wsOverlappedWn|wsVisible)
		r := w.saved
		procSetWindowPos.Call(h, 0, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), swpFrameChange|swpNoOwnerZ)
	}
	w.fullscreen = !w.fullscreen
}

func (w *Window) key(scan uint16, down bool) {
	switch scan & 0xFF {
	case scanCtrl:
		w.ctrl = down
	case scanAlt:
		w.alt = down
	}
	if down && w.ctrl && w.alt {
		switch scan {
		case scanEnter:
			w.ToggleFullscreen()
			return
		case 0xE000 | scanEnd:
			if w.OnSAS != nil {
				w.OnSAS()
			}
			return
		}
	}
	if down {
		w.down[scan] = true
	} else if !w.down[scan] {
		return // never went down there (e.g. the release of a toggle combo)
	} else {
		delete(w.down, scan)
	}
	if w.OnKey != nil {
		w.OnKey(scan, down)
	}
}

// releaseKeys lets go of every key held on the other side (focus moved
// away: its key-ups would never come).
func (w *Window) releaseKeys() {
	for s := range w.down {
		if w.OnKey != nil {
			w.OnKey(s, false)
		}
	}
	w.down = map[uint16]bool{}
	w.ctrl, w.alt = false, false
}

func keyboardHook(code int, wParam, lParam uintptr) uintptr {
	w := theWindow
	if code == 0 && w != nil {
		fg, _, _ := procGetForegroundWindow.Call()
		k := *(**kbdLL)(unsafe.Pointer(&lParam))
		// Injected keys are someone's SendInput - ours, when the viewer
		// shows its own machine: leave them alone.
		if fg == uintptr(w.HWND) && k.Flags&llkhfInjected == 0 && k.Scan != 0 {
			scan := uint16(k.Scan & 0xFF)
			if k.Flags&llkhfExtended != 0 {
				scan |= 0xE000
			}
			w.key(scan, k.Flags&llkhfUp == 0)
			return 1 // swallowed: the local system does not see it
		}
	}
	r, _, _ := procCallNextHookEx.Call(0, uintptr(code), wParam, lParam)
	return r
}

func lparamXY(l uintptr) (int, int) { return int(int16(l)), int(int16(l >> 16)) }

func wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	w := theWindow
	switch message {
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	case wmSetCursor:
		if lParam&0xFFFF == 1 /* HTCLIENT */ && w.OnCursor != nil {
			if c := w.OnCursor(); c != 0 {
				procSetCursor.Call(c)
				return 1
			}
		}
	case wmEraseBkgnd:
		return 1
	case wmSize:
		if w.OnResize != nil {
			w.OnResize()
		}
		return 0
	case wmKillFocus:
		w.releaseKeys()
	case wmMouseMove, wmLButtonDown, wmLButtonUp, wmRButtonDown, wmRButtonUp, wmMButtonDown, wmMButtonUp:
		x, y := lparamXY(lParam)
		kind, button := MouseMoved, 0
		switch message {
		case wmLButtonDown, wmRButtonDown, wmMButtonDown:
			kind = MouseDown
			procSetCapture.Call(hwnd)
		case wmLButtonUp, wmRButtonUp, wmMButtonUp:
			kind = MouseUp
			procReleaseCapture.Call()
		}
		switch message {
		case wmRButtonDown, wmRButtonUp:
			button = ButtonRight
		case wmMButtonDown, wmMButtonUp:
			button = ButtonMiddle
		case wmLButtonDown, wmLButtonUp:
			button = ButtonLeft
		}
		if w.OnMouse != nil {
			w.OnMouse(kind, x, y, button, 0, 0)
		}
		return 0
	case wmMouseWheel, wmMouseHWheel:
		d := int(int16(wParam >> 16))
		dx, dy := 0, d
		if message == wmMouseHWheel {
			dx, dy = d, 0
		}
		if w.OnMouse != nil {
			w.OnMouse(MouseWheeled, 0, 0, 0, dx, dy)
		}
		return 0
	case wmKeyDown, wmKeyUp, wmSysKeyDown, wmSysKeyUp:
		// Only without the hook (it takes keys first while we are in front).
		scan := uint16(lParam>>16) & 0xFF
		if lParam>>24&1 != 0 {
			scan |= 0xE000
		}
		w.key(scan, message == wmKeyDown || message == wmSysKeyDown)
		return 0
	case wmSysCommand:
		// Maximizing (the button, a double click on the title bar) goes full
		// screen, as Remote Desktop's window does; so does our menu item.
		if c := wParam & 0xFFF0; c == scMaximize && !w.fullscreen || wParam == menuFullscreen {
			w.ToggleFullscreen()
			return 0
		}
	case wmSysChar:
		return 0 // no menu beep for Alt+key
	}
	r, _, _ := syscall.SyscallN(procDefWindowProcW.Addr(), hwnd, message, wParam, lParam)
	return r
}

func clientSize(hwnd windows.HWND) (int, int) {
	var r rect
	procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))
	return int(r.Right - r.Left), int(r.Bottom - r.Top)
}
