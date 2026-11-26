//go:build linux

package screen

import "testing"

func TestSet1Evdev(t *testing.T) {
	for _, c := range []struct{ scan, ev uint16 }{
		{0x01, 1},     // Esc
		{0x1E, 30},    // A
		{0x1C, 28},    // Enter
		{0x2A, 42},    // left Shift
		{0x39, 57},    // Space
		{0x52, 82},    // keypad 0
		{0x57, 87},    // F11
		{0xE01C, 96},  // keypad Enter
		{0xE038, 100}, // AltGr
		{0xE048, 103}, // Up
		{0xE04D, 106}, // Right
		{0xE053, 111}, // Delete
		{0xE05B, 125}, // Win
		{0xE05D, 127}, // Menu
		{0xE045, 69},  // Num Lock, as Windows' hook reports it
		{0x45, 119},   // Pause, likewise
	} {
		if got := evdevFromSet1(c.scan); got != c.ev {
			t.Errorf("evdevFromSet1(%#x) = %d, want %d", c.scan, got, c.ev)
		}
		if got := set1FromEvdev(c.ev); got != c.scan {
			t.Errorf("set1FromEvdev(%d) = %#x, want %#x", c.ev, got, c.scan)
		}
	}
	if evdevFromSet1(0xE0FF) != 0 || set1FromEvdev(0x2FF) != 0 {
		t.Error("unknown keys map to something")
	}
	// Every key of the standard board goes both ways.
	for scan := uint16(1); scan <= 0x58; scan++ {
		if scan == 0x54 || scan == 0x55 {
			continue // not on a PC keyboard
		}
		if ev := evdevFromSet1(scan); ev == 0 || set1FromEvdev(ev) != scan {
			t.Errorf("scan %#x: evdev %d, back %#x", scan, ev, set1FromEvdev(ev))
		}
	}
	for s := range set1ToEvdev {
		if s != 0xE046 && set1FromEvdev(set1ToEvdev[s]) != s { // Ctrl+Break is Pause, which goes back as 0x45
			t.Errorf("scan %#x does not come back", s)
		}
	}
}

func TestKeysyms(t *testing.T) {
	for _, c := range []struct {
		r  rune
		ks uint32
	}{{'a', 'a'}, {'Z', 'Z'}, {'é', 0xE9}, {'€', 0x010020AC}, {'中', 0x01004E2D}, {'\n', 0xFF0D}} {
		if got := keysymFromRune(c.r); got != c.ks {
			t.Errorf("keysymFromRune(%q) = %#x, want %#x", c.r, got, c.ks)
		}
		if c.r != '\n' && runeFromKeysym(c.ks) != c.r {
			t.Errorf("runeFromKeysym(%#x) = %q, want %q", c.ks, runeFromKeysym(c.ks), c.r)
		}
	}
}

func TestStraightBGRA(t *testing.T) {
	// Premultiplied half-transparent white, opaque red, transparent.
	got := straightBGRA([]uint32{0x80808080, 0xFFFF0000, 0}, 3)
	want := []byte{255, 255, 255, 0x80, 0, 0, 255, 255, 0, 0, 0, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("straightBGRA = %v, want %v", got, want)
		}
	}
}
