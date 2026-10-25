package tray

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// hiddenCommand runs a console program without flashing a console window.
func hiddenCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd
}

// OpenURL opens an http(s) URL in the default browser.
func OpenURL(u string) error {
	cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", u)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// OpenTerminal runs argv in a new terminal window: Windows Terminal (wt)
// when installed, else a console window.
func OpenTerminal(argv []string) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}
	if wt, err := exec.LookPath("wt.exe"); err == nil {
		return exec.Command(wt, append([]string{"-w", "new"}, argv...)...).Start() // a new Windows Terminal window
	}
	// a new console window of its own
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000010} // CREATE_NEW_CONSOLE
	return cmd.Start()
}
