package sshd

import "testing"

func TestSyscallEscape(t *testing.T) {
	for in, want := range map[string]string{
		"":                   `""`,
		"plain":              "plain",
		"a b":                `"a b"`,
		`C:\Program Files\x`: `"C:\Program Files\x"`,
		`say "hi"`:           `"say \"hi\""`,
		`trail\ `:            `"trail\ "`,
		`end\`:               `end\`,
		`sp end\`:            `"sp end\\"`,
	} {
		if got := syscallEscape(in); got != want {
			t.Errorf("escape(%q) = %s, want %s", in, got, want)
		}
	}
}
