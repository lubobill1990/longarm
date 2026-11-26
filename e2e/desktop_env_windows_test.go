//go:build windows

package e2e

import "testing"

// desktopTestEnv: Windows tests on the session's own desktop.
func desktopTestEnv(*testing.T) []string { return nil }
