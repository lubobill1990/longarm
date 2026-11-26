//go:build linux

package screen

import (
	"sync"
	"testing"
	"time"
)

// The viewer window on a virtual X server: it shows a picture scaled into
// the window, and maps a click and a key back to the worker's display.
func TestX11Viewer(t *testing.T) {
	xvfb(t)
	canView = true
	const pw, ph = 800, 600 // larger than 90% of the 640x480 screen: scaled
	pic := make([]byte, pw*ph*4)
	for y := 0; y < ph; y++ {
		for x := 0; x < pw; x++ {
			o := (y*pw + x) * 4
			switch {
			case y < ph/2 && x < pw/2:
				pic[o+2] = 255 // red
			case y < ph/2:
				pic[o+1] = 255 // green
			case x < pw/2:
				pic[o] = 255 // blue
			default:
				pic[o], pic[o+1], pic[o+2] = 255, 255, 255
			}
			pic[o+3] = 255
		}
	}
	packets := make(chan VideoPacket, 4)
	packets <- VideoPacket{Config: true, Width: pw, Height: ph, Codec: "jpeg"}
	packets <- VideoPacket{Cursor: &Cursor{Visible: true, ShapeID: 1, W: 2, H: 2, BGRA: make([]byte, 16)}}
	packets <- VideoPacket{Seq: 1, Key: true, Tiles: EncodeTiles(pic, pw, []Rect{{W: pw, H: ph}}, 90)}
	shown := make(chan uint64, 4)
	var mu sync.Mutex
	var input []InputEvent
	done := make(chan error, 1)
	go func() {
		done <- RunViewer(ViewerOptions{Title: "test", Packets: packets,
			Shown:  func(s uint64) { shown <- s },
			Input:  func(e InputEvent) { mu.Lock(); input = append(input, e); mu.Unlock() },
			Status: func() string { return "ok" }})
	}()
	select {
	case s := <-shown:
		if s != 1 {
			t.Fatalf("shown %d", s)
		}
	case err := <-done:
		t.Fatalf("RunViewer: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("nothing shown")
	}
	time.Sleep(200 * time.Millisecond) // the resize's redraw

	// The window is at 0,0 (no window manager), 576x432: 90% of the
	// screen at the picture's aspect.
	src, err := OpenSource(0)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	src.Next(time.Second)
	f := src.Frame()
	const ww, wh = 576, 432
	for _, p := range []struct {
		x, y    int
		b, g, r byte
	}{{ww / 4, wh / 4, 0, 0, 255}, {3 * ww / 4, wh / 4, 0, 255, 0}, {ww / 4, 3 * wh / 4, 255, 0, 0}, {3 * ww / 4, 3 * wh / 4, 255, 255, 255}} {
		o := (p.y*640 + p.x) * 4
		if d(f[o], p.b) > 40 || d(f[o+1], p.g) > 40 || d(f[o+2], p.r) > 40 {
			t.Errorf("at %d,%d: BGR %v, want %v", p.x, p.y, f[o:o+3], []byte{p.b, p.g, p.r})
		}
	}
	if o := (wh/4*640 + ww + 10) * 4; f[o] != 0 || f[o+1] != 0 || f[o+2] != 0 {
		t.Errorf("right of the window: %v (want the black root)", f[o:o+3])
	}

	in, err := NewInjector()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	in.Move(288, 216)
	in.Button(ButtonLeft, true)
	in.Button(ButtonLeft, false)
	in.Key(0x1E, true)
	in.Key(0x1E, false)
	deadline := time.Now().Add(5 * time.Second)
	var click, key bool
	for time.Now().Before(deadline) && !(click && key) {
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		for _, e := range input {
			if m := e.Mouse; m != nil && m.Button == ButtonLeft && m.Down {
				click = abs(m.X-400) <= 2 && abs(m.Y-300) <= 2
				if !click {
					t.Errorf("click at %d,%d, want 400,300", m.X, m.Y)
				}
			}
			if k := e.Key; k != nil && k.Scan == 0x1E && k.Down {
				key = true
			}
		}
		mu.Unlock()
	}
	if !click || !key {
		t.Errorf("input: click %v, key %v: %+v", click, key, input)
	}

	// H.264, when OpenH264 is here.
	if enc, err := NewH264Encoder(EncoderOptions{Width: pw, Height: ph, FPS: 30, Bitrate: 4_000_000}); err != nil {
		t.Logf("no H.264: %v", err)
	} else {
		defer enc.Close()
		au, err := enc.Encode(pic)
		if err != nil {
			t.Fatal(err)
		}
		packets <- VideoPacket{Config: true, Width: pw, Height: ph, Codec: "h264"}
		packets <- VideoPacket{Seq: 2, Key: true, Data: au}
		select {
		case s := <-shown:
			if s != 2 {
				t.Fatalf("shown %d", s)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no H.264 picture shown")
		}
		src.Next(time.Second)
		f := src.Frame()
		if o := (wh/4*640 + 3*ww/4) * 4; d(f[o], 0) > 40 || d(f[o+1], 255) > 40 || d(f[o+2], 0) > 40 {
			t.Errorf("H.264: green quadrant is %v", f[o:o+3])
		}
	}
	close(packets)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func d(a, b byte) int { return abs(int(a) - int(b)) }

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
