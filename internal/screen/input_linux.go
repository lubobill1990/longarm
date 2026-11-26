//go:build linux

package screen

import (
	"errors"
	"fmt"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// x11Injector types and clicks through XTEST, as if on the X server's own
// keyboard and mouse.
type x11Injector struct {
	c        *xgb.Conn
	root     xproto.Window
	min, max xproto.Keycode
	wheel    [2]int // remainders below a notch (x, y)
}

func newInjector() (Injector, error) {
	c, err := xConnect()
	if err != nil {
		return nil, err
	}
	if err := xtest.Init(c); err != nil {
		c.Close()
		return nil, fmt.Errorf("the X server has no XTEST: %w", err)
	}
	if _, err := xtest.GetVersion(c, 2, 2).Reply(); err != nil {
		c.Close()
		return nil, fmt.Errorf("XTEST: %w", err)
	}
	setup := xproto.Setup(c)
	return &x11Injector{c: c, root: setup.DefaultScreen(c).Root, min: setup.MinKeycode, max: setup.MaxKeycode}, nil
}

func (in *x11Injector) fake(typ, detail byte, x, y int16) error {
	return xtest.FakeInputChecked(in.c, typ, detail, 0, in.root, x, y, 0).Check()
}

func (in *x11Injector) Move(x, y int) error {
	return in.fake(xproto.MotionNotify, 0, int16(x), int16(y)) // detail 0: absolute
}

func (in *x11Injector) Button(button int, down bool) error {
	if button < ButtonLeft || button > ButtonRight {
		return fmt.Errorf("no mouse button %d", button)
	}
	return in.fake(pick(down, xproto.ButtonPress, xproto.ButtonRelease), byte(button), 0, 0)
}

func pick(down bool, d, u byte) byte {
	if down {
		return d
	}
	return u
}

// Wheel clicks X's wheel buttons (4 up, 5 down, 6 left, 7 right) once a
// notch.
func (in *x11Injector) Wheel(dx, dy int) error {
	for i, v := range [2]int{dx, dy} {
		in.wheel[i] += v
		for in.wheel[i] >= 120 || in.wheel[i] <= -120 {
			b := byte(4) // up
			step := 120
			switch {
			case i == 1 && in.wheel[i] < 0:
				b, step = 5, -120
			case i == 0 && in.wheel[i] > 0:
				b = 7
			case i == 0:
				b, step = 6, -120
			}
			in.wheel[i] -= step
			if err := in.fake(xproto.ButtonPress, b, 0, 0); err != nil {
				return err
			}
			if err := in.fake(xproto.ButtonRelease, b, 0, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

func (in *x11Injector) Key(scan uint16, down bool) error {
	code := evdevFromSet1(scan)
	if code == 0 || code+8 > uint16(in.max) {
		return fmt.Errorf("no key for scan code %#x", scan)
	}
	return in.fake(pick(down, xproto.KeyPress, xproto.KeyRelease), byte(code+8), 0, 0)
}

const keyShiftL = 42 + 8

// Text types each character with the key that has it in the current
// keymap (with Shift for the second level); characters no key has are
// typed with a spare keycode mapped to them for the moment, as xdotool
// does.
func (in *x11Injector) Text(s string) error {
	n := byte(in.max - in.min + 1)
	km, err := xproto.GetKeyboardMapping(in.c, in.min, n).Reply()
	if err != nil {
		return err
	}
	per := int(km.KeysymsPerKeycode)
	find := func(ks uint32) (xproto.Keycode, bool, bool) {
		for i := 0; i < int(n); i++ {
			for lvl := 0; lvl < min(per, 2); lvl++ {
				if uint32(km.Keysyms[i*per+lvl]) == ks {
					return in.min + xproto.Keycode(i), lvl == 1, true
				}
			}
		}
		return 0, false, false
	}
	spare := xproto.Keycode(0)
	for i := int(n) - 1; i >= 0 && spare == 0; i-- {
		empty := true
		for lvl := 0; lvl < per; lvl++ {
			empty = empty && km.Keysyms[i*per+lvl] == 0
		}
		if empty {
			spare = in.min + xproto.Keycode(i)
		}
	}
	remapped := false
	defer func() {
		if remapped {
			// Clients read the mapping when they handle the key: give them
			// a moment before taking it back.
			time.Sleep(50 * time.Millisecond)
			xproto.ChangeKeyboardMapping(in.c, 1, spare, byte(per), make([]xproto.Keysym, per))
			in.c.Sync()
		}
	}()
	for _, r := range s {
		ks := keysymFromRune(r)
		code, shift, ok := find(ks)
		if !ok {
			if spare == 0 {
				return errors.New("no spare keycode to type " + string(r))
			}
			syms := make([]xproto.Keysym, per)
			for i := range syms {
				syms[i] = xproto.Keysym(ks)
			}
			if err := xproto.ChangeKeyboardMappingChecked(in.c, 1, spare, byte(per), syms).Check(); err != nil {
				return err
			}
			remapped = true
			time.Sleep(10 * time.Millisecond) // the clients' MappingNotify first
			code, shift = spare, false
		}
		if shift {
			if err := in.fake(xproto.KeyPress, keyShiftL, 0, 0); err != nil {
				return err
			}
		}
		err := in.fake(xproto.KeyPress, byte(code), 0, 0)
		if err == nil {
			err = in.fake(xproto.KeyRelease, byte(code), 0, 0)
		}
		if shift {
			in.fake(xproto.KeyRelease, keyShiftL, 0, 0)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (in *x11Injector) SecureAttention() error { return ErrUnsupported }

func (in *x11Injector) Close() { in.c.Close() }
