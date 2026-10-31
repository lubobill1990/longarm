package steer

import (
	"regexp"
	"strings"
)

// Keys.
const (
	KeyEnter = "\r"
	KeyDown  = "\x1b[B"
	KeyUp    = "\x1b[A"
	KeyCtrlC = "\x03"
	KeyEsc   = "\x1b"
)

// A selection marker (Ink's ❯ / >, ›), an optional number, then the label.
func option(label string) *regexp.Regexp {
	return regexp.MustCompile(`([❯>›]\s*)?(?:\d+\.\s*)?` + regexp.QuoteMeta(label))
}

var (
	claudeTrustNo  = option("No, exit")
	claudeTrustYes = option("Yes, I trust this folder")
	copilotTrustQ  = "Do you trust the files in this folder?"
	copilotYes     = regexp.MustCompile(`([❯>›]\s*)?1\.\s*Yes\b`)
)

// selected reports whether the last match of re in s carries the marker
// (-1: no match).
func selected(re *regexp.Regexp, s string) (int, bool) {
	all := re.FindAllStringSubmatchIndex(s, -1)
	if len(all) == 0 {
		return -1, false
	}
	m := all[len(all)-1]
	return m[0], m[2] >= 0
}

// StartupAnswer is the keystroke for a start-up screen of agent (screen:
// what the terminal shows, Screen.Text), with what it answers; "" when
// there is nothing to answer.
func StartupAnswer(agent, screen string) (keys, what string) {
	switch agent {
	case Claude:
		noAt, noSel := selected(claudeTrustNo, screen)
		yesAt, yesSel := selected(claudeTrustYes, screen)
		switch {
		case yesAt >= 0 && yesSel && (!noSel || yesAt > noAt):
			return KeyEnter, "trust the folder"
		case noAt >= 0 && noSel:
			return KeyDown, "select: Yes, I trust this folder"
		case strings.Contains(screen, "Choose the text style"):
			return KeyEnter, "the default text style"
		case strings.Contains(screen, "Press Enter to continue"):
			return KeyEnter, "continue"
		}
	case Copilot:
		if i := strings.LastIndex(screen, copilotTrustQ); i >= 0 {
			if at, sel := selected(copilotYes, screen[i:]); at >= 0 && sel {
				return KeyEnter, "trust the folder"
			}
			return KeyUp, "select: 1. Yes"
		}
	}
	return "", ""
}
