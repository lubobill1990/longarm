package tray

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

func hiddenCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// OpenURL opens an http(s) URL in the default browser.
func OpenURL(u string) error { return exec.Command("open", u).Start() }

// OpenTerminal runs argv in a new Terminal.app window.
func OpenTerminal(argv []string) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	line := strings.Join(q, " ")
	script := `tell application "Terminal" to do script "` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(line) + `"` + "\n" +
		`tell application "Terminal" to activate`
	return exec.Command("osascript", "-e", script).Start()
}

// RunElevated: Windows only (the admin worker).
func RunElevated(string, ...string) error {
	return errors.New("elevated actions are Windows-only")
}
