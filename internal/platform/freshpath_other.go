//go:build !windows

package platform

// FreshPath: this platform rebuilds PATH before a job starts (RefreshPath).
// Elsewhere longarmd's environment comes from its service unit.
const FreshPath = false

// RefreshPath is Windows-only.
func RefreshPath() {}

// NoteStartPath is Windows-only.
func NoteStartPath() {}
