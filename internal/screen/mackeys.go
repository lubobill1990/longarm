package screen

// macKeys maps PC/AT set-1 scan codes (0xE0xx: extended) to macOS virtual
// key codes (Carbon's kVK_*, the US ANSI keyboard: the codes name key
// positions, so the worker's layout still decides the characters). The
// first entry of a key code is the one the viewer sends back (macScan).
var macKeys = [][2]uint16{
	{0x01, 0x35},                                                         // Esc
	{0x02, 0x12}, {0x03, 0x13}, {0x04, 0x14}, {0x05, 0x15}, {0x06, 0x17}, // 1-5
	{0x07, 0x16}, {0x08, 0x1A}, {0x09, 0x1C}, {0x0A, 0x19}, {0x0B, 0x1D}, // 6-0
	{0x0C, 0x1B},                                                         // -
	{0x0D, 0x18},                                                         // =
	{0x0E, 0x33},                                                         // Backspace (kVK_Delete)
	{0x0F, 0x30},                                                         // Tab
	{0x10, 0x0C}, {0x11, 0x0D}, {0x12, 0x0E}, {0x13, 0x0F}, {0x14, 0x11}, // Q W E R T
	{0x15, 0x10}, {0x16, 0x20}, {0x17, 0x22}, {0x18, 0x1F}, {0x19, 0x23}, // Y U I O P
	{0x1A, 0x21},                                                         // [
	{0x1B, 0x1E},                                                         // ]
	{0x1C, 0x24},                                                         // Enter (kVK_Return)
	{0x1D, 0x3B},                                                         // left Ctrl
	{0x1E, 0x00}, {0x1F, 0x01}, {0x20, 0x02}, {0x21, 0x03}, {0x22, 0x05}, // A S D F G
	{0x23, 0x04}, {0x24, 0x26}, {0x25, 0x28}, {0x26, 0x25}, // H J K L
	{0x27, 0x29},                                                         // ;
	{0x28, 0x27},                                                         // '
	{0x29, 0x32},                                                         // `
	{0x2A, 0x38},                                                         // left Shift
	{0x2B, 0x2A},                                                         // \
	{0x2C, 0x06}, {0x2D, 0x07}, {0x2E, 0x08}, {0x2F, 0x09}, {0x30, 0x0B}, // Z X C V B
	{0x31, 0x2D}, {0x32, 0x2E}, // N M
	{0x33, 0x2B},                                                         // ,
	{0x34, 0x2F},                                                         // .
	{0x35, 0x2C},                                                         // /
	{0x36, 0x3C},                                                         // right Shift
	{0x37, 0x43},                                                         // keypad *
	{0x38, 0x3A},                                                         // left Alt: Option
	{0x39, 0x31},                                                         // Space
	{0x3A, 0x39},                                                         // Caps Lock
	{0x3B, 0x7A}, {0x3C, 0x78}, {0x3D, 0x63}, {0x3E, 0x76}, {0x3F, 0x60}, // F1-F5
	{0x40, 0x61}, {0x41, 0x62}, {0x42, 0x64}, {0x43, 0x65}, {0x44, 0x6D}, // F6-F10
	{0x57, 0x67}, {0x58, 0x6F}, // F11 F12
	{0x64, 0x69}, {0x65, 0x6B}, {0x66, 0x71}, {0x67, 0x6A}, // F13-F16
	{0x68, 0x40}, {0x69, 0x4F}, {0x6A, 0x50}, {0x6B, 0x5A}, // F17-F20
	{0x45, 0x47},                             // Num Lock: keypad Clear (where a Mac keyboard has it)
	{0x47, 0x59}, {0x48, 0x5B}, {0x49, 0x5C}, // keypad 7 8 9
	{0x4A, 0x4E},                             // keypad -
	{0x4B, 0x56}, {0x4C, 0x57}, {0x4D, 0x58}, // keypad 4 5 6
	{0x4E, 0x45},                             // keypad +
	{0x4F, 0x53}, {0x50, 0x54}, {0x51, 0x55}, // keypad 1 2 3
	{0x52, 0x52},   // keypad 0
	{0x53, 0x41},   // keypad .
	{0x59, 0x51},   // keypad =
	{0x56, 0x0A},   // the ISO key left of Z (kVK_ISO_Section)
	{0x73, 0x5E},   // JIS Ro (kVK_JIS_Underscore)
	{0x7D, 0x5D},   // JIS Yen
	{0x7E, 0x5F},   // keypad , (kVK_JIS_KeypadComma)
	{0x70, 0x68},   // Katakana/Hiragana: kVK_JIS_Kana
	{0x7B, 0x66},   // Muhenkan: kVK_JIS_Eisu
	{0xF2, 0x68},   // Hangul (LANG1)
	{0xF1, 0x66},   // Hanja (LANG2)
	{0xE01C, 0x4C}, // keypad Enter
	{0xE01D, 0x3E}, // right Ctrl
	{0xE035, 0x4B}, // keypad /
	{0xE038, 0x3D}, // right Alt: right Option
	{0xE047, 0x73}, // Home
	{0xE048, 0x7E}, // Up
	{0xE049, 0x74}, // Page Up
	{0xE04B, 0x7B}, // Left
	{0xE04D, 0x7C}, // Right
	{0xE04F, 0x77}, // End
	{0xE050, 0x7D}, // Down
	{0xE051, 0x79}, // Page Down
	{0xE052, 0x72}, // Insert: Help
	{0xE053, 0x75}, // Delete: forward delete
	{0xE05B, 0x37}, // left Windows: Command
	{0xE05C, 0x36}, // right Windows: right Command
	{0xE05D, 0x6E}, // Menu (kVK_ContextualMenu)
	{0xE020, 0x4A}, // Mute
	{0xE02E, 0x49}, // Volume Down
	{0xE030, 0x48}, // Volume Up
	{0xE037, 0x69}, // Print Screen: F13, as on PC keyboards on a Mac
	{0x46, 0x6B},   // Scroll Lock: F14
}

var macKeyOf, scanOfMac = func() (map[uint16]uint16, map[uint16]uint16) {
	k, s := map[uint16]uint16{}, map[uint16]uint16{}
	for _, e := range macKeys {
		k[e[0]] = e[1]
		if _, ok := s[e[1]]; !ok {
			s[e[1]] = e[0]
		}
	}
	return k, s
}()

// macKeyCode is scan's macOS virtual key code.
func macKeyCode(scan uint16) (uint16, bool) {
	vk, ok := macKeyOf[scan]
	return vk, ok
}

// macScan is the scan code of a macOS virtual key code (the viewer's keys).
func macScan(vk uint16) (uint16, bool) {
	s, ok := scanOfMac[vk]
	return s, ok
}

// macModifier: the Quartz event flags a modifier key sets while held (the
// device-independent bit and the left/right one), 0 for other keys.
func macModifier(vk uint16) uint64 {
	const (
		shift, control, option, command = 0x20000, 0x40000, 0x80000, 0x100000
		// NX_DEVICE*KEYMASK: which of the two keys it is.
		lCtl, lShift, rShift, lCmd, rCmd, lAlt, rAlt, rCtl = 0x1, 0x2, 0x4, 0x8, 0x10, 0x20, 0x40, 0x2000
	)
	switch vk {
	case 0x38:
		return shift | lShift
	case 0x3C:
		return shift | rShift
	case 0x3B:
		return control | lCtl
	case 0x3E:
		return control | rCtl
	case 0x3A:
		return option | lAlt
	case 0x3D:
		return option | rAlt
	case 0x37:
		return command | lCmd
	case 0x36:
		return command | rCmd
	}
	return 0
}
