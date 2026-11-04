package main

import "testing"

// TestIntoTarget: a copy into a directory of the target worker takes the
// source's name, whatever either side's OS.
func TestIntoTarget(t *testing.T) {
	for _, c := range []struct{ dir, src, want string }{
		{"in", "/home/a/out", "in/out"},
		{"in/", "/home/a/out", "in/out"},
		{`C:\in\`, `D:\data\out`, `C:\in/out`},
		{"", "/home/a/out", "out"},   // the home directory
		{"/", "/home/a/out", "/out"}, // the root
		{"/srv", `C:\Users\a\model.bin`, "/srv/model.bin"},
		{"in", `D:\`, "in/"}, // a drive: no name of its own
	} {
		if got := intoTarget(c.dir, c.src); got != c.want {
			t.Errorf("intoTarget(%q, %q) = %q, want %q", c.dir, c.src, got, c.want)
		}
	}
}
