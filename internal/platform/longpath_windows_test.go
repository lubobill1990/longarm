//go:build windows

package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestLongPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a long directory name")
	os.MkdirAll(dir, 0o755)
	exe := filepath.Join(dir, "program.exe")
	os.WriteFile(exe, nil, 0o644)
	if l, err := filepath.EvalSymlinks(exe); err == nil {
		exe = l // TEMP itself may be an 8.3 path
	}
	if got := longPath(exe); !strings.EqualFold(got, exe) {
		t.Fatalf("longPath(%s) = %s", exe, got)
	}
	u, _ := windows.UTF16PtrFromString(exe)
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetShortPathName(u, &buf[0], uint32(len(buf)))
	short := windows.UTF16ToString(buf[:n])
	if err != nil || strings.EqualFold(short, exe) {
		t.Skip("no 8.3 names on this volume")
	}
	if got := longPath(short); !strings.EqualFold(got, exe) {
		t.Fatalf("longPath(%s) = %s, want %s", short, got, exe)
	}
	if !sameFile(short, exe) {
		t.Fatal("sameFile(short, long) = false")
	}
	if got := longPath(`C:\does\not\exist.exe`); got != `C:\does\not\exist.exe` {
		t.Fatalf("missing path changed: %s", got)
	}
}
