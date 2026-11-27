//go:build linux

package screen

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jezek/xgb/xproto"
)

// xvfb starts a virtual X server for the test and points $DISPLAY at it.
func xvfb(t *testing.T) {
	bin, err := exec.LookPath("Xvfb")
	if err != nil {
		t.Skip("no Xvfb")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// -noreset: Xvfb otherwise restarts when its last client goes, and the
	// next connection (Available's, then Displays') hits the restart.
	cmd := exec.Command(bin, "-displayfd", "3", "-screen", "0", "640x480x24", "-nolisten", "tcp", "-noreset")
	cmd.ExtraFiles = []*os.File{w}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	line, _ := bufio.NewReader(r).ReadString('\n')
	r.Close()
	if strings.TrimSpace(line) == "" {
		t.Fatal("Xvfb did not start")
	}
	t.Setenv("DISPLAY", ":"+strings.TrimSpace(line))
	t.Setenv("XAUTHORITY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("XDG_SESSION_TYPE", "")
}

func TestX11CaptureAndInput(t *testing.T) {
	xvfb(t)
	if r := Available(); r != "" {
		t.Fatalf("Available: %s", r)
	}
	ds, err := Displays()
	if err != nil || len(ds) == 0 || ds[0].W != 640 || ds[0].H != 480 {
		t.Fatalf("Displays: %+v, %v", ds, err)
	}
	src, err := OpenSource(0)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	xs := src.(*x11Source)
	t.Logf("MIT-SHM %v, DAMAGE %v, XFIXES %v", xs.seg != 0, xs.dmg != 0, xs.xfixes)
	if changed, err := src.Next(time.Second); err != nil || !changed {
		t.Fatalf("first Next: %v, %v", changed, err)
	}
	if len(src.Frame()) != 640*480*4 {
		t.Fatalf("frame of %d bytes", len(src.Frame()))
	}
	// Idle: no change (the pointer may report once).
	src.Next(50 * time.Millisecond)
	start := time.Now()
	if changed, err := src.Next(200 * time.Millisecond); err != nil || changed {
		t.Fatalf("idle Next: %v, %v", changed, err)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("idle Next returned after %v", d)
	}

	// Draw something: DAMAGE reports it.
	c, err := xConnect()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	scr := xproto.Setup(c).DefaultScreen(c)
	id, _ := c.NewId()
	win := xproto.Window(id)
	xproto.CreateWindow(c, scr.RootDepth, win, scr.Root, 100, 100, 50, 50, 0, xproto.WindowClassInputOutput, scr.RootVisual,
		xproto.CwBackPixel|xproto.CwOverrideRedirect|xproto.CwEventMask, []uint32{0xFF0000, 1, xproto.EventMaskKeyPress | xproto.EventMaskButtonPress})
	xproto.MapWindow(c, win)
	xproto.SetInputFocus(c, xproto.InputFocusPointerRoot, win, xproto.TimeCurrentTime)
	c.Sync()
	got := false
	for i := 0; i < 10 && !got; i++ {
		changed, err := src.Next(500 * time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		f := src.Frame()
		o := (120*640 + 120) * 4
		got = changed && f[o+2] == 0xFF && f[o+1] == 0 && f[o] == 0
	}
	if !got {
		t.Fatal("the red window was not captured")
	}

	// Input: the pointer moves, the window gets a click and keys.
	in, err := NewInjector()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := in.Move(110, 120); err != nil {
		t.Fatal(err)
	}
	moved := false
	for i := 0; i < 10 && !moved; i++ {
		src.Next(100 * time.Millisecond)
		cur := src.Cursor()
		moved = cur.X == 110 && cur.Y == 120
	}
	if !moved {
		t.Errorf("cursor at %+v, want 110,120", src.Cursor())
	}
	if err := in.Button(ButtonLeft, true); err != nil {
		t.Fatal(err)
	}
	in.Button(ButtonLeft, false)
	in.Key(0x1E, true) // a
	in.Key(0x1E, false)
	if err := in.Text("H€"); err != nil { // € is on no key of the default map
		t.Fatal(err)
	}
	if err := in.Wheel(0, -240); err != nil {
		t.Fatal(err)
	}
	var keys []xproto.Keycode
	buttons := 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (len(keys) < 4 || buttons < 3) {
		ev, _ := c.PollForEvent()
		switch e := ev.(type) {
		case xproto.KeyPressEvent:
			keys = append(keys, e.Detail)
		case xproto.ButtonPressEvent:
			buttons++
		case nil:
			time.Sleep(10 * time.Millisecond)
		}
	}
	// a, Shift, h, the spare keycode for €.
	if len(keys) < 4 || keys[0] != 30+8 || keys[1] != 42+8 || keys[2] != 35+8 {
		t.Errorf("keys %v", keys)
	}
	if buttons < 3 {
		t.Errorf("%d button presses (click and two wheel notches)", buttons)
	}
}
