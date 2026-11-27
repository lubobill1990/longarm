//go:build windows

package screen

import (
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The full-screen window's connection bar, as Remote Desktop's: a strip at
// the top of the screen with the worker's name, a pin, minimize, leave
// full screen and close. Pinned, it stays; unpinned, it hides a moment
// after the pointer leaves it and comes back when the pointer touches the
// top edge of the screen. It never takes the keyboard (WS_EX_NOACTIVATE):
// keys keep going to the worker.

var (
	procCreateFontW      = modGDI32.NewProc("CreateFontW")
	procSelectObject     = modGDI32.NewProc("SelectObject")
	procSetBkMode        = modGDI32.NewProc("SetBkMode")
	procSetTextColor     = modGDI32.NewProc("SetTextColor")
	procCreateSolidBrush = modGDI32.NewProc("CreateSolidBrush")
	procDrawTextW        = modUser32.NewProc("DrawTextW")
	procFillRect         = modUser32.NewProc("FillRect")
	procBeginPaint       = modUser32.NewProc("BeginPaint")
	procEndPaint         = modUser32.NewProc("EndPaint")
	procInvalidateRect   = modUser32.NewProc("InvalidateRect")
	procGetCursorPos     = modUser32.NewProc("GetCursorPos")
	procGetDpiForWindow  = modUser32.NewProc("GetDpiForWindow")
	procSetTimer         = modUser32.NewProc("SetTimer")
	procKillTimer        = modUser32.NewProc("KillTimer")
)

const (
	wmPaint        = 0x000F
	wmTimer        = 0x0113
	wmLButtonUpBar = 0x0202
	wmMouseMoveBar = 0x0200
	wsExToolWin    = 0x00000080
	wsExNoActive   = 0x08000000
	swHide         = 0
	swShowNA       = 8 // shown, not activated
	swMinimize     = 6

	barW, barH   = 520, 30 // at 96 DPI
	barBtnW      = 40
	barHideAfter = 1500 * time.Millisecond

	// Segoe MDL2 Assets glyphs
	glyphPin      = ""
	glyphPinned   = ""
	glyphMinimize = ""
	glyphRestore  = ""
	glyphClose    = ""
)

type paintStruct struct {
	HDC       uintptr
	Erase     int32
	Paint     rect
	Restore   int32
	IncUpdate int32
	Reserved  [32]byte
}

type point struct{ X, Y int32 }

// bar is the connection bar of the viewer window (one per process).
type bar struct {
	owner       *Window
	hwnd        uintptr
	pinned      bool
	shown       bool
	lastInside  time.Time
	hover       int // button under the pointer (-1 none)
	font, icons uintptr
	fontDPI     uint32
}

var theBar *bar

// Bar buttons, left to right as hit-tested.
const (
	btnPin = iota
	btnMinimize
	btnRestore
	btnClose
)

func (b *bar) scale() float64 {
	dpi, _, _ := procGetDpiForWindow.Call(b.hwnd)
	if dpi == 0 {
		dpi = 96
	}
	return float64(dpi) / 96
}

// rects: the bar's size and its buttons' places (client coordinates).
func (b *bar) rects() (w, h int32, btns [4]rect) {
	s := b.scale()
	w, h = int32(barW*s), int32(barH*s)
	bw := int32(barBtnW * s)
	btns[btnPin] = rect{0, 0, bw, h}
	btns[btnMinimize] = rect{w - 3*bw, 0, w - 2*bw, h}
	btns[btnRestore] = rect{w - 2*bw, 0, w - bw, h}
	btns[btnClose] = rect{w - bw, 0, w, h}
	return
}

func newBar(owner *Window) *bar {
	if theBar != nil {
		return theBar
	}
	b := &bar{owner: owner, pinned: true, hover: -1}
	cls, _ := windows.UTF16PtrFromString("LongarmDesktopBar")
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	wc := wndClassEx{WndProc: windows.NewCallback(barProc), Cursor: cursor, ClassName: cls}
	wc.Size = uint32(unsafe.Sizeof(wc))
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	// Owned by the viewer: always above it, gone with it.
	b.hwnd, _, _ = procCreateWindowExW.Call(wsExToolWin|wsExNoActive, uintptr(unsafe.Pointer(cls)), 0, wsPopup,
		0, 0, barW, barH, uintptr(owner.HWND), 0, 0, 0)
	theBar = b
	return b
}

// show puts the bar at the top centre of the viewer's monitor.
func (b *bar) show() {
	if b.hwnd == 0 {
		return
	}
	var r rect
	procGetWindowRect.Call(uintptr(b.owner.HWND), uintptr(unsafe.Pointer(&r)))
	w, h, _ := b.rects()
	x := r.Left + (r.Right-r.Left-w)/2
	const swpNoActivate, swpShowWindow = 0x0010, 0x0040
	procSetWindowPos.Call(b.hwnd, 0, uintptr(x), uintptr(r.Top), uintptr(w), uintptr(h), swpNoActivate|swpShowWindow)
	b.lastInside = time.Now()
	if !b.shown {
		b.shown = true
		procSetTimer.Call(b.hwnd, 1, 200, 0)
	}
	procInvalidateRect.Call(b.hwnd, 0, 0)
}

func (b *bar) hide() {
	if b.hwnd == 0 || !b.shown {
		return
	}
	b.shown = false
	procKillTimer.Call(b.hwnd, 1)
	procShowWindow.Call(b.hwnd, swHide)
}

// tick: unpinned, the bar goes once the pointer has been away a moment.
func (b *bar) tick() {
	var p point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	var r rect
	procGetWindowRect.Call(b.hwnd, uintptr(unsafe.Pointer(&r)))
	if p.X >= r.Left && p.X < r.Right && p.Y >= r.Top && p.Y < r.Bottom+4 {
		b.lastInside = time.Now()
		return
	}
	if b.hover != -1 {
		b.hover = -1
		procInvalidateRect.Call(b.hwnd, 0, 0)
	}
	if !b.pinned && time.Since(b.lastInside) > barHideAfter {
		b.hide()
	}
}

func (b *bar) fonts() {
	dpi, _, _ := procGetDpiForWindow.Call(b.hwnd)
	if b.font != 0 && uint32(dpi) == b.fontDPI {
		return
	}
	if b.font != 0 {
		procDeleteObject.Call(b.font)
		procDeleteObject.Call(b.icons)
	}
	b.fontDPI = uint32(dpi)
	s := b.scale()
	face := func(name string, px float64) uintptr {
		n, _ := windows.UTF16PtrFromString(name)
		f, _, _ := procCreateFontW.Call(uintptr(int32(-px*s)), 0, 0, 0, 400, 0, 0, 0, 1 /* DEFAULT_CHARSET */, 0, 0, 5 /* CLEARTYPE_QUALITY */, 0, uintptr(unsafe.Pointer(n)))
		return f
	}
	b.font, b.icons = face("Segoe UI", 13), face("Segoe MDL2 Assets", 11)
}

func (b *bar) paint() {
	var ps paintStruct
	dc, _, _ := procBeginPaint.Call(b.hwnd, uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(b.hwnd, uintptr(unsafe.Pointer(&ps)))
	b.fonts()
	w, h, btns := b.rects()
	fill := func(r rect, rgb uint32) {
		br, _, _ := procCreateSolidBrush.Call(uintptr(rgb))
		procFillRect.Call(dc, uintptr(unsafe.Pointer(&r)), br)
		procDeleteObject.Call(br)
	}
	fill(rect{0, 0, w, h}, 0x2B2B2B)
	if b.hover >= 0 {
		c := uint32(0x404040)
		if b.hover == btnClose {
			c = 0x2311E8 // red (BGR)
		}
		fill(btns[b.hover], c)
	}
	procSetBkMode.Call(dc, 1) // TRANSPARENT
	procSetTextColor.Call(dc, 0xF0F0F0)
	text := func(s string, r rect, font uintptr) {
		u, _ := windows.UTF16FromString(s)
		procSelectObject.Call(dc, font)
		const dtCenter, dtVCenter, dtSingle, dtEllipsis = 0x1, 0x4, 0x20, 0x8000
		procDrawTextW.Call(dc, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&r)), dtCenter|dtVCenter|dtSingle|dtEllipsis)
	}
	pin := glyphPin
	if b.pinned {
		pin = glyphPinned
	}
	text(pin, btns[btnPin], b.icons)
	text(glyphMinimize, btns[btnMinimize], b.icons)
	text(glyphRestore, btns[btnRestore], b.icons)
	text(glyphClose, btns[btnClose], b.icons)
	text(b.owner.Name+"   ·   Ctrl+Alt+Enter", rect{btns[btnPin].Right, 0, btns[btnMinimize].Left, h}, b.font)
}

func (b *bar) hit(x, y int32) int {
	_, _, btns := b.rects()
	for i, r := range btns {
		if x >= r.Left && x < r.Right && y >= r.Top && y < r.Bottom {
			return i
		}
	}
	return -1
}

func (b *bar) click(btn int) {
	switch btn {
	case btnPin:
		b.pinned = !b.pinned
		b.lastInside = time.Now()
		procInvalidateRect.Call(b.hwnd, 0, 0)
	case btnMinimize:
		procShowWindow.Call(uintptr(b.owner.HWND), swMinimize)
	case btnRestore:
		b.owner.ToggleFullscreen()
	case btnClose:
		b.owner.Close()
	}
}

func barProc(hwnd, message, wParam, lParam uintptr) uintptr {
	b := theBar
	if b != nil && hwnd == b.hwnd {
		switch message {
		case wmPaint:
			b.paint()
			return 0
		case wmEraseBkgnd:
			return 1
		case wmTimer:
			b.tick()
			return 0
		case wmMouseMoveBar:
			x, y := lparamXY(lParam)
			if h := b.hit(int32(x), int32(y)); h != b.hover {
				b.hover = h
				procInvalidateRect.Call(b.hwnd, 0, 0)
			}
			b.lastInside = time.Now()
			return 0
		case wmLButtonUpBar:
			x, y := lparamXY(lParam)
			if h := b.hit(int32(x), int32(y)); h >= 0 {
				b.click(h)
			}
			return 0
		}
	}
	r, _, _ := syscall.SyscallN(procDefWindowProcW.Addr(), hwnd, message, wParam, lParam)
	return r
}
