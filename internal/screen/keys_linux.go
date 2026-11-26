//go:build linux

package screen

// The wire's keys are PC/AT set-1 scan codes (0xE0xx: extended), as the
// Windows viewer's keyboard hook reports them. X keycodes are Linux evdev
// codes + 8 (the evdev/xkb keymap every current X server uses).

// set1ToEvdev maps set-1 scan codes to evdev key codes.
var set1ToEvdev = map[uint16]uint16{
	// 0x01-0x53 are the evdev codes themselves (Esc .. keypad dot).
	0x56: 86,  // 102nd (ISO <>)
	0x57: 87,  // F11
	0x58: 88,  // F12
	0x59: 117, // keypad =
	0x64: 183, // F13 .. F23
	0x65: 184,
	0x66: 185,
	0x67: 186,
	0x68: 187,
	0x69: 188,
	0x6A: 189,
	0x6B: 190,
	0x6C: 191,
	0x6D: 192,
	0x6E: 193,
	0x76: 194, // F24
	0x70: 93,  // Katakana/Hiragana
	0x73: 89,  // Ro
	0x79: 92,  // Henkan
	0x7B: 94,  // Muhenkan
	0x7D: 124, // Yen
	0xF1: 123, // Hanja
	0xF2: 122, // Hangul

	0xE010: 165, // previous track
	0xE019: 163, // next track
	0xE01C: 96,  // keypad Enter
	0xE01D: 97,  // right Ctrl
	0xE020: 113, // mute
	0xE022: 164, // play/pause
	0xE024: 166, // stop
	0xE02E: 114, // volume down
	0xE030: 115, // volume up
	0xE035: 98,  // keypad /
	0xE037: 99,  // Print Screen
	0xE038: 100, // right Alt
	0xE046: 119, // Pause (Ctrl+Break)
	0xE047: 102, // Home
	0xE048: 103, // Up
	0xE049: 104, // Page Up
	0xE04B: 105, // Left
	0xE04D: 106, // Right
	0xE04F: 107, // End
	0xE050: 108, // Down
	0xE051: 109, // Page Down
	0xE052: 110, // Insert
	0xE053: 111, // Delete
	0xE05B: 125, // left Win
	0xE05C: 126, // right Win
	0xE05D: 127, // Menu
	0xE05E: 116, // Power
	0xE05F: 142, // Sleep
	0xE063: 143, // Wake
}

// Windows' keyboard hook reports Num Lock as 0x45 extended and Pause as
// plain 0x45 (its real sequence, E1 1D 45, does not fit the wire).
const (
	scanNumLock = 0xE045
	scanPause   = 0x45
	evNumLock   = 69
	evPause     = 119
)

// evdevFromSet1 returns the evdev code of a set-1 scan code (0: unknown).
func evdevFromSet1(scan uint16) uint16 {
	switch scan {
	case scanNumLock:
		return evNumLock
	case scanPause:
		return evPause
	}
	if scan >= 0x01 && scan <= 0x53 {
		return scan
	}
	return set1ToEvdev[scan]
}

var evdevToSet1 = func() map[uint16]uint16 {
	m := map[uint16]uint16{evNumLock: scanNumLock, evPause: scanPause}
	for s, e := range set1ToEvdev {
		if _, ok := m[e]; !ok {
			m[e] = s
		}
	}
	return m
}()

// set1FromEvdev is evdevFromSet1's inverse (0: no scan code).
func set1FromEvdev(code uint16) uint16 {
	if s, ok := evdevToSet1[code]; ok {
		return s
	}
	if code >= 0x01 && code <= 0x53 && code != evNumLock {
		return code
	}
	return 0
}

// keysymFromRune is the X keysym that types r.
func keysymFromRune(r rune) uint32 {
	switch {
	case r == '\n' || r == '\r':
		return 0xFF0D // Return
	case r == '\t':
		return 0xFF09
	case r == '\b':
		return 0xFF08
	case r >= 0x20 && r <= 0x7E, r >= 0xA0 && r <= 0xFF:
		return uint32(r)
	}
	return 0x01000000 | uint32(r)
}

// runeFromKeysym is the character a keysym types (0: none): Latin-1,
// Unicode keysyms and the few control keys keysymFromRune makes.
func runeFromKeysym(ks uint32) rune {
	switch {
	case ks >= 0x20 && ks <= 0x7E, ks >= 0xA0 && ks <= 0xFF:
		return rune(ks)
	case ks&0xFF000000 == 0x01000000:
		return rune(ks & 0xFFFFFF)
	case ks == 0xFF0D || ks == 0xFF8D:
		return '\r'
	case ks == 0xFF09:
		return '\t'
	case ks == 0xFF08:
		return '\b'
	}
	return 0
}
