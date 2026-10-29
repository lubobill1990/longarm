package platform

import (
	"errors"
	"runtime"
	"strings"
	"unicode"
)

// ErrUnsupportedChars: a path or argument has a character no paste-ready
// command line can carry safely (control characters, line breaks).
var ErrUnsupportedChars = errors.New("path contains unsupported characters")

// CommandLine is exe and args as one line to paste into the shell people
// use here: PowerShell on Windows (the default terminal's shell), a POSIX
// shell elsewhere. Every command longarm prints or the tray copies for a
// person to run ("next: ...", "fix (elevated): ...") is built with it.
// These lines are pasted into elevated shells, and the paths in them come
// from the file system (any directory name): quoting must hold for every
// character, or a path becomes a command.
//
// PowerShell: anything beyond a plain path is a single-quoted literal,
// in which only a quote ends the string - and PowerShell takes U+2018,
// U+2019, U+201A and U+201B as single quotes too, so all five are doubled
// (and the double quotes U+201C-U+201E don't matter inside single quotes).
// A quoted executable needs the call operator: `"C:\Program Files\...\longarm.exe"
// firewall allow` is a ParserError; `& 'C:\Program Files\...\longarm.exe'
// firewall allow` runs it. (cmd.exe rejects that `&`; an executable that
// needs no quotes works in both.)
//
// ErrUnsupportedChars for control characters and line breaks (and, on
// Windows, an executable starting with -): no command is printed for them.
func CommandLine(exe string, args ...string) (string, error) {
	return commandLine(runtime.GOOS, exe, args...)
}

func commandLine(goos, exe string, args ...string) (string, error) {
	for _, s := range append([]string{exe}, args...) {
		if strings.IndexFunc(s, unsupportedRune) >= 0 {
			return "", ErrUnsupportedChars
		}
	}
	quote := posixQuote
	if goos == "windows" {
		quote = psQuote
	}
	parts := make([]string, 0, 1+len(args))
	if goos == "windows" && strings.HasPrefix(exe, "-") {
		// PowerShell's parser takes even a quoted leading '--%' as its
		// stop-parsing token; executables are paths, never "-..."
		return "", ErrUnsupportedChars
	}
	q := quote(exe)
	if goos == "windows" && q != exe {
		q = "& " + q
	}
	parts = append(parts, q)
	for _, a := range args {
		parts = append(parts, quote(a))
	}
	return strings.Join(parts, " "), nil
}

// unsupportedRune: control characters (newlines included) and the Unicode
// line and paragraph separators.
func unsupportedRune(r rune) bool {
	return unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || r == '\u0085'
}

// psBare: a token PowerShell reads as a literal word without quotes. A
// short ASCII allow-list: path characters, a leading - (parameters) and
// nothing PowerShell gives a meaning to (no $ ` ' " , ; & | ( ) { } @ #, no
// Unicode quotes or dashes).
func psBare(s string) bool {
	if s == "" || strings.HasPrefix(s, "--%") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune(`_-.:\/`, r):
		default:
			return false
		}
	}
	return true
}

// psSingleQuotes: the characters PowerShell ends a single-quoted string
// with (doubled to escape).
const psSingleQuotes = "'\u2018\u2019\u201A\u201B"

// psQuote returns s as a PowerShell word: bare when psBare, else a
// single-quoted literal with every single-quote character doubled.
func psQuote(s string) string {
	if psBare(s) {
		return s
	}
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		if strings.ContainsRune(psSingleQuotes, r) {
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// posixQuote quotes s for a POSIX shell when needed (single quotes).
func posixQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
