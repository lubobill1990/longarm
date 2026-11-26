//go:build linux

package e2e

import "testing"

// desktopTestEnv: placeholder until the Linux backend (it starts Xvfb).
func desktopTestEnv(t *testing.T) []string {
	t.Skip("the remote desktop has no Linux backend yet")
	return nil
}
