//go:build windows

package screen

import (
	"fmt"
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

func send(in ...input) error {
	n, _, err := procSendInput.Call(uintptr(len(in)), uintptr(unsafe.Pointer(&in[0])), unsafe.Sizeof(in[0]))
	if int(n) != len(in) {
		// Blocked by UIPI (a higher-integrity window has focus) or the
		// input desktop is not ours (Winlogon): see docs/remote-desktop.md.
		return fmt.Errorf("SendInput: %d of %d sent: %v", n, len(in), err)
	}
	return nil
}

func metric(i int) int32 {
	r, _, _ := procGetSystemMetrics.Call(uintptr(i))
	return int32(r)
}

// Buttons for MouseButton.
const (
	ButtonLeft = iota
	ButtonMiddle
	ButtonRight
)

// MouseMove puts the pointer at desktop coordinates (x, y) (physical
// pixels; a display's are Display.X+fx, Display.Y+fy).
func MouseMove(x, y int) error {
	vx, vy := metric(smXVirtualScreen), metric(smYVirtualScreen)
	vw, vh := metric(smCXVirtualScreen), metric(smCYVirtualScreen)
	if vw <= 1 || vh <= 1 {
		return fmt.Errorf("SendInput: no virtual screen")
	}
	// Normalized 0..65535 over the virtual desktop.
	nx := (int64(x-int(vx))*65535 + int64(vw-1)/2) / int64(vw-1)
	ny := (int64(y-int(vy))*65535 + int64(vh-1)/2) / int64(vh-1)
	return send(mouse(mouseInput{Dx: int32(nx), Dy: int32(ny), Flags: mouseMove | mouseAbsolute | mouseVirtual}))
}

// MouseButton presses or releases a button where the pointer is.
func MouseButton(button int, down bool) error {
	flags := map[[2]int]uint32{
		{ButtonLeft, 1}: mouseLeftDown, {ButtonLeft, 0}: mouseLeftUp,
		{ButtonMiddle, 1}: mouseMiddleDown, {ButtonMiddle, 0}: mouseMiddleUp,
		{ButtonRight, 1}: mouseRightDown, {ButtonRight, 0}: mouseRightUp,
	}[[2]int{button, b2i(down)}]
	if flags == 0 {
		return fmt.Errorf("SendInput: unknown button %d", button)
	}
	return send(mouse(mouseInput{Flags: flags}))
}

// MouseWheel scrolls by delta (120 = one notch; positive: up / right).
func MouseWheel(dx, dy int) error {
	var in []input
	if dy != 0 {
		in = append(in, mouse(mouseInput{MouseData: uint32(int32(dy)), Flags: mouseWheel}))
	}
	if dx != 0 {
		in = append(in, mouse(mouseInput{MouseData: uint32(int32(dx)), Flags: mouseHWheel}))
	}
	if len(in) == 0 {
		return nil
	}
	return send(in...)
}

// Key presses or releases a key by its PC/AT set-1 scan code (what the
// browser's KeyboardEvent.code maps to; extended keys have 0xE0 in the
// high byte, e.g. 0xE04B is the left arrow). Scan codes, not virtual keys
// or characters: the worker's keyboard layout decides the character, as on
// a local keyboard.
func Key(scan uint16, down bool) error {
	k := keybdInput{Scan: scan & 0xFF, Flags: keyScancode}
	if scan>>8 == 0xE0 {
		k.Flags |= keyExtended
	}
	if !down {
		k.Flags |= keyUp
	}
	var in input
	in.Type = inputKeyboard
	*(*keybdInput)(unsafe.Pointer(&in.U[0])) = k
	return send(in)
}

func mouse(m mouseInput) input {
	var in input
	in.Type = inputMouse
	*(*mouseInput)(unsafe.Pointer(&in.U[0])) = m
	return in
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
