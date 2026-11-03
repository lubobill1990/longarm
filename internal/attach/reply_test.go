package attach

import "testing"

// Replies as terminals send them (recorded from xterm.js, Windows Terminal
// / ConPTY, iTerm2 and xterm), and keystrokes that look alike.
func TestIsReply(t *testing.T) {
	for _, s := range []string{
		"\x1b[?1;2c",                            // DA1, xterm.js
		"\x1b[?61;6;7;14;21;22;23;24;28;32;42c", // DA1, Windows Terminal
		"\x1b[?62;4c",                           // DA1, iTerm2
		"\x1b[>0;276;0c",                        // DA2, xterm.js
		"\x1b[>1;10;0c",                         // DA2, Windows Terminal
		"\x1b[0n",                               // DSR OK
		"\x1b[24;80R",                           // CPR
		"\x1b[1;1R",                             // CPR at the top left
		"\x1b[1;40R",                            // CPR, row 1 past column 16
		"\x1b[?5;12R",                           // DECXCPR
		"\x1b]11;rgb:1e1e/1e1e/1e1e\x1b\\",      // OSC 11, ST
		"\x1b]10;rgb:cccc/cccc/cccc\x07",        // OSC 10, BEL
		"\x1b]12;rgba:ffff/ffff/ffff/ffff\x07",
		"\x1b[I", "\x1b[O", // focus in / out
		// several in one read, as xterm.js answers Claude Code's start-up queries
		"\x1b]10;rgb:cccc/cccc/cccc\x1b\\\x1b]11;rgb:1e1e/1e1e/1e1e\x1b\\\x1b[?1;2c",
		"\x1b[I\x1b[12;1R",
	} {
		if !IsReply([]byte(s)) {
			t.Errorf("%q: not a reply", s)
		}
	}
	for _, s := range []string{
		"", "a", "\r", "\x1b", "\x1b[A", "\x1b[Z", "\x03",
		"\x1b[1;2R", "\x1b[1;5R", "\x1b[1;16R", // Shift / Ctrl / ... F3
		"\x1b[?1;2", "\x1b]11;rgb:1e1e/1e1e/1e1e", // partial
		"\x1b[?1;2cx", "x\x1b[I", "\x1b[Ia", // a reply and a key
		"\x1b[200~text\x1b[201~", "\x1b[c", "\x1b[5n", "\x1b[;5R", "\x1b[24R",
		"\x1b]11;?\x07", "\x1b]2;title\x07", "\x1b]11;rgb:zz/zz/zz\x07",
		"\x1b[1234567;1R",
		"\x1b]10;rgb:/\x07", "\x1b]11;rgb:deadbeef\x07", "\x1b]11;rgb:1e1e/1e1e\x07", // malformed colours
		"\x1b]11;rgb:1e1e/1e1e/1e1e/1e1e\x07", "\x1b]11;rgba:1/2/3\x07", "\x1b]11;rgb:12345/1/1\x07",
		"\x1b]11;rgb:1e//1e\x1b\\",
	} {
		if IsReply([]byte(s)) {
			t.Errorf("%q: a reply", s)
		}
	}
}
