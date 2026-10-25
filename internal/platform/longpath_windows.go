//go:build windows

package platform

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// longPath expands 8.3 short names (C:\Users\RUNNER~1\...) of an existing
// path: netsh can't resolve a program given by its short name, and firewall
// rules store long paths. Unchanged when the path doesn't exist.
func longPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetLongPathName(u, &buf[0], uint32(len(buf)))
		if err != nil || n == 0 {
			return p
		}
		if int(n) <= len(buf) {
			return windows.UTF16ToString(buf[:n])
		}
		buf = make([]uint16, n)
	}
}
