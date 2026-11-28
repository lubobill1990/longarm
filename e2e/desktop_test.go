package e2e

import (
	"bufio"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRemoteDesktop: Longarm's own remote desktop (docs/remote-desktop.md)
// end to end on this machine's desktop. The helper shows a known picture
// (four coloured quadrants, a black square that turns yellow when
// clicked); longarm desktop --screenshot must see it through H.264 and
// through JPEG tiles, its --input must reach the helper's window, and only
// the worker's owner may connect (audited).
func TestRemoteDesktop(t *testing.T) {
	// The OS's desktop for the test (desktop_env_*_test.go): Linux starts
	// Xvfb, for one; the environment goes to longarmd and the helper.
	denv := desktopTestEnv(t)
	e := newEnv(t)
	e.longarmdEnv = denv
	if err := os.MkdirAll(e.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dataDir, "longarmd.toml"), []byte("[access]\nowners = [\"alice\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.startLongarmd()
	info := e.mustLongarm(e.client, "info", "w1")
	if !strings.Contains(info, `"desktop-native"`) {
		// A machine without an interactive desktop (a service session):
		// nothing to show. CI's Windows runners have one.
		t.Skipf("this machine's desktop can't be shown:\n%s", info)
	}

	logPath := filepath.Join(e.root, "pattern.log")
	helper := exec.Command(bins.helper, "pattern", logPath, "180")
	helper.Env = append(os.Environ(), denv...)
	out, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { helper.Process.Kill(); helper.Wait() })
	var w, h int
	ready := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if _, err := fmt.Sscanf(sc.Text(), "ready %d %d", &w, &h); err == nil {
				ready <- nil
				return
			}
		}
		ready <- fmt.Errorf("the pattern helper ended: %v", sc.Err())
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the pattern helper did not show its window")
	}

	for _, codec := range []string{"h264", "jpeg"} {
		shot := filepath.Join(e.root, "shot-"+codec+".png")
		r := e.longarm(e.client, "desktop", "w1", "--native", "--codec", codec, "--screenshot", shot)
		if r.code != 0 {
			t.Fatalf("%s screenshot: exit %d\n%s%s", codec, r.code, r.stdout, r.stderr)
		}
		checkPattern(t, codec, shot, w, h, false)
	}

	// Input: a click on the square (it turns yellow) and typed text.
	r := e.longarm(e.client, "desktop", "w1", "--native", "--screenshot", filepath.Join(e.root, "after-input.png"),
		"--input", fmt.Sprintf("click:%d,%d;text:Hi;key:0x1E", w/2, h/2))
	if r.code != 0 {
		b, _ := os.ReadFile(logPath)
		t.Fatalf("input: exit %d\n%s%s\nthe helper's log:\n%s", r.code, r.stdout, r.stderr, b)
	}
	waitFor(t, "the helper to log the click and the keys", 15*time.Second, func() bool {
		b, _ := os.ReadFile(logPath)
		s := string(b)
		return strings.Contains(s, "click ") && strings.Contains(s, "char H") && strings.Contains(s, "char i") && strings.Contains(s, "char a")
	})
	b, _ := os.ReadFile(logPath)
	var cx, cy int
	if _, err := fmt.Sscanf(strings.SplitN(string(b[strings.Index(string(b), "click "):]), "\n", 2)[0], "click %d %d", &cx, &cy); err != nil {
		t.Fatalf("helper log: %v\n%s", err, b)
	}
	if abs(cx-w/2) > 3 || abs(cy-h/2) > 3 {
		t.Errorf("the click landed at %d,%d, sent to %d,%d", cx, cy, w/2, h/2)
	}
	checkPattern(t, "after input", filepath.Join(e.root, "after-input.png"), w, h, true)

	// A lower quality (internal/rdesk/quality): a smaller picture of the
	// same screen, and input in the picture's pixels lands where it shows.
	const lines = 540 // "minimal"
	if h > lines {
		pw, ph := (w*lines/h)&^1, lines
		shot := filepath.Join(e.root, "minimal.png")
		r := e.longarm(e.client, "desktop", "w1", "--native", "--quality", "minimal", "--screenshot", shot,
			"--input", fmt.Sprintf("click:%d,%d", pw/2, ph/2))
		if r.code != 0 {
			t.Fatalf("--quality minimal: exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
		f, err := os.Open(shot)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(f)
		f.Close()
		if err != nil || cfg.Width != pw || cfg.Height != ph {
			t.Fatalf("--quality minimal: a %dx%d picture (%v), want %dx%d", cfg.Width, cfg.Height, err, pw, ph)
		}
		checkPattern(t, "minimal", shot, pw, ph, true)
		waitFor(t, "the helper to log the second click", 15*time.Second, func() bool {
			b, _ := os.ReadFile(logPath)
			return strings.Count(string(b), "click ") >= 2
		})
		b, _ := os.ReadFile(logPath)
		s := string(b)
		last := s[strings.LastIndex(s, "click "):]
		if _, err := fmt.Sscanf(strings.SplitN(last, "\n", 2)[0], "click %d %d", &cx, &cy); err != nil {
			t.Fatalf("helper log: %v\n%s", err, b)
		}
		if slack := w/pw + 3; abs(cx-w/2) > slack || abs(cy-h/2) > slack {
			t.Errorf("--quality minimal: the click landed at %d,%d, want about %d,%d", cx, cy, w/2, h/2)
		}
	}

	// Privacy mode (Windows): the worker's own screen goes black, the viewer
	// still sees the desktop - the curtain is not in the capture. Elsewhere
	// the worker answers that it is off.
	r = e.longarm(e.client, "desktop", "w1", "--native", "--privacy", "--screenshot", filepath.Join(e.root, "privacy.png"))
	if r.code != 0 {
		t.Fatalf("privacy: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	want := "privacy mode off"
	if runtime.GOOS == "windows" {
		want = "privacy mode on"
	}
	if !strings.Contains(r.stderr, want) {
		t.Fatalf("privacy: want %q from the worker:\n%s", want, r.stderr)
	}
	checkPattern(t, "privacy mode", filepath.Join(e.root, "privacy.png"), w, h, true)

	// Only the owner: an admin certificate of somebody else is refused.
	r = e.longarm(e.admin, "desktop", "w1", "--native", "--screenshot", filepath.Join(e.root, "root.png"))
	if r.code == 0 || !strings.Contains(r.stderr, "only the owner") {
		t.Fatalf("root (an admin, not the owner): exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	audit, _ := os.ReadFile(filepath.Join(e.dataDir, "audit.log"))
	for _, want := range []string{"/longarm.v1.RemoteDesktop/Stream", `"denied":"not the owner"`, `"who":"client/alice"`} {
		if !strings.Contains(string(audit), want) {
			t.Errorf("audit.log misses %s:\n%s", want, audit)
		}
	}
}

// checkPattern looks at a screenshot of the helper's picture: the four
// quadrants and the square (yellow once clicked), with room for lossy
// encoding.
func checkPattern(t *testing.T, what, path string, w, h int, clicked bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	b := img.Bounds()
	if b.Dx() < w-1 || b.Dy() < h-1 {
		t.Fatalf("%s: a %dx%d picture of a %dx%d display", what, b.Dx(), b.Dy(), w, h)
	}
	square := [3]int{0, 0, 0}
	if clicked {
		square = [3]int{255, 255, 0}
	}
	for _, p := range []struct {
		name string
		x, y int
		rgb  [3]int
	}{
		{"top left (red)", w / 4, h / 4, [3]int{255, 0, 0}},
		{"top right (green)", 3 * w / 4, h / 4, [3]int{0, 255, 0}},
		{"bottom left (blue)", w / 4, 3 * h / 4, [3]int{0, 0, 255}},
		{"bottom right (white)", 3 * w / 4, 3 * h / 4, [3]int{255, 255, 255}},
		{"the square", w / 2, h / 2, square},
	} {
		got := rgbAt(img, image.Pt(b.Min.X+p.x, b.Min.Y+p.y))
		for c := 0; c < 3; c++ {
			if abs(got[c]-p.rgb[c]) > 48 {
				t.Errorf("%s: %s is %v, want about %v (%s)", what, p.name, got, p.rgb, path)
				break
			}
		}
	}
}

func rgbAt(img image.Image, p image.Point) [3]int {
	r, g, b, _ := img.At(p.X, p.Y).RGBA()
	return [3]int{int(r >> 8), int(g >> 8), int(b >> 8)}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
