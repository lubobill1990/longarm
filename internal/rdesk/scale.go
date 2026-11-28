package rdesk

import (
	"runtime"
	"sync"
)

// scaler shrinks BGRA pictures (the quality's smaller sizes): bilinear,
// in fixed point, the rows split over the CPUs.
type scaler struct {
	sw, sh, dw, dh int
	xs             []int32 // per destination column: source column << 8 | weight
	ys             []int32
	out            []byte
}

func newScaler(sw, sh, dw, dh int) *scaler {
	s := &scaler{sw: sw, sh: sh, dw: dw, dh: dh, out: make([]byte, dw*dh*4)}
	s.xs = taps(sw, dw)
	s.ys = taps(sh, dh)
	return s
}

// taps maps each destination index to its first source index and the
// second's weight (0..255), sampling at the pixel centres.
func taps(src, dst int) []int32 {
	out := make([]int32, dst)
	for i := range out {
		// centre of destination pixel i in source pixels, << 8
		c := ((2*i+1)*src*256)/(2*dst) - 128
		c = max(c, 0)
		j, w := c>>8, c&0xFF
		if j >= src-1 {
			j, w = src-1, 0
		}
		out[i] = int32(j<<8 | w)
	}
	return out
}

// scale: src is sw x sh BGRA (stride sw*4); the result is dw x dh, owned
// by the scaler (the next call overwrites it).
func (s *scaler) scale(src []byte) []byte {
	n := min(runtime.NumCPU(), 8, s.dh)
	var wg sync.WaitGroup
	for k := 0; k < n; k++ {
		y0, y1 := s.dh*k/n, s.dh*(k+1)/n
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.rows(src, y0, y1)
		}()
	}
	wg.Wait()
	return s.out
}

func (s *scaler) rows(src []byte, y0, y1 int) {
	stride := s.sw * 4
	for y := y0; y < y1; y++ {
		ty := s.ys[y]
		sy, wy := int(ty>>8), int(ty&0xFF)
		sy2 := min(sy+1, s.sh-1)
		r1, r2 := src[sy*stride:], src[sy2*stride:]
		o := s.out[y*s.dw*4:]
		for x := 0; x < s.dw; x++ {
			tx := s.xs[x]
			sx, wx := int(tx>>8)*4, int(tx&0xFF)
			sx2 := min(sx+4, stride-4)
			for c := 0; c < 4; c++ {
				a := int(r1[sx+c])*(256-wx) + int(r1[sx2+c])*wx
				b := int(r2[sx+c])*(256-wx) + int(r2[sx2+c])*wx
				o[x*4+c] = byte((a*(256-wy) + b*wy) >> 16)
			}
		}
	}
}
