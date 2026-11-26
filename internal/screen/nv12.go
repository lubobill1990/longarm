package screen

import (
	"runtime"
	"sync"
)

// BGRAToNV12 converts a BGRA image (stride w*4) to NV12 (BT.709, limited
// range: what H.264 decoders assume for HD video without colour info)
// into dst (w*h*3/2 bytes), in parallel strips. w and h must be even.
func BGRAToNV12(dst, src []byte, w, h int) {
	strips := min(runtime.GOMAXPROCS(0), h/2)
	rows := (h/2 + strips - 1) / strips // row pairs per strip
	var wg sync.WaitGroup
	for s := 0; s < strips; s++ {
		y0, y1 := s*rows*2, min((s+1)*rows*2, h)
		if y0 >= y1 {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			nv12Rows(dst, src, w, h, y0, y1)
		}()
	}
	wg.Wait()
}

// BT.709 limited range, 16.16 fixed point.
const (
	yR, yG, yB = 11966, 40254, 4064 // 0.1826, 0.6142, 0.0620 * 65536
	uR, uG, uB = -6596, -22189, 28784
	vR, vG, vB = 28784, -26145, -2639
)

func nv12Rows(dst, src []byte, w, h, y0, y1 int) {
	stride := w * 4
	uv := dst[w*h:]
	for y := y0; y < y1; y += 2 {
		r0 := src[y*stride : (y+1)*stride]
		r1 := src[(y+1)*stride : (y+2)*stride]
		d0 := dst[y*w : (y+1)*w]
		d1 := dst[(y+1)*w : (y+2)*w]
		c := uv[(y/2)*w : (y/2+1)*w]
		for x := 0; x < w; x += 2 {
			i := x * 4
			var sr, sg, sb int32
			for _, p := range [4][]byte{r0[i : i+4], r0[i+4 : i+8], r1[i : i+4], r1[i+4 : i+8]} {
				sr += int32(p[2])
				sg += int32(p[1])
				sb += int32(p[0])
			}
			d0[x] = luma(r0[i:])
			d0[x+1] = luma(r0[i+4:])
			d1[x] = luma(r1[i:])
			d1[x+1] = luma(r1[i+4:])
			// Average of the 2x2 block (sums are 4x).
			c[x] = clamp8(128 + (uR*sr+uG*sg+uB*sb)>>18)
			c[x+1] = clamp8(128 + (vR*sr+vG*sg+vB*sb)>>18)
		}
	}
}

func luma(p []byte) byte {
	return byte(16 + (yR*int32(p[2])+yG*int32(p[1])+yB*int32(p[0])+32768)>>16)
}

func clamp8(v int32) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// NV12ToBGRA converts NV12 (BT.709, limited range; Y rows of stride bytes,
// planeH rows, then the interleaved UV plane) to a w×h BGRA image (stride
// w*4) in dst, in parallel strips: the decoder's padding is cut off.
func NV12ToBGRA(dst, src []byte, stride, planeH, w, h int) {
	strips := min(runtime.GOMAXPROCS(0), max(h/2, 1))
	rows := (h/2 + strips - 1) / strips
	var wg sync.WaitGroup
	for s := 0; s < strips; s++ {
		y0, y1 := s*rows*2, min((s+1)*rows*2, h)
		if y0 >= y1 {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			bgraRows(dst, src, stride, planeH, w, y0, y1)
		}()
	}
	wg.Wait()
}

// BT.709 limited range to RGB, 16.16 fixed point.
const (
	cY, cRV, cGU, cGV, cBU = 76284, 117489, 13954, 34931, 138438
)

func bgraRows(dst, src []byte, stride, planeH, w, y0, y1 int) {
	uv := src[stride*planeH:]
	for y := y0; y < y1; y++ {
		yr := src[y*stride:]
		cr := uv[(y/2)*stride:]
		d := dst[y*w*4:]
		for x := 0; x < w; x++ {
			l := (int32(yr[x]) - 16) * cY
			u := int32(cr[x&^1]) - 128
			v := int32(cr[x|1]) - 128
			o := x * 4
			d[o] = clamp8((l + cBU*u + 32768) >> 16)
			d[o+1] = clamp8((l - cGU*u - cGV*v + 32768) >> 16)
			d[o+2] = clamp8((l + cRV*v + 32768) >> 16)
			d[o+3] = 255
		}
	}
}
