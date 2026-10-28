//go:build !windows

package tray

// SystemLightTheme: not read outside Windows; the popup starts dark there
// (as before) unless Settings > Appearance picks Light.
func SystemLightTheme() bool { return false }
