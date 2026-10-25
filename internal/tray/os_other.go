//go:build !windows && !darwin

package tray

import (
	"context"
	"errors"
	"os/exec"
)

func hiddenCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// OpenURL opens an http(s) URL in the default browser.
func OpenURL(u string) error { return exec.Command("xdg-open", u).Start() }

// OpenTerminal runs argv in a new terminal emulator window.
func OpenTerminal(argv []string) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}
	for _, t := range [][]string{{"x-terminal-emulator", "-e"}, {"gnome-terminal", "--"}, {"konsole", "-e"}, {"xfce4-terminal", "-x"}, {"xterm", "-e"}} {
		if p, err := exec.LookPath(t[0]); err == nil {
			return exec.Command(p, append(t[1:], argv...)...).Start()
		}
	}
	return errors.New("no terminal emulator found (x-terminal-emulator, gnome-terminal, konsole, xterm)")
}
