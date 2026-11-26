package screen

import "testing"

func TestMacKeys(t *testing.T) {
	for _, c := range []struct {
		scan uint16
		vk   uint16
		name string
	}{
		{0x1E, 0x00, "A"},
		{0x10, 0x0C, "Q"},
		{0x2C, 0x06, "Z"},
		{0x02, 0x12, "1"},
		{0x0B, 0x1D, "0"},
		{0x1C, 0x24, "Enter"},
		{0x0E, 0x33, "Backspace"},
		{0x39, 0x31, "Space"},
		{0x38, 0x3A, "Alt = Option"},
		{0xE038, 0x3D, "right Alt = right Option"},
		{0xE05B, 0x37, "Windows = Command"},
		{0xE052, 0x72, "Insert = Help"},
		{0xE053, 0x75, "Delete = forward delete"},
		{0xE048, 0x7E, "Up"},
		{0xE04B, 0x7B, "Left"},
		{0xE047, 0x73, "Home"},
		{0xE04F, 0x77, "End"},
		{0xE01C, 0x4C, "keypad Enter"},
		{0x3B, 0x7A, "F1"},
		{0x58, 0x6F, "F12"},
	} {
		vk, ok := macKeyCode(c.scan)
		if !ok || vk != c.vk {
			t.Errorf("%s: scan 0x%X gives key code 0x%X (%v), want 0x%X", c.name, c.scan, vk, ok, c.vk)
		}
		if s, ok := macScan(c.vk); !ok || s != c.scan {
			t.Errorf("%s: key code 0x%X gives scan 0x%X (%v), want 0x%X", c.name, c.vk, s, ok, c.scan)
		}
	}
	// Every scan code is mapped once; every key code maps back to a scan
	// code that maps to it again.
	seen := map[uint16]bool{}
	for _, e := range macKeys {
		if seen[e[0]] {
			t.Errorf("scan 0x%X is in the table twice", e[0])
		}
		seen[e[0]] = true
		s, ok := macScan(e[1])
		if !ok {
			t.Fatalf("key code 0x%X has no scan code", e[1])
		}
		if vk, _ := macKeyCode(s); vk != e[1] {
			t.Errorf("key code 0x%X → scan 0x%X → key code 0x%X", e[1], s, vk)
		}
	}
	// The whole main block of a US keyboard.
	for s := uint16(0x01); s <= 0x53; s++ {
		if s == 0x54 || s == 0x55 {
			continue
		}
		if _, ok := macKeyCode(s); !ok {
			t.Errorf("scan 0x%X has no key code", s)
		}
	}
	if _, ok := macKeyCode(0xE05E); ok {
		t.Error("Power should not be mapped")
	}
	if m := macModifier(0x38); m&0x20000 == 0 {
		t.Errorf("Shift's flags: %#x", m)
	}
	if macModifier(0x00) != 0 {
		t.Error("A is not a modifier")
	}
}
