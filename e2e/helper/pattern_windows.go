//go:build windows

package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pattern shows the remote desktop test's picture (e2e/desktop_test.go)
// over the whole primary display, topmost, for secs seconds: red, green,
// blue and white quadrants and a black square in the middle that turns
// yellow when clicked. It writes "ready W H" to stdout once shown and
// every click ("click X Y") and character ("char C") to logPath.
func pattern(logPath string, secs int) {
	runtime.LockOSThread()
	user32 := windows.NewLazySystemDLL("user32.dll")
	gdi32 := windows.NewLazySystemDLL("gdi32.dll")
	proc := func(n string) *windows.LazyProc { return user32.NewProc(n) }
	setDPI := proc("SetProcessDpiAwarenessContext")
	if setDPI.Find() == nil {
		setDPI.Call(^uintptr(3)) // per monitor v2
	}
	log, err := os.Create(logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer log.Close()
	sw, _, _ := proc("GetSystemMetrics").Call(0)
	sh, _, _ := proc("GetSystemMetrics").Call(1)
	w, h := int32(sw), int32(sh)
	clicked := false
	createBrush := gdi32.NewProc("CreateSolidBrush")
	fillRect := proc("FillRect")
	brush := func(r, g, b byte) uintptr {
		h, _, _ := createBrush.Call(uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16)
		return h
	}
	red, green, blue, white, black, yellow := brush(255, 0, 0), brush(0, 255, 0), brush(0, 0, 255), brush(255, 255, 255), brush(0, 0, 0), brush(255, 255, 0)
	type rc struct{ l, t, r, b int32 }
	paint := func(hwnd uintptr) {
		var ps [72]byte // PAINTSTRUCT
		dc, _, _ := proc("BeginPaint").Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		fill := func(r rc, br uintptr) { fillRect.Call(dc, uintptr(unsafe.Pointer(&r)), br) }
		fill(rc{0, 0, w / 2, h / 2}, red)
		fill(rc{w / 2, 0, w, h / 2}, green)
		fill(rc{0, h / 2, w / 2, h}, blue)
		fill(rc{w / 2, h / 2, w, h}, white)
		sq := black
		if clicked {
			sq = yellow
		}
		fill(rc{w/2 - 60, h/2 - 60, w/2 + 60, h/2 + 60}, sq)
		proc("EndPaint").Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	}
	const wmPaint, wmLButtonDown, wmChar, wmDestroy, wmTimer = 0x000F, 0x0201, 0x0102, 0x0002, 0x0113
	defProc := proc("DefWindowProcW")
	wndProc := windows.NewCallback(func(hwnd, msg, wp, lp uintptr) uintptr {
		switch msg {
		case wmPaint:
			paint(hwnd)
			return 0
		case wmLButtonDown:
			fmt.Fprintf(log, "click %d %d\n", int16(lp), int16(lp>>16))
			log.Sync()
			clicked = true
			proc("InvalidateRect").Call(hwnd, 0, 0)
			return 0
		case wmChar:
			fmt.Fprintf(log, "char %c\n", rune(wp))
			log.Sync()
			return 0
		case wmTimer:
			proc("DestroyWindow").Call(hwnd)
			return 0
		case wmDestroy:
			proc("PostQuitMessage").Call(0)
			return 0
		}
		r, _, _ := syscall.SyscallN(defProc.Addr(), hwnd, msg, wp, lp)
		return r
	})
	cls, _ := windows.UTF16PtrFromString("LongarmE2EPattern")
	type wndClassEx struct {
		Size, Style                   uint32
		WndProc                       uintptr
		ClsExtra, WndExtra            int32
		Instance, Icon, Cursor, Brush uintptr
		MenuName, ClassName           *uint16
		IconSm                        uintptr
	}
	wc := wndClassEx{WndProc: wndProc, ClassName: cls}
	wc.Size = uint32(unsafe.Sizeof(wc))
	proc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&wc)))
	const wsExTopmost, wsPopup, wsVisible = 0x8, 0x80000000, 0x10000000
	hwnd, _, err := proc("CreateWindowExW").Call(wsExTopmost, uintptr(unsafe.Pointer(cls)), 0, wsPopup|wsVisible, 0, 0, uintptr(w), uintptr(h), 0, 0, 0, 0)
	if hwnd == 0 {
		fmt.Fprintln(os.Stderr, "CreateWindowEx:", err)
		os.Exit(1)
	}
	proc("SetForegroundWindow").Call(hwnd)
	proc("SetTimer").Call(hwnd, 1, uintptr(secs*1000), 0)
	proc("UpdateWindow").Call(hwnd)
	time.AfterFunc(500*time.Millisecond, func() { fmt.Printf("ready %d %d\n", w, h) })
	var m [48]byte // MSG
	getMessage, translate, dispatch := proc("GetMessageW"), proc("TranslateMessage"), proc("DispatchMessageW")
	for {
		r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			return
		}
		translate.Call(uintptr(unsafe.Pointer(&m)))
		dispatch.Call(uintptr(unsafe.Pointer(&m)))
	}
}
