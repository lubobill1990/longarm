package tray

import "os"

// newAutostart: a LaunchAgent in ~/Library/LaunchAgents.
func newAutostart() autostarter {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	exe, _ := os.Executable()
	return launchAgent(home, exe)
}
