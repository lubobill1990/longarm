//go:build darwin

package e2e

import "testing"

// desktopTestEnv: placeholder until the macOS backend.
func desktopTestEnv(t *testing.T) []string {
	t.Skip("the remote desktop has no macOS backend yet")
	return nil
}
