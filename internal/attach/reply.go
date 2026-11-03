package attach

import "bytes"

// IsReply reports whether p, an attached client's Input frame, consists
// only of well-formed terminal replies - answers its terminal (xterm.js,
// a ConPTY, a terminal emulator) sends by itself to queries of the
// session's program, not keystrokes:
//
//   - primary / secondary device attributes: ESC [ ? Ps;... c, ESC [ > Ps;... c
//   - device status: ESC [ 0 n
//   - cursor position: ESC [ row ; col R, ESC [ ? row ; col R
//   - colours: ESC ] 10|11|12 ; rgb:r/g/b or rgba:r/g/b/a, 1 to 4 hex
//     digits each (BEL or ESC \)
//   - focus in / out: ESC [ I, ESC [ O
//
// ESC [ 1 ; 2..16 R is also Shift/Ctrl/Alt-F3 on xterm-like terminals: it
// counts as a keystroke. Anything else in the frame (a key, a partial
// reply) makes the whole frame input.
func IsReply(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	for len(p) > 0 {
		n := replyLen(p)
		if n == 0 {
			return false
		}
		p = p[n:]
	}
	return true
}

// replyLen is the length of the terminal reply p starts with (0: none).
func replyLen(p []byte) int {
	if len(p) < 3 || p[0] != 0x1b {
		return 0
	}
	switch p[1] {
	case '[':
		return csiReplyLen(p)
	case ']':
		return oscReplyLen(p)
	}
	return 0
}

func csiReplyLen(p []byte) int {
	if p[2] == 'I' || p[2] == 'O' { // focus
		return 3
	}
	i, prefix := 2, byte(0)
	if p[i] == '?' || p[i] == '>' {
		prefix = p[i]
		i++
	}
	var params []int
	cur, digits := 0, 0
	for ; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= '0' && c <= '9':
			if digits++; digits > 6 {
				return 0
			}
			cur = cur*10 + int(c-'0')
		case c == ';':
			if digits == 0 {
				return 0
			}
			params, cur, digits = append(params, cur), 0, 0
		default:
			if digits == 0 {
				return 0
			}
			params = append(params, cur)
			switch {
			case c == 'c' && prefix != 0: // DA1 / DA2
				return i + 1
			case c == 'n' && prefix == 0 && len(params) == 1 && params[0] == 0: // DSR: OK
				return i + 1
			case c == 'R' && len(params) == 2 && params[0] > 0 && params[1] > 0 && prefix != '>': // CPR
				if prefix == 0 && params[0] == 1 && params[1] >= 2 && params[1] <= 16 {
					return 0 // also a modified F3
				}
				return i + 1
			}
			return 0
		}
	}
	return 0
}

func oscReplyLen(p []byte) int {
	if len(p) < 9 || p[2] != '1' || p[3] < '0' || p[3] > '2' || p[4] != ';' {
		return 0
	}
	rest, n := p[5:], 0 // n: the colour's components
	switch {
	case len(rest) > 4 && string(rest[:4]) == "rgb:":
		rest, n = rest[4:], 3
	case len(rest) > 5 && string(rest[:5]) == "rgba:":
		rest, n = rest[5:], 4
	default:
		return 0
	}
	end, term := bytes.IndexByte(rest, 0x07), 1
	if st := bytes.Index(rest, []byte("\x1b\\")); st >= 0 && (end < 0 || st < end) {
		end, term = st, 2
	}
	if end < 0 || !hexComponents(rest[:end], n) {
		return 0
	}
	return len(p) - len(rest) + end + term
}

// hexComponents: c is n components of 1 to 4 hex digits separated by '/'
// (an X11 rgb: colour).
func hexComponents(c []byte, n int) bool {
	parts := bytes.Split(c, []byte("/"))
	if len(parts) != n {
		return false
	}
	for _, h := range parts {
		if len(h) < 1 || len(h) > 4 {
			return false
		}
		for _, d := range h {
			if !(d >= '0' && d <= '9' || d >= 'a' && d <= 'f' || d >= 'A' && d <= 'F') {
				return false
			}
		}
	}
	return true
}
