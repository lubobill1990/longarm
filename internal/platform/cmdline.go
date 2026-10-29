package platform

import (
	"runtime"
	"strings"
)

// CommandLine is exe and args as one line to paste into the shell people
// use here: PowerShell on Windows (the default terminal's shell), a POSIX
// shell elsewhere. Every command longarm prints or the tray copies for a
// person to run ("next: ...", "fix (elevated): ...") is built with it.
//
// PowerShell treats a quoted string at the start of a line as a value, not
// a command: `"C:\Program Files\Longarm\longarm.exe" firewall allow` is a
// ParserError. A quoted executable needs the call operator: `& "..." args`.
// (cmd.exe rejects that `&`; an executable that needs no quotes works in
// both.)
func CommandLine(exe string, args ...string) string {
	return commandLine(runtime.GOOS, exe, args...)
}

func commandLine(goos, exe string, args ...string) string {
	quote := posixQuote
	if goos == "windows" {
		quote = psQuote
	}
	parts := make([]string, 0, 1+len(args))
	q := quote(exe)
	if goos == "windows" && q != exe {
		q = "& " + q
	}
	parts = append(parts, q)
	for _, a := range args {
		parts = append(parts, quote(a))
	}
	return strings.Join(parts, " ")
}

// psQuote quotes s for PowerShell when needed: double quotes, or single
// quotes (literal) when s has characters double quotes would expand.
func psQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t'\"`$&|;,(){}<>@#") {
		return s
	}
	if !strings.ContainsAny(s, "\"`$") {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// posixQuote quotes s for a POSIX shell when needed (single quotes).
func posixQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
