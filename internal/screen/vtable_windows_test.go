//go:build windows

package screen

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// TestVtableSlots: every vtable slot a file names fits comObj's vtable
// (a slot past it panics - ClearState, 110, did on the viewer's resize).
func TestVtableSlots(t *testing.T) {
	files, _ := filepath.Glob("*_windows.go")
	re := regexp.MustCompile(`(?m)^\s*(slot\w+)\s*=\s*(\d+)`)
	n := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			n++
			if v, _ := strconv.Atoi(m[2]); v >= vtableSlots {
				t.Errorf("%s: %s = %d, past the %d slots of comObj's vtable", f, m[1], v, vtableSlots)
			}
		}
	}
	if n < 20 {
		t.Fatalf("found only %d slot constants: the pattern no longer matches", n)
	}
}
