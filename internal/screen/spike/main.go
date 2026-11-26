//go:build windows

// Command spike is the native remote desktop feasibility spike
// (docs/remote-desktop.md "Spike"): not shipped, not wired into longarmd.
//
//	go run ./internal/screen/spike bench [-display 0] [-seconds 5]          # JPEG tiles
//	go run ./internal/screen/spike h264bench [-software] [-bitrate 8000]  # H.264 on the live screen
//	go run ./internal/screen/spike scrollbench [-software] [-fps 60]      # H.264 vs JPEG, synthetic scroll
//	go run ./internal/screen/spike serve [-codec h264|jpeg] [-addr 127.0.0.1:7788]   # open it in a browser
//
// serve streams H.264 (WebCodecs) or changed rectangles as JPEG over a
// WebSocket to a canvas
// and injects the browser's mouse and keyboard with SendInput.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"

	"longarm/internal/screen"
)

//go:embed viewer.html
var viewerHTML []byte

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: spike bench|h264bench|serve|displays [flags]")
		os.Exit(2)
	}
	screen.SetDPIAware()
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	display := fs.Int("display", 0, "display index")
	seconds := fs.Int("seconds", 5, "bench: how long")
	quality := fs.Int("quality", 70, "JPEG quality")
	addr := fs.String("addr", "127.0.0.1:7788", "serve: listen address")
	codec := fs.String("codec", "h264", "serve: h264 or jpeg")
	fps := fs.Int("fps", 60, "h264: max frames per second")
	bitrate := fs.Int("bitrate", 8000, "h264: kbit/s")
	software := fs.Bool("software", false, "h264: the software encoder, not the GPU's")
	fs.Parse(os.Args[2:])
	switch os.Args[1] {
	case "displays":
		ds, err := screen.Displays()
		if err != nil {
			log.Fatal(err)
		}
		for _, d := range ds {
			fmt.Printf("%d %s %dx%d at %d,%d\n", d.Index, d.Name, d.W, d.H, d.X, d.Y)
		}
	case "bench":
		bench(*display, *seconds, *quality)
	case "scrollbench":
		scrollBench(videoOptions{*display, *fps, *bitrate * 1000, *software}, *seconds, 8, *quality)
	case "h264bench":
		videoBench(videoOptions{*display, *fps, *bitrate * 1000, *software}, *seconds)
	case "serve":
		serve(*addr, *display, *quality, *codec, videoOptions{*display, *fps, *bitrate * 1000, *software})
	default:
		log.Fatalf("unknown mode %q", os.Args[1])
	}
}

// toRGBA copies r of a BGRA frame into an RGBA image (the JPEG encoder's
// fast path).
func toRGBA(frame []byte, stride int, r screen.Rect) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, r.W, r.H))
	for y := 0; y < r.H; y++ {
		src := frame[(r.Y+y)*stride+r.X*4:]
		dst := img.Pix[y*img.Stride:]
		for x := 0; x < r.W*4; x += 4 {
			dst[x], dst[x+1], dst[x+2], dst[x+3] = src[x+2], src[x+1], src[x], 255
		}
	}
	return img
}

func encode(frame []byte, stride int, r screen.Rect, q int) []byte {
	var b bytes.Buffer
	jpeg.Encode(&b, toRGBA(frame, stride, r), &jpeg.Options{Quality: q})
	return b.Bytes()
}

func bench(display, seconds, q int) {
	c, err := screen.Open(display)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	fmt.Printf("display %d: %s %dx%d\n", display, c.Display.Name, c.W, c.H)
	// The first frame is the whole screen.
	var first []screen.Rect
	for first == nil {
		if first, err = c.Next(500); err != nil {
			log.Fatal(err)
		}
	}
	full := screen.Rect{W: c.W, H: c.H}
	t := time.Now()
	var conv time.Duration
	for i := 0; i < 5; i++ {
		t0 := time.Now()
		toRGBA(c.Frame(), c.W*4, full)
		conv += time.Since(t0)
	}
	var sizes []int
	t = time.Now()
	for i := 0; i < 5; i++ {
		sizes = append(sizes, len(encode(c.Frame(), c.W*4, full, q)))
	}
	enc := time.Since(t) / 5
	fmt.Printf("full frame: BGRA->RGBA %v, JPEG q%d %v (incl. conversion), %d KB\n", conv/5, q, enc, sizes[0]/1024)

	fmt.Printf("capturing for %ds - move windows / scroll to make changes...\n", seconds)
	end := time.Now().Add(time.Duration(seconds) * time.Second)
	var frames, timeouts, rects, px int
	var lat []time.Duration
	var bytesOut int
	for time.Now().Before(end) {
		t0 := time.Now()
		d, err := c.Next(100)
		if err != nil {
			log.Fatal(err)
		}
		if d == nil {
			timeouts++
			continue
		}
		frames++
		rects += len(d)
		for _, r := range d {
			px += r.W * r.H
			bytesOut += len(encode(c.Frame(), c.W*4, r, q))
		}
		lat = append(lat, time.Since(t0))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	fmt.Printf("%d changed frames (%.1f/s), %d idle timeouts, %d rects, %.1f%% of pixels/frame avg, %.1f KB/s JPEG\n",
		frames, float64(frames)/float64(seconds), timeouts, rects,
		100*float64(px)/float64(max(frames, 1))/float64(c.W*c.H), float64(bytesOut)/1024/float64(seconds))
	if len(lat) > 0 {
		fmt.Printf("acquire+copy+encode per frame: p50 %v p95 %v\n", lat[len(lat)/2], lat[len(lat)*95/100])
	}
}

type inputMsg struct {
	T      string `json:"t"`
	X, Y   int
	B      int
	D      bool
	DX, DY int
	C      uint16
}

func serve(addr string, display, q int, codec string, vo videoOptions) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(viewerHTML)
	})
	var one sync.Mutex // one viewer at a time
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if !one.TryLock() {
			http.Error(w, "busy", http.StatusConflict)
			return
		}
		defer one.Unlock()
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(1 << 16)
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		frames := make(chan [][]byte, 2)
		var origin struct{ x, y int }
		var originMu sync.Mutex
		go func() { // capture on its own locked thread
			defer close(frames)
			setOrigin := func(x, y int) { originMu.Lock(); origin.x, origin.y = x, y; originMu.Unlock() }
			if codec == "h264" {
				videoStream(ctx, vo, frames, setOrigin)
			} else {
				stream(ctx, display, q, frames, setOrigin)
			}
		}()
		go func() { // input
			defer cancel()
			for {
				_, b, err := ws.Read(ctx)
				if err != nil {
					return
				}
				var m inputMsg
				if json.Unmarshal(b, &m) != nil {
					continue
				}
				originMu.Lock()
				ox, oy := origin.x, origin.y
				originMu.Unlock()
				var ierr error
				switch m.T {
				case "m":
					ierr = screen.MouseMove(ox+m.X, oy+m.Y)
				case "b":
					ierr = screen.MouseButton(m.B, m.D)
				case "w":
					ierr = screen.MouseWheel(m.DX, m.DY)
				case "k":
					ierr = screen.Key(m.C, m.D)
				}
				if ierr != nil {
					log.Print(ierr)
				}
			}
		}()
		sent, start := 0, time.Now()
		for msgs := range frames {
			for _, m := range msgs {
				if err := ws.Write(ctx, websocket.MessageBinary, m); err != nil {
					return
				}
				sent += len(m)
			}
		}
		log.Printf("viewer gone: %.1f MB in %v", float64(sent)/1e6, time.Since(start).Round(time.Second))
		ws.Close(websocket.StatusNormalClosure, "")
	})
	log.Printf("open http://%s/", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// stream captures display and sends batches of messages: 'I' + JSON
// {w,h} when (re)opened, then 'T' + x,y,w,h (uint16 BE) + JPEG per changed
// rectangle. It reopens the capturer when duplication is lost.
func stream(ctx context.Context, display, q int, out chan<- [][]byte, setOrigin func(x, y int)) {
	for ctx.Err() == nil {
		c, err := screen.Open(display)
		if err != nil {
			log.Print(err)
			time.Sleep(time.Second)
			continue
		}
		setOrigin(c.Display.X, c.Display.Y)
		info, _ := json.Marshal(map[string]int{"w": c.W, "h": c.H})
		out <- [][]byte{append([]byte{'I'}, info...)}
		for ctx.Err() == nil {
			d, err := c.Next(200)
			if err != nil {
				log.Print(err)
				break
			}
			if d == nil {
				continue
			}
			batch := make([][]byte, 0, len(d))
			for _, r := range d {
				if r.W <= 0 || r.H <= 0 {
					continue
				}
				m := make([]byte, 9, 9+r.W*r.H/8)
				m[0] = 'T'
				binary.BigEndian.PutUint16(m[1:], uint16(r.X))
				binary.BigEndian.PutUint16(m[3:], uint16(r.Y))
				binary.BigEndian.PutUint16(m[5:], uint16(r.W))
				binary.BigEndian.PutUint16(m[7:], uint16(r.H))
				batch = append(batch, append(m, encode(c.Frame(), c.W*4, r, q)...))
			}
			select {
			case out <- batch:
			case <-ctx.Done():
			}
		}
		c.Close()
	}
}
