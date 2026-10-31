package steer

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Screen is a minimal virtual terminal: enough of VT100 / xterm to know
// what a TUI's screen shows (ConPTY redraws with absolute cursor moves and
// repaints only what changed, so the text of the output stream is not the
// screen). Colors, modes and the alternate screen are ignored.
type Screen struct {
	cols, rows int
	cells      [][]rune // rows x cols; 0 = blank, wide = the right half of a wide rune
	r, c       int
	savedR     int
	savedC     int
	pending    []byte // an escape sequence or rune cut at the end of a write
}

// wide marks the second cell of a double-width rune.
const wide = -1

// NewScreen returns a blank screen.
func NewScreen(cols, rows int) *Screen {
	s := &Screen{}
	s.Resize(cols, rows)
	return s
}

// Resize changes the size, keeping the top-left of what is shown.
func (s *Screen) Resize(cols, rows int) {
	cols, rows = max(cols, 1), max(rows, 1)
	cells := make([][]rune, rows)
	for i := range cells {
		cells[i] = make([]rune, cols)
		if i < len(s.cells) {
			copy(cells[i], s.cells[i])
		}
	}
	s.cells, s.cols, s.rows = cells, cols, rows
	s.r, s.c = min(s.r, rows-1), min(s.c, cols-1)
}

// Text is the screen: its rows (trailing blanks cut), blank rows at the end
// dropped.
func (s *Screen) Text() string {
	var lines []string
	for _, row := range s.cells {
		var b strings.Builder
		for _, r := range row {
			switch r {
			case 0:
				b.WriteByte(' ')
			case wide:
			default:
				b.WriteRune(r)
			}
		}
		lines = append(lines, strings.TrimRight(b.String(), " "))
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// Write interprets terminal output.
func (s *Screen) Write(p []byte) {
	if len(s.pending) > 0 {
		p = append(s.pending, p...)
		s.pending = nil
	}
	for i := 0; i < len(p); {
		c := p[i]
		switch {
		case c == 0x1b:
			n, ok := s.escape(p[i:])
			if !ok { // cut: wait for the rest
				s.pending = append([]byte(nil), p[i:]...)
				return
			}
			i += n
			continue
		case c == '\r':
			s.c = 0
		case c == '\n', c == 0x0b, c == 0x0c:
			s.lineFeed()
		case c == '\b':
			s.c = max(min(s.c, s.cols-1)-1, 0)
		case c == '\t':
			s.c = min((s.c/8+1)*8, s.cols-1)
		case c < 0x20 || c == 0x7f:
		default:
			if !utf8.FullRune(p[i:]) {
				s.pending = append([]byte(nil), p[i:]...)
				return
			}
			r, n := utf8.DecodeRune(p[i:])
			s.put(r)
			i += n
			continue
		}
		i++
	}
}

// put writes r at the cursor; past the last column (s.c == cols) the next
// rune wraps to the next line.
func (s *Screen) put(r rune) {
	w := 1
	if isWide(r) && s.cols > 1 {
		w = 2
	}
	if s.c+w > s.cols { // autowrap
		s.c = 0
		s.lineFeed()
	}
	s.cells[s.r][s.c] = r
	if w == 2 {
		s.cells[s.r][s.c+1] = wide
	}
	s.c += w
}

func (s *Screen) lineFeed() {
	if s.r == s.rows-1 {
		s.scrollUp(1)
		return
	}
	s.r++
}

func (s *Screen) scrollUp(n int) {
	n = min(n, s.rows)
	copy(s.cells, s.cells[n:])
	for i := s.rows - n; i < s.rows; i++ {
		s.cells[i] = make([]rune, s.cols)
	}
}

func (s *Screen) scrollDown(n int) {
	n = min(n, s.rows)
	copy(s.cells[n:], s.cells[:s.rows-n])
	for i := 0; i < n; i++ {
		s.cells[i] = make([]rune, s.cols)
	}
}

// escape interprets the escape sequence at p[0] (ESC): its length, false
// when p ends inside it.
func (s *Screen) escape(p []byte) (int, bool) {
	if len(p) < 2 {
		return 0, false
	}
	switch p[1] {
	case '[':
		for j := 2; j < len(p); j++ {
			if p[j] >= 0x40 && p[j] <= 0x7e {
				s.csi(string(p[2:j]), p[j])
				return j + 1, true
			}
		}
		return 0, false
	case ']', 'P', '_', '^', 'X': // up to BEL or ST
		for j := 2; j < len(p); j++ {
			if p[j] == 0x07 {
				return j + 1, true
			}
			if p[j] == 0x1b && j+1 < len(p) && p[j+1] == '\\' {
				return j + 2, true
			}
		}
		return 0, false
	case '(', ')', '*', '+', '#', '%':
		if len(p) < 3 {
			return 0, false
		}
		return 3, true
	case '7':
		s.savedR, s.savedC = s.r, s.c
	case '8':
		s.r, s.c = min(s.savedR, s.rows-1), min(s.savedC, s.cols-1)
	case 'M': // reverse index
		if s.r == 0 {
			s.scrollDown(1)
		} else {
			s.r--
		}
	case 'D':
		s.lineFeed()
	case 'E':
		s.c = 0
		s.lineFeed()
	case 'c': // reset
		s.erase(0, 0, s.rows-1, s.cols-1)
		s.r, s.c = 0, 0
	}
	return 2, true
}

// csi interprets ESC [ params final.
func (s *Screen) csi(params string, final byte) {
	if strings.HasPrefix(params, "?") || strings.HasPrefix(params, ">") || strings.HasPrefix(params, "=") || strings.HasPrefix(params, "<") {
		return // private modes, queries
	}
	var ns []int
	for _, f := range strings.Split(params, ";") {
		n, _ := strconv.Atoi(strings.TrimSpace(f))
		ns = append(ns, n)
	}
	arg := func(i, def int) int {
		if i < len(ns) && ns[i] > 0 {
			return ns[i]
		}
		return def
	}
	clampC := func(c int) int { return min(max(c, 0), s.cols-1) }
	clampR := func(r int) int { return min(max(r, 0), s.rows-1) }
	switch final {
	case 'H', 'f':
		s.r, s.c = clampR(arg(0, 1)-1), clampC(arg(1, 1)-1)
	case 'A':
		s.r = clampR(s.r - arg(0, 1))
	case 'B', 'e':
		s.r = clampR(s.r + arg(0, 1))
	case 'C', 'a':
		s.c = clampC(s.c + arg(0, 1))
	case 'D':
		s.c = clampC(s.c - arg(0, 1))
	case 'E':
		s.r, s.c = clampR(s.r+arg(0, 1)), 0
	case 'F':
		s.r, s.c = clampR(s.r-arg(0, 1)), 0
	case 'G', '`':
		s.c = clampC(arg(0, 1) - 1)
	case 'd':
		s.r = clampR(arg(0, 1) - 1)
	case 'J':
		s.c = clampC(s.c)
		switch arg(0, 0) {
		case 0:
			s.erase(s.r, s.c, s.rows-1, s.cols-1)
		case 1:
			s.erase(0, 0, s.r, s.c)
		case 2, 3:
			s.erase(0, 0, s.rows-1, s.cols-1)
		}
	case 'K':
		s.c = clampC(s.c)
		switch arg(0, 0) {
		case 0:
			s.erase(s.r, s.c, s.r, s.cols-1)
		case 1:
			s.erase(s.r, 0, s.r, s.c)
		case 2:
			s.erase(s.r, 0, s.r, s.cols-1)
		}
	case 'X':
		s.c = clampC(s.c)
		s.erase(s.r, s.c, s.r, clampC(s.c+arg(0, 1)-1))
	case 'P': // delete chars
		s.c = clampC(s.c)
		row := s.cells[s.r]
		n := min(arg(0, 1), s.cols-s.c)
		copy(row[s.c:], row[s.c+n:])
		clear(row[s.cols-n:])
	case '@': // insert blanks
		s.c = clampC(s.c)
		row := s.cells[s.r]
		n := min(arg(0, 1), s.cols-s.c)
		copy(row[s.c+n:], row[s.c:])
		clear(row[s.c : s.c+n])
	case 'L', 'M': // insert / delete lines (in the whole screen)
		n := min(arg(0, 1), s.rows-s.r)
		part := s.cells[s.r:]
		if final == 'L' {
			copy(part[n:], part[:len(part)-n])
			for i := 0; i < n; i++ {
				part[i] = make([]rune, s.cols)
			}
		} else {
			copy(part, part[n:])
			for i := len(part) - n; i < len(part); i++ {
				part[i] = make([]rune, s.cols)
			}
		}
	case 'S':
		s.scrollUp(arg(0, 1))
	case 'T':
		s.scrollDown(arg(0, 1))
	case 's':
		s.savedR, s.savedC = s.r, s.c
	case 'u':
		s.r, s.c = clampR(s.savedR), clampC(s.savedC)
	}
}

// erase blanks from (r0, c0) to (r1, c1) inclusive, row by row.
func (s *Screen) erase(r0, c0, r1, c1 int) {
	for r := r0; r <= r1 && r < s.rows; r++ {
		from, to := 0, s.cols-1
		if r == r0 {
			from = c0
		}
		if r == r1 {
			to = c1
		}
		for c := max(from, 0); c <= to && c < s.cols; c++ {
			s.cells[r][c] = 0
		}
	}
}

// isWide: runes two columns wide (East Asian wide / fullwidth, most emoji).
func isWide(r rune) bool {
	switch {
	case r < 0x1100:
		return false
	case r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf && r != 0x303f, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1f64f, r >= 0x1f900 && r <= 0x1faff,
		r >= 0x20000 && r <= 0x3fffd:
		return true
	}
	return false
}
