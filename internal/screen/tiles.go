package screen

import (
	"bytes"
	"image"
	"image/draw"
	"image/jpeg"
	"runtime"
	"sync"
)

// JPEG tiles: the codec where there is no H.264 (docs/remote-desktop.md
// "Encoding"). The worker sends the changed rectangles of a frame, the
// viewer draws them into its copy of the screen. Pure Go on both sides.

// TileSize is the grid changes are found and sent in.
const TileSize = 64

// ChangedTiles compares frame with prev (both BGRA w×h, stride w*4) on
// the tile grid and returns the changed tiles, merged into runs along each
// row; all of them when prev is nil. It copies frame into prev.
func ChangedTiles(frame, prev []byte, w, h int) []Rect {
	var out []Rect
	for ty := 0; ty < h; ty += TileSize {
		th := min(TileSize, h-ty)
		run := -1
		for tx := 0; tx <= w; tx += TileSize {
			changed := false
			if tx < w {
				tw := min(TileSize, w-tx)
				changed = prev == nil || tileDiffers(frame, prev, w, tx, ty, tw, th)
			}
			switch {
			case changed && run < 0:
				run = tx
			case !changed && run >= 0:
				out = append(out, Rect{run, ty, min(tx, w) - run, th})
				run = -1
			}
		}
	}
	if prev != nil {
		copy(prev, frame)
	}
	return out
}

func tileDiffers(a, b []byte, w, x, y, tw, th int) bool {
	for r := y; r < y+th; r++ {
		o := (r*w + x) * 4
		if !bytes.Equal(a[o:o+tw*4], b[o:o+tw*4]) {
			return true
		}
	}
	return false
}

// EncodeTiles encodes the rectangles of frame as JPEG (in parallel).
func EncodeTiles(frame []byte, w int, rects []Rect, quality int) []Tile {
	out := make([]Tile, len(rects))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i, r := range rects {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			img := image.NewRGBA(image.Rect(0, 0, r.W, r.H))
			for y := 0; y < r.H; y++ {
				src := frame[((r.Y+y)*w+r.X)*4:]
				dst := img.Pix[y*img.Stride:]
				for x := 0; x < r.W*4; x += 4 {
					dst[x], dst[x+1], dst[x+2], dst[x+3] = src[x+2], src[x+1], src[x], 255
				}
			}
			var b bytes.Buffer
			jpeg.Encode(&b, img, &jpeg.Options{Quality: quality})
			out[i] = Tile{X: r.X, Y: r.Y, W: r.W, H: r.H, JPEG: b.Bytes()}
		}()
	}
	wg.Wait()
	return out
}

// DrawTiles decodes JPEG tiles into a BGRA image (w×h, stride w*4).
func DrawTiles(img []byte, w, h int, tiles []Tile) {
	for _, t := range tiles {
		m, err := jpeg.Decode(bytes.NewReader(t.JPEG))
		if err != nil {
			continue
		}
		rgba := image.NewRGBA(m.Bounds())
		draw.Draw(rgba, rgba.Bounds(), m, m.Bounds().Min, draw.Src)
		for y := 0; y < t.H && y < rgba.Rect.Dy() && t.Y+y < h; y++ {
			for x := 0; x < t.W && x < rgba.Rect.Dx() && t.X+x < w; x++ {
				s := rgba.Pix[y*rgba.Stride+x*4:]
				d := img[((t.Y+y)*w+t.X+x)*4:]
				d[0], d[1], d[2], d[3] = s[2], s[1], s[0], 255
			}
		}
	}
}
