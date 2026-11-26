//go:build windows

package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"log"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"longarm/internal/screen"
)

// view is the native viewer: H.264 from serve's TCP port, decoded on the
// GPU and drawn into a window (internal/screen Presenter and Window).
// Latency is the capture time on the server to Present here: meaningful
// when both run on one machine (one clock).
func view(addr string, control bool, seconds int, snapPath string) {
	started := time.Now()
	snapArmed := snapPath != ""
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	conn.(*net.TCPConn).SetNoDelay(true)
	var wmu sync.Mutex
	bw := bufio.NewWriter(conn)
	writeFrame(bw, []byte("VIEW"))
	bw.Flush()
	sendInput := func(m inputMsg) {
		if !control {
			return
		}
		b, _ := json.Marshal(m)
		wmu.Lock()
		defer wmu.Unlock()
		writeFrame(bw, b)
		bw.Flush()
	}

	win, err := screen.NewWindow("Longarm desktop (spike)", 1600, 940, control)
	if err != nil {
		log.Fatal(err)
	}
	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		log.Fatal(err)
	}
	if snapPath != "" {
		time.AfterFunc(3200*time.Millisecond, func() { windows.SetEvent(ev) }) // take it even on a still screen
	}
	var qmu sync.Mutex
	var queue [][]byte
	closed := false
	go func() {
		br := bufio.NewReaderSize(conn, 1<<20)
		for {
			m, err := readFrame(br)
			qmu.Lock()
			if err != nil {
				closed = true
			} else {
				queue = append(queue, m)
			}
			qmu.Unlock()
			windows.SetEvent(ev)
			if err != nil {
				return
			}
		}
	}()

	var deadline time.Time
	if seconds > 0 {
		deadline = time.Now().Add(time.Duration(seconds) * time.Second)
		time.AfterFunc(time.Duration(seconds)*time.Second, func() { windows.SetEvent(ev) })
	}
	var p *screen.Presenter
	var frameW, frameH int
	var lat []time.Duration
	var bytes, frames int
	statsAt := time.Now()
	codec := ""
	win.OnResize = func() {
		if p != nil {
			if err := p.Resize(); err != nil {
				log.Print(err)
			}
		}
	}
	// Client coordinates to the remote display's.
	toFrame := func(x, y int) (int, int, bool) {
		if p == nil {
			return 0, 0, false
		}
		dx, dy, dw, dh := p.Dest()
		if dw == 0 || dh == 0 {
			return 0, 0, false
		}
		fx, fy := (x-dx)*frameW/dw, (y-dy)*frameH/dh
		return min(max(fx, 0), frameW-1), min(max(fy, 0), frameH-1), true
	}
	win.OnMouse = func(kind, x, y, button, wdx, wdy int) {
		switch kind {
		case screen.MouseMoved, screen.MouseDown, screen.MouseUp:
			fx, fy, ok := toFrame(x, y)
			if !ok {
				return
			}
			sendInput(inputMsg{T: "m", X: fx, Y: fy})
			if kind != screen.MouseMoved {
				sendInput(inputMsg{T: "b", B: button, D: kind == screen.MouseDown})
			}
		case screen.MouseWheeled:
			sendInput(inputMsg{T: "w", DX: wdx, DY: wdy})
		}
	}
	win.OnKey = func(scan uint16, down bool) { sendInput(inputMsg{T: "k", C: scan, D: down}) }
	win.OnSAS = func() { log.Print("Ctrl+Alt+Del requested (the SYSTEM agent sends it: not in the spike)") }

	win.Run(ev, func() {
		qmu.Lock()
		q := queue
		queue = nil
		gone := closed
		qmu.Unlock()
		for _, m := range q {
			bytes += len(m)
			switch m[0] {
			case 'C':
				var cfg struct {
					W, H  int
					Codec string
				}
				json.Unmarshal(m[1:], &cfg)
				if p != nil && (cfg.W != frameW || cfg.H != frameH) {
					p.Close()
					p = nil
				}
				frameW, frameH, codec = cfg.W, cfg.H, cfg.Codec
				if p == nil {
					if p, err = screen.NewPresenter(win.HWND, frameW, frameH); err != nil {
						log.Fatal(err)
					}
				}
			case 'H':
				if p == nil || len(m) < 10 {
					continue
				}
				captured := time.Unix(0, int64(binary.BigEndian.Uint64(m[2:])))
				shown, err := p.Decode(m[10:])
				if err != nil {
					log.Print(err)
					continue
				}
				if shown {
					frames++
					lat = append(lat, time.Since(captured))
				}
			}
		}
		if snapArmed && p != nil && time.Since(started) > 3*time.Second {
			snapArmed = false
			p.Snapshot = func(bgra []byte, w, h int) { saveBGRA(snapPath, bgra, w, h) }
			p.Redraw()
		}
		if s := time.Since(statsAt); s >= 2*time.Second {
			t := fmt.Sprintf("Longarm desktop (spike) - %s %dx%d - %.1f fps, %.0f KB/s", codec, frameW, frameH, float64(frames)/s.Seconds(), float64(bytes)/1024/s.Seconds())
			if len(lat) > 0 {
				sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
				t += fmt.Sprintf(", capture→present p50 %v p95 %v", lat[len(lat)/2].Round(100*time.Microsecond), lat[len(lat)*95/100].Round(100*time.Microsecond))
			}
			win.SetTitle(t)
			fmt.Println(t)
			lat, bytes, frames, statsAt = lat[:0], 0, 0, time.Now()
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			gone = true
		}
		if gone {
			log.Print("server closed the connection")
			win.Close()
		}
	})
	if p != nil {
		p.Close()
	}
}

// saveBGRA writes a BGRA image as a PNG (the viewer's -snapshot).
func saveBGRA(path string, bgra []byte, w, h int) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(bgra); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = bgra[i+2], bgra[i+1], bgra[i], 255
	}
	f, err := os.Create(path)
	if err != nil {
		log.Print(err)
		return
	}
	defer f.Close()
	png.Encode(f, img)
	log.Printf("snapshot: %s (%dx%d)", path, w, h)
}
