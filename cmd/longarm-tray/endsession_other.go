//go:build !windows

package main

// watchEndSession: Windows only (WM_QUERYENDSESSION / WM_ENDSESSION,
// Restart Manager).
func watchEndSession(quit func()) func() { return func() {} }
