//go:build !windows

package signin

// InstalledHub is the hub an installer recorded: only the Windows MSI
// does (Linux packages and install.sh sign in right away).
func InstalledHub() (hub, fingerprint string) { return "", "" }
