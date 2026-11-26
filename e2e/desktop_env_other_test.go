//go:build !windows && !linux && !darwin

package e2e

import "testing"

func desktopTestEnv(t *testing.T) []string {
	t.Skip("no remote desktop on this OS")
	return nil
}
