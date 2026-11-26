//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/coder/websocket"

	"longarm/internal/screen"
)

func serve(addr, tcpAddr string, display, q int, codec string, vo videoOptions) {
	var one sync.Mutex // one viewer at a time
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(viewerHTML)
	})
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
		ctx := r.Context()
		session(ctx, codec, display, q, vo,
			func(m []byte) error { return ws.Write(ctx, websocket.MessageBinary, m) },
			func() ([]byte, error) { _, b, err := ws.Read(ctx); return b, err })
		ws.Close(websocket.StatusNormalClosure, "")
	})
	if tcpAddr != "" {
		l, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("native viewers: spike view -connect %s", tcpAddr)
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					log.Fatal(err)
				}
				go func() {
					defer c.Close()
					c.(*net.TCPConn).SetNoDelay(true)
					br := bufio.NewReader(c)
					// The first message says who connects: "VIEW" or "POKE".
					hello, err := readFrame(br)
					if err != nil {
						return
					}
					if string(hello) == "POKE" {
						poked(br) // input only, next to a viewer
						return
					}
					if !one.TryLock() {
						return
					}
					defer one.Unlock()
					bw := bufio.NewWriterSize(c, 1<<20)
					session(context.Background(), "h264", display, q, vo,
						func(m []byte) error {
							if err := writeFrame(bw, m); err != nil {
								return err
							}
							return bw.Flush()
						},
						func() ([]byte, error) { return readFrame(br) })
				}()
			}
		}()
	}
	log.Printf("open http://%s/", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// poked applies a poke connection's input (spike poke): after the
// "POKE" message, inputMsg JSON messages, coordinates on display 0.
func poked(br *bufio.Reader) {

	for {
		b, err := readFrame(br)
		if err != nil {
			return
		}
		var m inputMsg
		if json.Unmarshal(b, &m) == nil {
			log.Printf("poke: %s", b)
			applyInput(m, 0, 0)
		}
	}
}

// writeFrame and readFrame: the TCP transport's messages, a uint32 BE
// length then the message.
func writeFrame(w io.Writer, m []byte) error {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(m)))
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	_, err := w.Write(m)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	l := binary.BigEndian.Uint32(n[:])
	if l > 64<<20 {
		return nil, fmt.Errorf("message of %d bytes", l)
	}
	b := make([]byte, l)
	_, err := io.ReadFull(r, b)
	return b, err
}

// inputs is the one thread that injects input: it follows the input
// desktop, so keys and clicks reach UAC prompts and the Winlogon screens
// when serve runs as SYSTEM in the console session (spike agent).
var inputs = make(chan func(), 64)

func init() {
	go func() {
		runtime.LockOSThread()
		for f := range inputs {
			screen.FollowInputDesktop()
			f()
		}
	}()
}

func applyInput(m inputMsg, ox, oy int) {
	done := make(chan struct{})
	inputs <- func() {
		defer close(done)
		var err error
		switch m.T {
		case "m":
			err = screen.MouseMove(ox+m.X, oy+m.Y)
		case "b":
			err = screen.MouseButton(m.B, m.D)
		case "w":
			err = screen.MouseWheel(m.DX, m.DY)
		case "k":
			err = screen.Key(m.C, m.D)
		case "sas":
			err = screen.SendSAS()
			log.Print("sent Ctrl+Alt+Del")
		}
		if err != nil {
			log.Print(err)
		}
	}
	<-done
}

// session streams to one viewer until it goes: frames out through send,
// input (JSON inputMsg) in through recv.
func session(parent context.Context, codec string, display, q int, vo videoOptions, send func([]byte) error, recv func() ([]byte, error)) {
	ctx, cancel := context.WithCancel(parent)
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
			b, err := recv()
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
			applyInput(m, ox, oy)
		}
	}()
	sent, start := 0, time.Now()
	for msgs := range frames {
		for _, m := range msgs {
			if err := send(m); err != nil {
				cancel()
				for range frames {
				}
				return
			}
			sent += len(m)
		}
	}
	log.Printf("viewer gone: %.1f MB in %v", float64(sent)/1e6, time.Since(start).Round(time.Second))
}
