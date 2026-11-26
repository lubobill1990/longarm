//go:build darwin

package screen

import (
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf16"
)

// Quartz event types and fields (CGEventTypes.h).
const (
	cgLeftMouseDown     = 1
	cgLeftMouseUp       = 2
	cgRightMouseDown    = 3
	cgRightMouseUp      = 4
	cgMouseMoved        = 5
	cgLeftMouseDragged  = 6
	cgRightMouseDragged = 7
	cgOtherMouseDown    = 25
	cgOtherMouseUp      = 26
	cgOtherMouseDragged = 27

	cgButtonLeft, cgButtonRight, cgButtonCenter = 0, 1, 2

	cgHIDEventTap          = 0
	cgScrollEventUnitLine  = 1
	cgMouseEventClickState = 1
	cgFlagAlphaShift       = 0x10000 // Caps Lock
)

// macInjector posts Quartz events at the HID level, as if from the
// devices. It keeps the state macOS would otherwise lose between events:
// the pointer (drags while a button is held), held modifiers (posted key
// events carry their flags) and the click count (double clicks).
type macInjector struct {
	displays []macDisplay
	pos      cgPoint // points, global
	havePos  bool
	held     [4]bool // by Button* (1-3)
	mods     map[uint16]uint64
	caps     bool

	lastDown   time.Time
	lastButton int
	lastAt     cgPoint
	clicks     int64

	wheelX, wheelY int // the part of a line not scrolled yet (1/120ths)
}

func newInjector() (Injector, error) {
	if err := macLoad(); err != nil {
		return nil, err
	}
	if !axIsProcessTrusted() {
		return nil, errors.New("macOS has not allowed Longarm to control the computer: System Settings > Privacy & Security > Accessibility (longarmd)")
	}
	return &macInjector{mods: map[uint16]uint64{}}, nil
}

func (in *macInjector) Close() {}

func (in *macInjector) flags() uint64 {
	var f uint64
	for _, m := range in.mods {
		f |= m
	}
	if in.caps {
		f |= cgFlagAlphaShift
	}
	return f
}

func (in *macInjector) post(ev uintptr, err string) error {
	if ev == 0 {
		return errors.New(err)
	}
	cgEventSetFlags(ev, in.flags())
	cgEventPost(cgHIDEventTap, ev)
	cfRelease(ev)
	return nil
}

// toPoints maps desktop pixels (Display coordinates) to Quartz's global
// points.
func (in *macInjector) toPoints(x, y int) (cgPoint, error) {
	for try := 0; try < 2; try++ {
		if try == 1 || in.displays == nil {
			ds, err := macDisplays()
			if err != nil {
				return cgPoint{}, err
			}
			in.displays = ds
		}
		for _, d := range in.displays {
			if x >= d.X && y >= d.Y && x < d.X+d.W && y < d.Y+d.H {
				return cgPoint{X: d.pts.Origin.X + float64(x-d.X)/d.scale, Y: d.pts.Origin.Y + float64(y-d.Y)/d.scale}, nil
			}
		}
	}
	return cgPoint{}, fmt.Errorf("%d,%d is on no display", x, y)
}

func (in *macInjector) current() cgPoint {
	if !in.havePos {
		if ev := cgEventCreate(0); ev != 0 {
			in.pos = cgEventGetLocation(ev)
			cfRelease(ev)
		}
		in.havePos = true
	}
	return in.pos
}

func (in *macInjector) Move(x, y int) error {
	p, err := in.toPoints(x, y)
	if err != nil {
		return err
	}
	in.pos, in.havePos = p, true
	typ, button := uint32(cgMouseMoved), uint32(cgButtonLeft)
	switch {
	case in.held[ButtonLeft]:
		typ = cgLeftMouseDragged
	case in.held[ButtonRight]:
		typ, button = cgRightMouseDragged, cgButtonRight
	case in.held[ButtonMiddle]:
		typ, button = cgOtherMouseDragged, cgButtonCenter
	}
	return in.post(cgEventCreateMouseEvent(0, typ, p, button), "CGEventCreateMouseEvent failed")
}

func (in *macInjector) Button(button int, down bool) error {
	var typ, cgButton uint32
	switch button {
	case ButtonLeft:
		typ, cgButton = uint32(pick(down, cgLeftMouseDown, cgLeftMouseUp)), cgButtonLeft
	case ButtonRight:
		typ, cgButton = uint32(pick(down, cgRightMouseDown, cgRightMouseUp)), cgButtonRight
	case ButtonMiddle:
		typ, cgButton = uint32(pick(down, cgOtherMouseDown, cgOtherMouseUp)), cgButtonCenter
	default:
		return fmt.Errorf("unknown button %d", button)
	}
	p := in.current()
	if down {
		// A press soon after the last one, at the same place, is the next
		// click of a double (triple) click: apps read the count.
		if button == in.lastButton && time.Since(in.lastDown) < 500*time.Millisecond &&
			math.Abs(p.X-in.lastAt.X) < 4 && math.Abs(p.Y-in.lastAt.Y) < 4 {
			in.clicks++
		} else {
			in.clicks = 1
		}
		in.lastDown, in.lastButton, in.lastAt = time.Now(), button, p
	}
	in.held[button] = down
	ev := cgEventCreateMouseEvent(0, typ, p, cgButton)
	if ev != 0 {
		cgEventSetIntegerValueField(ev, cgMouseEventClickState, max(in.clicks, 1))
	}
	return in.post(ev, "CGEventCreateMouseEvent failed")
}

func pick(down bool, d, u int) int {
	if down {
		return d
	}
	return u
}

// Wheel scrolls by lines: a notch (120) is one, as a Mac mouse wheel's
// unaccelerated step; smaller deltas add up.
func (in *macInjector) Wheel(dx, dy int) error {
	in.wheelX += dx
	in.wheelY += dy
	lx, ly := in.wheelX/120, in.wheelY/120
	in.wheelX -= lx * 120
	in.wheelY -= ly * 120
	if lx == 0 && ly == 0 {
		return nil
	}
	// Quartz: wheel 1 positive scrolls up, wheel 2 positive scrolls left.
	return in.post(cgEventCreateScrollWheelEv2(0, cgScrollEventUnitLine, 2, int32(ly), int32(-lx), 0), "CGEventCreateScrollWheelEvent2 failed")
}

func (in *macInjector) Key(scan uint16, down bool) error {
	vk, ok := macKeyCode(scan)
	if !ok {
		return fmt.Errorf("no macOS key for scan code 0x%X", scan)
	}
	switch m := macModifier(vk); {
	case m != 0 && down:
		in.mods[vk] = m
	case m != 0:
		delete(in.mods, vk)
	case vk == 0x39 && down: // Caps Lock toggles on its press
		in.caps = !in.caps
	}
	return in.post(cgEventCreateKeyboardEvent(0, vk, down), "CGEventCreateKeyboardEvent failed")
}

// Text types each character as a key event carrying it (no modifiers),
// so it needs no key of the worker's layout.
func (in *macInjector) Text(s string) error {
	for _, r := range s {
		u := utf16.Encode([]rune{r})
		for _, down := range []bool{true, false} {
			ev := cgEventCreateKeyboardEvent(0, 0, down)
			if ev == 0 {
				return errors.New("CGEventCreateKeyboardEvent failed")
			}
			cgEventKeyboardSetUnicodeStr(ev, uint(len(u)), &u[0])
			cgEventSetFlags(ev, 0)
			cgEventPost(cgHIDEventTap, ev)
			cfRelease(ev)
		}
	}
	return nil
}

func (in *macInjector) SecureAttention() error {
	return fmt.Errorf("%w: macOS has no Ctrl+Alt+Del", ErrUnsupported)
}
