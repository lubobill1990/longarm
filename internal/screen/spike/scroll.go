//go:build windows

package main

import (
	"fmt"
	"log"
	"sort"
	"time"

	"longarm/internal/screen"
)

// scrollBench encodes a synthetic scroll of the current screen (each frame
// the image moves up by step rows) at fps for seconds, with H.264 and with
// full-frame JPEG (what the tile path sends when everything changes), so
// heavy motion can be measured without touching the desktop.
func scrollBench(o videoOptions, seconds, step, q int) {
	c, err := screen.Open(o.display)
	if err != nil {
		log.Fatal(err)
	}
	for d := []screen.Rect(nil); d == nil; {
		if d, err = c.Next(500); err != nil {
			log.Fatal(err)
		}
	}
	base := append([]byte(nil), c.Frame()...)
	W, H := c.W, c.H
	c.Close()
	w, h := W&^1, H&^1
	e, err := screen.NewEncoder(screen.EncoderOptions{Width: w, Height: h, FPS: o.fps, Bitrate: o.bitrate, Software: o.software})
	if err != nil {
		log.Fatal(err)
	}
	defer e.Close()
	n := seconds * o.fps
	fmt.Printf("scrolling %dx%d by %d rows/frame, %d frames at %d fps, %s encoder, %d kbit/s\n", w, h, step, n, o.fps, e.Name, o.bitrate/1000)
	frame := make([]byte, W*H*4)
	nv12 := make([]byte, w*h*3/2)
	var lat []time.Duration
	total := 0
	jpegTotal := 0
	interval := time.Second / time.Duration(o.fps)
	next := time.Now()
	for i := 0; i < n; i++ {
		off := (i * step) % H
		copy(frame, base[off*W*4:])
		copy(frame[(H-off)*W*4:], base[:off*W*4])
		t0 := time.Now()
		screen.BGRAToNV12(nv12, frame, w, h)
		au, err := e.Encode(nv12)
		if err != nil {
			log.Fatal(err)
		}
		lat = append(lat, time.Since(t0))
		total += len(au)
		if i%(o.fps/4) == 0 { // JPEG is slow: sample 4 frames a second
			jpegTotal += len(encode(frame, W*4, screen.Rect{W: W, H: H}, q)) * (o.fps / 4)
		}
		next = next.Add(interval)
		time.Sleep(time.Until(next))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	fmt.Printf("H.264: %.2f Mbit/s, NV12 + encode p50 %v p95 %v\n", float64(total)*8/1e6/float64(seconds), lat[len(lat)/2], lat[len(lat)*95/100])
	fmt.Printf("JPEG q%d full frames at the same rate: %.1f Mbit/s\n", q, float64(jpegTotal)*8/1e6/float64(seconds))
}
