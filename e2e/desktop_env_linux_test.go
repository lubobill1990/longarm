//go:build linux

package e2e

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// desktopTestEnv starts a virtual X server (Xvfb, on a display number it
// picks itself) for the test: a desktop without a window manager, so the
// helper's window covers the screen by itself.
func desktopTestEnv(t *testing.T) []string {
	xvfb, err := exec.LookPath("Xvfb")
	if err != nil {
		t.Skip("Xvfb is not installed (apt-get install xvfb)")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// -displayfd: Xvfb writes the display number once it accepts clients.
	cmd := exec.Command(xvfb, "-displayfd", "3", "-screen", "0", "1280x800x24", "-nolisten", "tcp", "-noreset")
	cmd.ExtraFiles = []*os.File{w}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	num := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(r).ReadString('\n')
		num <- strings.TrimSpace(line)
		r.Close()
	}()
	select {
	case n := <-num:
		if n == "" {
			t.Fatal("Xvfb did not start")
		}
		// XAUTHORITY: none (Xvfb takes everybody local); no Wayland
		// session around it either.
		return []string{"DISPLAY=:" + n, "XAUTHORITY=", "WAYLAND_DISPLAY=", "XDG_SESSION_TYPE=x11"}
	case <-time.After(30 * time.Second):
		t.Fatal("Xvfb did not start in 30 s")
	}
	return nil
}
