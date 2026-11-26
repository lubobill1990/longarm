//go:build windows

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"longarm/internal/screen"
)

// videoOptions: the H.264 path's flags.
type videoOptions struct {
	display, fps, bitrate int
	software              bool
}

// nalUnits splits an Annex B stream into NAL units (without start codes).
func nalUnits(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				end := i
				if end > start && b[end-1] == 0 {
					end--
				}
				out = append(out, b[start:end])
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// avcInfo says whether an access unit holds an IDR picture and, from its
// SPS, the WebCodecs codec string (avc1.PPCCLL).
func avcInfo(au []byte) (key bool, codec string) {
	for _, n := range nalUnits(au) {
		if len(n) == 0 {
			continue
		}
		switch n[0] & 0x1F {
		case 5:
			key = true
		case 7:
			if len(n) >= 4 {
				codec = fmt.Sprintf("avc1.%02X%02X%02X", n[1], n[2], n[3])
			}
		}
	}
	return key, codec
}

// videoSession captures a display and encodes it to H.264 until ctx ends;
// emit gets every access unit. Frames come when the screen changes (at
// most fps per second); once it has been still for a moment the last image
// is encoded twice more so the encoder can sharpen it.
func videoSession(ctx context.Context, o videoOptions, started func(c *screen.Capturer, e *screen.Encoder), emit func(au []byte, key bool, codec string, captured time.Time, encodeTime time.Duration) error) error {
	c, err := screen.Open(o.display)
	if err != nil {
		return err
	}
	defer c.Close()
	w, h := c.W&^1, c.H&^1
	e, err := screen.NewEncoder(screen.EncoderOptions{Width: w, Height: h, FPS: o.fps, Bitrate: o.bitrate, Software: o.software})
	if err != nil {
		return err
	}
	defer e.Close()
	started(c, e)
	nv12 := make([]byte, w*h*3/2)
	interval := time.Second / time.Duration(o.fps)
	var last time.Time
	refresh := 0
	have := false
	for ctx.Err() == nil {
		if s := time.Since(last); s < interval {
			time.Sleep(interval - s) // pace before capturing, so the frame is fresh
		}
		wait := 100 * time.Millisecond
		if refresh > 0 {
			wait = 150 * time.Millisecond
		}
		d, err := c.Next(int(wait / time.Millisecond))
		if err != nil {
			return err
		}
		switch {
		case d != nil:
			have, refresh = true, 2
		case have && refresh > 0:
			refresh--
		default:
			continue
		}
		t0 := time.Now() // captured
		if c.W == w {
			screen.BGRAToNV12(nv12, c.Frame(), w, h)
		} else {
			screen.BGRAToNV12(nv12, cropEven(c.Frame(), c.W, w, h), w, h)
		}
		au, err := e.Encode(nv12)
		if err != nil {
			return err
		}
		last = time.Now()
		if len(au) == 0 {
			continue
		}
		key, codec := avcInfo(au)
		if err := emit(au, key, codec, t0, time.Since(t0)); err != nil {
			return err
		}
	}
	return nil
}

func cropEven(frame []byte, stride4, w, h int) []byte {
	out := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		copy(out[y*w*4:(y+1)*w*4], frame[y*stride4*4:])
	}
	return out
}

func videoBench(o videoOptions, seconds int) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	defer cancel()
	var lat []time.Duration
	var total, keys, frames, firstKey int
	var codecStr string
	err := videoSession(ctx, o, func(c *screen.Capturer, e *screen.Encoder) {
		fmt.Printf("display %d: %dx%d, %s encoder, %d fps max, %d kbit/s\n", o.display, c.W, c.H, e.Name, o.fps, o.bitrate/1000)
		fmt.Printf("capturing for %ds - move windows / scroll / play a video to make changes...\n", seconds)
	}, func(au []byte, key bool, codec string, _ time.Time, t time.Duration) error {
		frames++
		total += len(au)
		if key {
			keys++
			if firstKey == 0 {
				firstKey = len(au)
			}
		}
		if codec != "" {
			codecStr = codec
		}
		lat = append(lat, t)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	fmt.Printf("%s: %d frames (%.1f/s), %d keyframes (first %d KB), %.1f KB/s = %.2f Mbit/s\n",
		codecStr, frames, float64(frames)/float64(seconds), keys, firstKey/1024,
		float64(total)/1024/float64(seconds), float64(total)*8/1e6/float64(seconds))
	if len(lat) > 0 {
		fmt.Printf("NV12 + encode per frame: p50 %v p95 %v max %v\n", lat[len(lat)/2], lat[len(lat)*95/100], lat[len(lat)-1])
	}
}

// videoStream is serve's H.264 path: 'C' + JSON {w,h,codec} once the SPS
// is known, then 'H' + flags (1: key) + capture time (Unix ns, int64 BE)
// + Annex B per access unit.
func videoStream(ctx context.Context, o videoOptions, out chan<- [][]byte, setOrigin func(x, y int)) {
	for ctx.Err() == nil {
		var w, h int
		sentConfig := ""
		err := videoSession(ctx, o, func(c *screen.Capturer, e *screen.Encoder) {
			setOrigin(c.Display.X, c.Display.Y)
			w, h = c.W&^1, c.H&^1
			log.Printf("streaming %dx%d with the %s encoder", w, h, e.Name)
		}, func(au []byte, key bool, codec string, captured time.Time, _ time.Duration) error {
			var batch [][]byte
			if codec != "" && codec != sentConfig {
				cfg, _ := json.Marshal(map[string]any{"w": w, "h": h, "codec": codec})
				batch = append(batch, append([]byte{'C'}, cfg...))
				sentConfig = codec
			}
			if sentConfig == "" {
				return nil // no SPS yet: nothing the viewer can decode
			}
			flags := byte(0)
			if key {
				flags = 1
			}
			m := make([]byte, 10, 10+len(au))
			m[0], m[1] = 'H', flags
			binary.BigEndian.PutUint64(m[2:], uint64(captured.UnixNano()))
			batch = append(batch, append(m, au...))
			select {
			case out <- batch:
			case <-ctx.Done():
			}
			return nil
		})
		if err != nil {
			log.Print(err)
			time.Sleep(500 * time.Millisecond) // desktop switch: open again
		}
	}
}
