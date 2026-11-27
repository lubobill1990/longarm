//go:build windows

package screen

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The clipboard (text, CF_UNICODETEXT) through a message-only window on a
// thread of its own: Windows tells it about changes
// (AddClipboardFormatListener), and SetClipboardData needs an owner window.

var (
	procAddClipboardFormatListener = modUser32.NewProc("AddClipboardFormatListener")
	procOpenClipboard              = modUser32.NewProc("OpenClipboard")
	procCloseClipboard             = modUser32.NewProc("CloseClipboard")
	procEmptyClipboard             = modUser32.NewProc("EmptyClipboard")
	procGetClipboardData           = modUser32.NewProc("GetClipboardData")
	procSetClipboardData           = modUser32.NewProc("SetClipboardData")
	procGlobalAlloc                = modKernel32.NewProc("GlobalAlloc")
	procGlobalFree                 = modKernel32.NewProc("GlobalFree")
	procGlobalLock                 = modKernel32.NewProc("GlobalLock")
	procGlobalUnlock               = modKernel32.NewProc("GlobalUnlock")
	procGlobalSize                 = modKernel32.NewProc("GlobalSize")
)

const (
	wmClipboardUpdate = 0x031D
	wmAppClipSet      = 0x8000 + 2 // WM_APP+2: Set's request is queued
	cfUnicodeText     = 13
	gmemMoveable      = 0x0002
	hwndMessage       = ^uintptr(2) // HWND_MESSAGE (-3)
	// MaxClipboardText caps what crosses the wire (UTF-8 bytes).
	MaxClipboardText = 1 << 20
)

type winClipboard struct {
	hwnd    uintptr
	thread  uint32
	changes chan string
	sets    chan setReq
	last    string // what Set put there (its own change is not reported)
}

type setReq struct {
	text string
	done chan error
}

var theClipboard *winClipboard // the message-only window's procedure finds it here

func openClipboard() (Clipboard, error) {
	if theClipboard != nil {
		return nil, errors.New("screen: one clipboard watcher per process")
	}
	c := &winClipboard{changes: make(chan string, 4), sets: make(chan setReq, 4)}
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		cls, _ := windows.UTF16PtrFromString("LongarmClipboard")
		wc := wndClassEx{WndProc: windows.NewCallback(clipProc), ClassName: cls}
		wc.Size = uint32(unsafe.Sizeof(wc))
		procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		h, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), 0, 0, 0, 0, 0, 0, hwndMessage, 0, 0, 0)
		if h == 0 {
			ready <- fmt.Errorf("clipboard window: %w", err)
			return
		}
		c.hwnd, c.thread = h, windows.GetCurrentThreadId()
		theClipboard = c
		if r, _, err := procAddClipboardFormatListener.Call(h); r == 0 {
			ready <- fmt.Errorf("AddClipboardFormatListener: %w", err)
			return
		}
		ready <- nil
		var m msg
		for {
			if r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) <= 0 {
				break
			}
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
		procDestroyWindow.Call(h)
		close(c.changes)
	}()
	if err := <-ready; err != nil {
		theClipboard = nil
		return nil, err
	}
	return c, nil
}

func (c *winClipboard) Changes() <-chan string { return c.changes }

func (c *winClipboard) Set(text string) error {
	r := setReq{text: text, done: make(chan error, 1)}
	c.sets <- r
	procPostMessageW.Call(c.hwnd, wmAppClipSet, 0, 0)
	select {
	case err := <-r.done:
		return err
	case <-time.After(5 * time.Second):
		return errors.New("clipboard: busy")
	}
}

func (c *winClipboard) Close() {
	procPostThreadMessageW.Call(uintptr(c.thread), wmQuit, 0, 0)
	theClipboard = nil
}

// open retries a moment: another program may hold the clipboard.
func (c *winClipboard) open() bool {
	for i := 0; i < 10; i++ {
		if r, _, _ := procOpenClipboard.Call(c.hwnd); r != 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func (c *winClipboard) read() (string, bool) {
	if !c.open() {
		return "", false
	}
	defer procCloseClipboard.Call()
	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", false // not text
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return "", false
	}
	defer procGlobalUnlock.Call(h)
	size, _, _ := procGlobalSize.Call(h)
	u := unsafe.Slice(*(**uint16)(unsafe.Pointer(&p)), size/2)
	for i, v := range u {
		if v == 0 {
			u = u[:i]
			break
		}
	}
	return string(utf16.Decode(u)), true
}

func (c *winClipboard) write(text string) error {
	u := utf16.Encode([]rune(text))
	u = append(u, 0)
	h, _, err := procGlobalAlloc.Call(gmemMoveable, uintptr(len(u)*2))
	if h == 0 {
		return fmt.Errorf("GlobalAlloc: %w", err)
	}
	p, _, _ := procGlobalLock.Call(h)
	copy(unsafe.Slice(*(**uint16)(unsafe.Pointer(&p)), len(u)), u)
	procGlobalUnlock.Call(h)
	if !c.open() {
		procGlobalFree.Call(h)
		return errors.New("clipboard: another program holds it")
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	if r, _, err := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData: %w", err)
	}
	c.last = text // the system owns h now
	return nil
}

func clipProc(hwnd, message, wParam, lParam uintptr) uintptr {
	c := theClipboard
	if c != nil && hwnd == c.hwnd {
		switch message {
		case wmClipboardUpdate:
			if text, ok := c.read(); ok && text != c.last && len(text) <= MaxClipboardText {
				c.last = text
				select {
				case c.changes <- text:
				default: // the reader is behind: it gets the next change
				}
			}
			return 0
		case wmAppClipSet:
			for {
				select {
				case r := <-c.sets:
					r.done <- c.write(r.text)
					continue
				default:
				}
				break
			}
			return 0
		}
	}
	r, _, _ := syscall.SyscallN(procDefWindowProcW.Addr(), hwnd, message, wParam, lParam)
	return r
}
