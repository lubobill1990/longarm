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
					if !one.TryLock() {
						return
					}
					defer one.Unlock()
					c.(*net.TCPConn).SetNoDelay(true)
					bw := bufio.NewWriterSize(c, 1<<20)
					br := bufio.NewReader(c)
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
