//go:build windows

package e2e

import (
	"bufio"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"longarm/internal/desktopsvc"
	"longarm/internal/platform"
)

// TestDesktopService: the Longarm Desktop service (internal/desktopsvc)
// lets longarm desktop see and drive the secure desktop: Ctrl+Alt+Del
// shows Winlogon's screen, Esc goes back. Opt-in, and it must run in the
// console session:
//
//   - LONGARM_SERVICE_TEST=1 from an elevated prompt: the test installs the
//     service (for this account) and removes it after;
//   - LONGARM_DESKTOP_SERVICE=installed: an administrator installed it
//     already with this account as a client (longarmd desktop-service
//     install --client <account>; the binaries must be this build's).
func TestDesktopService(t *testing.T) {
	switch {
	case os.Getenv("LONGARM_DESKTOP_SERVICE") == "installed":
	case os.Getenv("LONGARM_SERVICE_TEST") != "" && platform.Elevated():
		if b, err := exec.Command(bins.longarmd, "desktop-service", "install").CombinedOutput(); err != nil {
			t.Fatalf("install: %v\n%s", err, b)
		}
		t.Cleanup(func() {
			if b, err := exec.Command(bins.longarmd, "desktop-service", "uninstall").CombinedOutput(); err != nil {
				t.Errorf("uninstall: %v\n%s", err, b)
			}
		})
	default:
		t.Skip("set LONGARM_SERVICE_TEST=1 (elevated) or LONGARM_DESKTOP_SERVICE=installed, in the console session")
	}
	waitFor(t, "the desktop agent", 30*time.Second, func() bool {
		b, _ := exec.Command(bins.longarmd, "desktop-service", "status").CombinedOutput()
		return strings.Contains(string(b), "agent: answers")
	})

	e := newEnv(t)
	if err := os.MkdirAll(e.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dataDir, "longarmd.toml"), []byte("[access]\nowners = [\"alice\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.startLongarmd()
	if info := e.mustLongarm(e.client, "info", "w1"); !strings.Contains(info, `"desktop-native"`) {
		t.Fatalf("no desktop-native with the service:\n%s", info)
	}

	logPath := filepath.Join(e.root, "pattern.log")
	helper := exec.Command(bins.helper, "pattern", logPath, "180")
	out, _ := helper.StdoutPipe()
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { helper.Process.Kill(); helper.Wait() })
	var w, h int
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if _, err := fmt.Sscanf(sc.Text(), "ready %d %d", &w, &h); err == nil {
			break
		}
	}
	if w == 0 {
		t.Fatal("the pattern helper did not show its window")
	}
	shot := func(name, input string) string {
		t.Helper()
		p := filepath.Join(e.root, name+".png")
		args := []string{"desktop", "w1", "--native", "--screenshot", p}
		if input != "" {
			args = append(args, "--input", input)
		}
		if r := e.longarm(e.client, args...); r.code != 0 {
			t.Fatalf("%s: exit %d\n%s%s", name, r.code, r.stdout, r.stderr)
		}
		return p
	}
	checkPattern(t, "through the agent", shot("agent", ""), w, h, false)

	// Ctrl+Alt+Del: Winlogon's screen (dark, no pattern) - only SYSTEM sees it.
	p := shot("sas", "sas")
	if mean := brightness(t, p); mean > 90 {
		t.Errorf("after Ctrl+Alt+Del the screen is not Winlogon's (mean brightness %d): %s", mean, p)
	}
	// Esc there (input on the secure desktop) goes back to the desktop.
	checkPattern(t, "after Esc on Winlogon's screen", shot("back", "key:0x01"), w, h, false)
	if _, err := os.Stat(filepath.Join(os.Getenv("ProgramData"), "Longarm", "desktop", "agent.log")); err != nil && desktopsvc.Installed() {
		t.Logf("no agent log: %v", err)
	}
}

// brightness is a picture's mean luma (0-255).
func brightness(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	var sum, n int
	for y := b.Min.Y; y < b.Max.Y; y += 8 {
		for x := b.Min.X; x < b.Max.X; x += 8 {
			c := rgbAt(img, image.Pt(x, y))
			sum += (c[0]*299 + c[1]*587 + c[2]*114) / 1000
			n++
		}
	}
	return sum / max(n, 1)
}
