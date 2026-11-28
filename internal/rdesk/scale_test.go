package rdesk

import "testing"

func TestScaler(t *testing.T) {
	// 4x2 BGRA: left half black, right half white; at 2x1 each output pixel
	// is the average of its 2x2 block
	src := make([]byte, 4*2*4)
	for y := 0; y < 2; y++ {
		for x := 2; x < 4; x++ {
			copy(src[(y*4+x)*4:], []byte{255, 255, 255, 255})
		}
	}
	out := newScaler(4, 2, 2, 1).scale(src)
	if out[0] != 0 || out[4] != 255 {
		t.Fatalf("2x: %v", out)
	}
	// a flat colour stays that colour at any size
	flat := make([]byte, 1920*1080*4)
	for i := range flat {
		flat[i] = byte(10 + i%4*50)
	}
	out = newScaler(1920, 1080, 1280, 720).scale(flat)
	for i, b := range out {
		if b != byte(10+i%4*50) {
			t.Fatalf("byte %d: %d", i, b)
		}
	}
}

func BenchmarkScaler1080to720(b *testing.B) {
	src := make([]byte, 1920*1080*4)
	s := newScaler(1920, 1080, 1280, 720)
	for b.Loop() {
		s.scale(src)
	}
}
