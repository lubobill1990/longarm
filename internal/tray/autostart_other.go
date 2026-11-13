//go:build !windows && !darwin

package tray

import "os"

// newAutostart: an XDG autostart .desktop file ($XDG_CONFIG_HOME/autostart).
func newAutostart() autostarter {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	exe, _ := os.Executable()
	return xdgAutostart(dir, exe)
}
