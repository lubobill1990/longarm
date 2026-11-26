//go:build windows

package screen

import (
	"fmt"
	"runtime"
	"unicode/utf16"
	"unsafe"
)

var (
	procSendInput        = modUser32.NewProc("SendInput")
	procGetSystemMetrics = modUser32.NewProc("GetSystemMetrics")
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseMove       = 0x0001
	mouseLeftDown   = 0x0002
	mouseLeftUp     = 0x0004
	mouseRightDown  = 0x0008
	mouseRightUp    = 0x0010
	mouseMiddleDown = 0x0020
	mouseMiddleUp   = 0x0040
	mouseWheel      = 0x0800
	mouseHWheel     = 0x1000
	mouseVirtual    = 0x4000
	mouseAbsolute   = 0x8000

	keyExtended = 0x0001
	keyUp       = 0x0002
	keyUnicode  = 0x0004
	keyScancode = 0x0008

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
)

// input is INPUT on 64-bit Windows: a type, padding, then the union
// (MOUSEINPUT is the largest member: 32 bytes).
type input struct {
	Type uint32
	_    uint32
	U    [32]byte
}

type mouseInput struct {
	Dx, Dy    int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	_         uint32
	ExtraInfo uintptr
}

type keybdInput struct {
	VK, Scan  uint16
	Flags     uint32
	Time      uint32
	_         uint32
	ExtraInfo uintptr
}

// winInjector injects with SendInput from one locked thread that follows
// the input desktop, so input also reaches UAC prompts and the Winlogon
// screens when the process may open them (SYSTEM in the session).
type winInjector struct {
	calls chan func()
}

func newInjector() (Injector, error) {
	in := &winInjector{calls: make(chan func())}
	go func() {
		runtime.LockOSThread() // never unlocked: the thread ends with the goroutine
		for f := range in.calls {
			FollowInputDesktop()
			f()
		}
	}()
	return in, nil
}

func (in *winInjector) do(f func() error) (err error) {
	done := make(chan struct{})
	in.calls <- func() { err = f(); close(done) }
	<-done
	return err
}

func (in *winInjector) Close() { close(in.calls) }

func send(in ...input) error {
	n, _, err := procSendInput.Call(uintptr(len(in)), uintptr(unsafe.Pointer(&in[0])), unsafe.Sizeof(in[0]))
	if int(n) != len(in) {
		// Blocked by UIPI (a higher-integrity window has focus) or the
		// input desktop is not ours (Winlogon, without SYSTEM).
		return fmt.Errorf("SendInput: %d of %d sent: %v", n, len(in), err)
	}
	return nil
}

func metric(i int) int32 {
	r, _, _ := procGetSystemMetrics.Call(uintptr(i))
	return int32(r)
}

func (in *winInjector) Move(x, y int) error {
	return in.do(func() error {
		vx, vy := metric(smXVirtualScreen), metric(smYVirtualScreen)
		vw, vh := metric(smCXVirtualScreen), metric(smCYVirtualScreen)
		if vw <= 1 || vh <= 1 {
			return fmt.Errorf("SendInput: no virtual screen")
		}
		// Normalized 0..65535 over the virtual desktop.
		nx := (int64(x-int(vx))*65535 + int64(vw-1)/2) / int64(vw-1)
		ny := (int64(y-int(vy))*65535 + int64(vh-1)/2) / int64(vh-1)
		return send(mouse(mouseInput{Dx: int32(nx), Dy: int32(ny), Flags: mouseMove | mouseAbsolute | mouseVirtual}))
	})
}

func (in *winInjector) Button(button int, down bool) error {
	var flags uint32
	switch button {
	case ButtonLeft:
		flags = pick(down, mouseLeftDown, mouseLeftUp)
	case ButtonMiddle:
		flags = pick(down, mouseMiddleDown, mouseMiddleUp)
	case ButtonRight:
		flags = pick(down, mouseRightDown, mouseRightUp)
	default:
		return fmt.Errorf("SendInput: unknown button %d", button)
	}
	return in.do(func() error { return send(mouse(mouseInput{Flags: flags})) })
}

func (in *winInjector) Wheel(dx, dy int) error {
	var ev []input
	if dy != 0 {
		ev = append(ev, mouse(mouseInput{MouseData: uint32(int32(dy)), Flags: mouseWheel}))
	}
	if dx != 0 {
		ev = append(ev, mouse(mouseInput{MouseData: uint32(int32(dx)), Flags: mouseHWheel}))
	}
	if len(ev) == 0 {
		return nil
	}
	return in.do(func() error { return send(ev...) })
}

func (in *winInjector) Key(scan uint16, down bool) error {
	k := keybdInput{Scan: scan & 0xFF, Flags: keyScancode}
	if scan>>8 == 0xE0 {
		k.Flags |= keyExtended
	}
	if !down {
		k.Flags |= keyUp
	}
	return in.do(func() error { return send(keyboard(k)) })
}

func (in *winInjector) Text(s string) error {
	var ev []input
	for _, u := range utf16.Encode([]rune(s)) {
		ev = append(ev, keyboard(keybdInput{Scan: u, Flags: keyUnicode}), keyboard(keybdInput{Scan: u, Flags: keyUnicode | keyUp}))
	}
	if len(ev) == 0 {
		return nil
	}
	return in.do(func() error { return send(ev...) })
}

// SecureAttentionVia, when set, sends Ctrl+Alt+Del (the desktop agent asks
// the Longarm Desktop service: Windows takes it only from session 0).
var SecureAttentionVia func() error

func (in *winInjector) SecureAttention() error {
	switch {
	case SecureAttentionVia != nil:
		return SecureAttentionVia()
	case InServiceSession():
		return SendSAS()
	}
	return fmt.Errorf("%w: Ctrl+Alt+Del needs the Longarm Desktop service", ErrUnsupported)
}

func pick(down bool, d, u uint32) uint32 {
	if down {
		return d
	}
	return u
}

func mouse(m mouseInput) input {
	var in input
	in.Type = inputMouse
	*(*mouseInput)(unsafe.Pointer(&in.U[0])) = m
	return in
}

func keyboard(k keybdInput) input {
	var in input
	in.Type = inputKeyboard
	*(*keybdInput)(unsafe.Pointer(&in.U[0])) = k
	return in
}
