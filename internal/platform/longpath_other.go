//go:build !windows

package platform

import "path/filepath"

// longPath is the absolute path (8.3 short names exist only on Windows).
func longPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
