//go:build windows

package screen

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The viewer's own title bar (Chrome). In a window it is the window's title
// bar - across the top, the video below it; drag it to move the window,
// double-click for full screen. In full screen it is Remote Desktop's
// connection bar: a strip at the top centre with a pin; unpinned it hides a
// moment after the pointer leaves and comes back at the top edge. Left: the
// connection's security; right: privacy mode, lock on leave, displays,
// sound, microphone, then minimize, full screen / window, close. Hovering a
// button says what it does. It never takes the keyboard (WS_EX_NOACTIVATE):
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
	procSendMessageW     = modUser32.NewProc("SendMessageW")
	procCreatePopupMenu  = modUser32.NewProc("CreatePopupMenu")
	procTrackPopupMenu   = modUser32.NewProc("TrackPopupMenu")
	procDestroyMenu      = modUser32.NewProc("DestroyMenu")
	procClientToScreen   = modUser32.NewProc("ClientToScreen")
	procMessageBoxW      = modUser32.NewProc("MessageBoxW")
)

const (
	wmPaint         = 0x000F
	wmTimer         = 0x0113
	wmLButtonDblClk = 0x0203
	wmNCLButtonDown = 0x00A1
	htCaption       = 2
	wsExToolWin     = 0x00000080
	wsExNoActive    = 0x08000000
	swHide          = 0
	swShowNA        = 8 // shown, not activated
	swMinimize      = 6
	csDblClks       = 0x0008

	barFullW     = 640 // full screen strip, at 96 DPI
	barH         = 32  // at 96 DPI
	barBtnW      = 44
	barHideAfter = 1500 * time.Millisecond

	accent = 0xD47800 // BGR of #0078D4
)

// Segoe MDL2 Assets glyphs.
const (
	glyphShield     = string(rune(0xEA18))
	glyphPrivacy    = string(rune(0xED1A))
	glyphLock       = string(rune(0xE72E))
	glyphDisplay    = string(rune(0xE7F4))
	glyphSession    = string(rune(0xE748)) // SwitchUser
	glyphSound      = string(rune(0xE767))
	glyphMic        = string(rune(0xE720))
	glyphPin        = string(rune(0xE718))
	glyphPinned     = string(rune(0xE840))
	glyphMinimize   = string(rune(0xE921))
	glyphFullscreen = string(rune(0xE740))
	glyphWindow     = string(rune(0xE73F))
	glyphClose      = string(rune(0xE8BB))
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

// barButton is one button: what it does, its glyph, whether it is on.
type barButton struct {
	id    string
	glyph string
	on    bool
	tip   string
	r     rect
}

type bar struct {
	owner       *Window
	hwnd        uintptr
	pinned      bool
	shown       bool
	lastInside  time.Time
	hover       string // the button under the pointer
	buttons     []barButton
	font, icons uintptr
	fontDPI     uint32
}

var theBar *bar

func (b *bar) scale() float64 {
	dpi, _, _ := procGetDpiForWindow.Call(b.hwnd)
	if dpi == 0 {
		dpi = 96
	}
	return float64(dpi) / 96
}

// height is the title bar's height in pixels (the video goes below it in a
// window).
func (b *bar) height() int32 { return int32(barH * b.scale()) }

func newBar(owner *Window) *bar {
	if theBar != nil {
		return theBar
	}
	b := &bar{owner: owner, pinned: true}
	cls, _ := windows.UTF16PtrFromString("LongarmDesktopBar")
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	wc := wndClassEx{Style: csDblClks, WndProc: windows.NewCallback(barProc), Cursor: cursor, ClassName: cls}
	wc.Size = uint32(unsafe.Sizeof(wc))
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	// Owned by the viewer: always above it, minimized and gone with it.
	b.hwnd, _, _ = procCreateWindowExW.Call(wsExToolWin|wsExNoActive, uintptr(unsafe.Pointer(cls)), 0, wsPopup,
		0, 0, barFullW, barH, uintptr(owner.HWND), 0, 0, 0)
	theBar = b
	return b
}

// place puts the bar where it belongs: across the top of the window's
// client area, or the top centre of the screen in full screen.
func (b *bar) place() {
	if b.hwnd == 0 {
		return
	}
	var r rect
	procGetClientRect.Call(uintptr(b.owner.HWND), uintptr(unsafe.Pointer(&r)))
	var tl point
	procClientToScreen.Call(uintptr(b.owner.HWND), uintptr(unsafe.Pointer(&tl)))
	w, h := r.Right-r.Left, b.height()
	x := tl.X
	if b.owner.fullscreen {
		fw := min(int32(barFullW*b.scale()), w)
		x, w = tl.X+(w-fw)/2, fw
	}
	const swpNoActivate, swpShowWindow = 0x0010, 0x0040
	procSetWindowPos.Call(b.hwnd, 0, uintptr(x), uintptr(tl.Y), uintptr(w), uintptr(h), swpNoActivate|swpShowWindow)
	b.shown = true
	b.layout()
	procInvalidateRect.Call(b.hwnd, 0, 0)
}

// show: full screen's "come back" (and the window's bar after a move).
func (b *bar) show() {
	b.lastInside = time.Now()
	b.place()
	procSetTimer.Call(b.hwnd, 1, 200, 0)
}

func (b *bar) hide() {
	if b.hwnd == 0 || !b.shown {
		return
	}
	b.shown = false
	procKillTimer.Call(b.hwnd, 1)
	procShowWindow.Call(b.hwnd, swHide)
}

// tick: in full screen, unpinned, the bar goes once the pointer has been
// away a moment.
func (b *bar) tick() {
	var p point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	var r rect
	procGetWindowRect.Call(b.hwnd, uintptr(unsafe.Pointer(&r)))
	inside := p.X >= r.Left && p.X < r.Right && p.Y >= r.Top && p.Y < r.Bottom+4
	if inside {
		b.lastInside = time.Now()
	} else if b.hover != "" {
		b.hover = ""
		procInvalidateRect.Call(b.hwnd, 0, 0)
	}
	if b.owner.fullscreen && !b.pinned && !inside && time.Since(b.lastInside) > barHideAfter {
		b.hide()
	}
}

// layout decides the buttons and their places, right to left.
func (b *bar) layout() {
	c := b.owner.Chrome
	var r rect
	procGetClientRect.Call(b.hwnd, uintptr(unsafe.Pointer(&r)))
	h, bw := r.Bottom, int32(barBtnW*b.scale())
	var btns []barButton
	add := func(id, glyph string, on bool, tip string) {
		btns = append(btns, barButton{id: id, glyph: glyph, on: on, tip: tip})
	}
	add("close", glyphClose, false, "Disconnect and close")
	if b.owner.fullscreen {
		add("window", glyphWindow, false, "Leave full screen (Ctrl+Alt+Enter)")
	} else {
		add("fullscreen", glyphFullscreen, false, "Full screen (Ctrl+Alt+Enter)")
	}
	add("minimize", glyphMinimize, false, "Minimize")
	if b.owner.fullscreen {
		pin, tip := glyphPin, "Pin the bar (it stays)"
		if b.pinned {
			pin, tip = glyphPinned, "Unpin the bar (it hides; the top edge brings it back)"
		}
		add("pin", pin, b.pinned, tip)
	}
	if c != nil {
		if c.Can["microphone"] {
			add("microphone", glyphMic, c.On["microphone"], onOff(c.On["microphone"], "Your microphone is the worker's"))
		}
		if c.Can["sound"] {
			add("sound", glyphSound, c.On["sound"], onOff(c.On["sound"], "The worker's sound plays here"))
		}
		if c.Can["sessions"] && len(c.Sessions) > 1 {
			add("session", glyphSession, false, "Windows sessions: show another (Remote Desktop sign-ins, disconnected ones)")
		}
		if len(c.Displays) > 1 {
			add("display", glyphDisplay, false, fmt.Sprintf("Display %d of %d: choose another", c.Display+1, len(c.Displays)))
		}
		if c.Can["lock_on_leave"] {
			add("lock_on_leave", glyphLock, c.On["lock_on_leave"], onOff(c.On["lock_on_leave"], "Lock the worker when you disconnect"))
		}
		if c.Can["privacy"] {
			add("privacy", glyphPrivacy, c.On["privacy"], onOff(c.On["privacy"], "Privacy mode: the worker's own screen shows black"))
		}
	}
	x := r.Right
	for i := range btns {
		btns[i].r = rect{x - bw, 0, x, h}
		x -= bw
	}
	b.buttons = btns
}

func onOff(on bool, s string) string {
	if on {
		return s + " (on)"
	}
	return s + " (off)"
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
	b.font, b.icons = face("Segoe UI", 12), face("Segoe MDL2 Assets", 11)
}

func (b *bar) paint() {
	var ps paintStruct
	dc, _, _ := procBeginPaint.Call(b.hwnd, uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(b.hwnd, uintptr(unsafe.Pointer(&ps)))
	b.fonts()
	if b.buttons == nil {
		b.layout()
	}
	var r rect
	procGetClientRect.Call(b.hwnd, uintptr(unsafe.Pointer(&r)))
	fill := func(r rect, rgb uint32) {
		br, _, _ := procCreateSolidBrush.Call(uintptr(rgb))
		procFillRect.Call(dc, uintptr(unsafe.Pointer(&r)), br)
		procDeleteObject.Call(br)
	}
	fill(r, 0x202020)
	procSetBkMode.Call(dc, 1) // TRANSPARENT
	text := func(s string, r rect, font uintptr, color uint32, flags uintptr) {
		u, _ := windows.UTF16FromString(s)
		procSelectObject.Call(dc, font)
		procSetTextColor.Call(dc, uintptr(color))
		procDrawTextW.Call(dc, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&r)), flags)
	}
	const dtCenter, dtVCenter, dtSingle, dtEllipsis = 0x1, 0x4, 0x20, 0x8000
	left := r.Right
	tip := ""
	for _, btn := range b.buttons {
		switch {
		case btn.id == b.hover && btn.id == "close":
			fill(btn.r, 0x2311E8) // red (BGR)
		case btn.on:
			fill(btn.r, accent)
		case btn.id == b.hover:
			fill(btn.r, 0x3C3C3C)
		}
		text(btn.glyph, btn.r, b.icons, 0xF0F0F0, dtCenter|dtVCenter|dtSingle)
		if btn.id == b.hover {
			tip = btn.tip
		}
		left = min(left, btn.r.Left)
	}
	// Left: the security (or what the hovered button does).
	pad := int32(10 * b.scale())
	shieldW := int32(28 * b.scale())
	text(glyphShield, rect{pad / 2, 0, pad/2 + shieldW, r.Bottom}, b.icons, 0x7AC77A, dtCenter|dtVCenter|dtSingle)
	label := b.owner.Name
	if c := b.owner.Chrome; c != nil && c.Security != nil {
		label += "   ·   " + c.Security()
	}
	if tip != "" {
		label = tip
	}
	text(label, rect{pad/2 + shieldW, 0, left - pad, r.Bottom}, b.font, 0xD8D8D8, dtVCenter|dtSingle|dtEllipsis)
}

func (b *bar) hit(x, y int32) string {
	for _, btn := range b.buttons {
		if x >= btn.r.Left && x < btn.r.Right && y >= btn.r.Top && y < btn.r.Bottom {
			return btn.id
		}
	}
	return ""
}

func (b *bar) click(id string) {
	c := b.owner.Chrome
	switch id {
	case "close":
		b.owner.Close()
		return
	case "minimize":
		procShowWindow.Call(uintptr(b.owner.HWND), swMinimize)
	case "fullscreen", "window":
		b.owner.ToggleFullscreen()
	case "pin":
		b.pinned = !b.pinned
		b.lastInside = time.Now()
	case "privacy", "lock_on_leave", "sound", "microphone":
		if c != nil && c.SetOption != nil {
			c.SetOption(id, !c.On[id]) // the worker answers with what is in force (Refresh)
		}
	case "session":
		b.sessionMenu()
	case "display":
		b.displayMenu()
	}
	b.layout()
	procInvalidateRect.Call(b.hwnd, 0, 0)
}

// displayMenu offers the worker's displays under the pointer.
func (b *bar) displayMenu() {
	c := b.owner.Chrome
	if c == nil || c.SwitchDisplay == nil {
		return
	}
	m, _, _ := procCreatePopupMenu.Call()
	defer procDestroyMenu.Call(m)
	for i, d := range c.Displays {
		flags := uintptr(0)
		if i == c.Display {
			flags = 0x8 // MF_CHECKED
		}
		label, _ := windows.UTF16PtrFromString(fmt.Sprintf("Display %d  (%d×%d)", i+1, d.W, d.H))
		procAppendMenuW.Call(m, flags, uintptr(i+1), uintptr(unsafe.Pointer(label)))
	}
	var p point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	const tpmReturnCmd, tpmNonotify = 0x0100, 0x0080
	sel, _, _ := procTrackPopupMenu.Call(m, tpmReturnCmd|tpmNonotify, uintptr(p.X), uintptr(p.Y), 0, b.hwnd, 0)
	if sel > 0 && int(sel)-1 != c.Display {
		c.SwitchDisplay(int(sel) - 1)
	}
}

// sessionMenu offers the worker's Windows sessions: the one shown (the
// console's) checked; choosing another moves it to the console. One in
// use over Remote Desktop would lose that connection: asked first.
func (b *bar) sessionMenu() {
	c := b.owner.Chrome
	if c == nil || c.SwitchSession == nil {
		return
	}
	m, _, _ := procCreatePopupMenu.Call()
	defer procDestroyMenu.Call(m)
	for i, s := range c.Sessions {
		flags := uintptr(0)
		if s.Console {
			flags = 0x8 // MF_CHECKED
		}
		label, _ := windows.UTF16PtrFromString(sessionLabel(s))
		procAppendMenuW.Call(m, flags, uintptr(i+1), uintptr(unsafe.Pointer(label)))
	}
	var p point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	const tpmReturnCmd, tpmNonotify = 0x0100, 0x0080
	sel, _, _ := procTrackPopupMenu.Call(m, tpmReturnCmd|tpmNonotify, uintptr(p.X), uintptr(p.Y), 0, b.hwnd, 0)
	if sel == 0 || int(sel) > len(c.Sessions) {
		return
	}
	s := c.Sessions[sel-1]
	if s.Console {
		return
	}
	if s.State == "active" { // somebody is connected to it over Remote Desktop
		text, _ := windows.UTF16PtrFromString(fmt.Sprintf("%s is in use over Remote Desktop (%s): showing it here disconnects that window. Show it?", s.User, s.Station))
		title, _ := windows.UTF16PtrFromString("Longarm")
		const mbYesNo, mbIconQuestion, idYes = 0x4, 0x20, 6
		if r, _, _ := procMessageBoxW.Call(uintptr(b.owner.HWND), uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), mbYesNo|mbIconQuestion); r != idYes {
			return
		}
	}
	c.SwitchSession(s.ID)
}

// sessionLabel: "CPC\alice - disconnected", "the sign-in screen", ...
func sessionLabel(s Session) string {
	who := s.User
	if who == "" {
		who = "the sign-in screen"
	}
	switch {
	case s.Console:
		return who + "  (shown)"
	case s.State == "disconnected":
		return who + "  (disconnected)"
	case s.Station != "":
		return who + "  (over Remote Desktop: " + s.Station + ")"
	}
	return who + "  (" + s.State + ")"
}

// Refresh redraws the title bar after the viewer changed Chrome.
func (w *Window) Refresh() {
	if theBar != nil && theBar.shown {
		theBar.layout()
		procInvalidateRect.Call(theBar.hwnd, 0, 0)
	}
}

// TopInset is how much of the client area the title bar covers (the video
// goes below it; nothing in full screen).
func (w *Window) TopInset() int {
	if w.fullscreen || theBar == nil {
		return 0
	}
	return int(theBar.height())
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
		case wmMouseMove:
			x, y := lparamXY(lParam)
			if h := b.hit(int32(x), int32(y)); h != b.hover {
				b.hover = h
				procInvalidateRect.Call(b.hwnd, 0, 0)
			}
			b.lastInside = time.Now()
			procSetTimer.Call(b.hwnd, 1, 200, 0) // notices the pointer leaving
			return 0
		case wmLButtonDown:
			x, y := lparamXY(lParam)
			if b.hit(int32(x), int32(y)) == "" && !b.owner.fullscreen {
				// The bar is the window's title bar: drag the window.
				procReleaseCapture.Call()
				procSendMessageW.Call(uintptr(b.owner.HWND), wmNCLButtonDown, htCaption, 0)
			}
			return 0
		case wmLButtonUp:
			x, y := lparamXY(lParam)
			if h := b.hit(int32(x), int32(y)); h != "" {
				b.click(h)
			}
			return 0
		case wmLButtonDblClk:
			x, y := lparamXY(lParam)
			if b.hit(int32(x), int32(y)) == "" {
				b.owner.ToggleFullscreen()
			}
			return 0
		}
	}
	r, _, _ := syscall.SyscallN(procDefWindowProcW.Addr(), hwnd, message, wParam, lParam)
	return r
}
