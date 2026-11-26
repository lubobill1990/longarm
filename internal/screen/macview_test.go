package screen

import "testing"

func TestMacToRemote(t *testing.T) {
	for _, c := range []struct {
		name         string
		x, y, vw, vh float64
		fw, fh       int
		rx, ry       int
	}{
		{"same size", 10, 20, 1920, 1080, 1920, 1080, 10, 20},
		{"half size", 960, 540, 960, 540, 1920, 1080, 1919, 1079},
		{"centre, half size", 480, 270, 960, 540, 1920, 1080, 960, 540},
		// A 1000×1000 view of 2000×1000: bars of 250 above and below.
		{"wide picture, top bar", 500, 100, 1000, 1000, 2000, 1000, 1000, 0},
		{"wide picture, middle", 500, 500, 1000, 1000, 2000, 1000, 1000, 500},
		{"wide picture, bottom bar", 0, 990, 1000, 1000, 2000, 1000, 0, 999},
		// A 1000×500 view of 1000×1000: bars of 250 left and right.
		{"tall picture, left bar", 10, 250, 1000, 500, 1000, 1000, 0, 500},
		{"tall picture", 500, 250, 1000, 500, 1000, 1000, 500, 500},
	} {
		x, y, ok := macToRemote(c.x, c.y, c.vw, c.vh, c.fw, c.fh)
		if !ok || x != c.rx || y != c.ry {
			t.Errorf("%s: %v,%v → %d,%d (%v), want %d,%d", c.name, c.x, c.y, x, y, ok, c.rx, c.ry)
		}
	}
	if _, _, ok := macToRemote(1, 1, 100, 100, 0, 0); ok {
		t.Error("no picture yet: mapped")
	}
}
