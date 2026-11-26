package screen

import "testing"

func TestBGRAToNV12(t *testing.T) {
	const w, h = 8, 6
	src := make([]byte, w*h*4)
	for i := 0; i < len(src); i += 4 {
		if (i/4)%w < w/2 { // left half white, right half black
			src[i], src[i+1], src[i+2], src[i+3] = 255, 255, 255, 255
		}
	}
	dst := make([]byte, w*h*3/2)
	BGRAToNV12(dst, src, w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			want := byte(16)
			if x < w/2 {
				want = 235
			}
			if got := dst[y*w+x]; got != want {
				t.Fatalf("Y(%d,%d) = %d, want %d", x, y, got, want)
			}
		}
	}
	for i, c := range dst[w*h:] {
		if c < 127 || c > 129 {
			t.Fatalf("chroma[%d] = %d, want 128 (grey)", i, c)
		}
	}
}

func BenchmarkBGRAToNV12_1440p(b *testing.B) {
	const w, h = 2560, 1440
	src := make([]byte, w*h*4)
	dst := make([]byte, w*h*3/2)
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		BGRAToNV12(dst, src, w, h)
	}
}

func TestNV12RoundTrip(t *testing.T) {
	const w, h = 16, 8
	src := make([]byte, w*h*4)
	colours := [][3]byte{{255, 0, 0}, {0, 255, 0}, {0, 0, 255}, {200, 200, 200}} // BGR
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := colours[(y/(h/2))*2+x/(w/2)]
			o := (y*w + x) * 4
			src[o], src[o+1], src[o+2], src[o+3] = c[0], c[1], c[2], 255
		}
	}
	nv12 := make([]byte, w*h*3/2)
	BGRAToNV12(nv12, src, w, h)
	back := make([]byte, w*h*4)
	NV12ToBGRA(back, nv12, w, h, w, h)
	for i := 0; i < len(src); i += 4 {
		for c := 0; c < 3; c++ {
			if d := int(src[i+c]) - int(back[i+c]); d > 6 || d < -6 {
				t.Fatalf("pixel %d channel %d: %d -> %d", i/4, c, src[i+c], back[i+c])
			}
		}
	}
}
