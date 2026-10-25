package longnet_test

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
	"github.com/lubobill1990/longarm/internal/transport"
	"github.com/lubobill1990/longarm/internal/transport/longnet"
)

// Micro-benchmarks of one end-to-end QUIC stream over loopback, per path:
//
//	go test -run '^$' -bench . -benchtime 3s ./internal/transport/longnet/
var benchPaths = []struct {
	name string
	mod  func(*longnet.Options)
	kind transport.PathKind
}{
	{"direct", nil, transport.PathDirect},
	{"relay-udp", func(o *longnet.Options) { o.DisableDirect = true }, transport.PathRelay},
	{"relay-tcp", func(o *longnet.Options) {
		o.DisableDirect = true
		o.Mode = relay.ModeTCP
	}, transport.PathRelay},
}

func benchConn(b *testing.B, mod func(*longnet.Options), kind transport.PathKind, handler func(net.Conn)) net.Conn {
	w := newWorld(b)
	wn, _ := w.node(pki.RoleWorker, "w1", mod)
	ln, err := wn.Listen()
	if err != nil {
		b.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handler(c)
		}
	}()
	cn, _ := w.node(pki.RoleClient, "alice", mod)
	c, _, err := cn.Dial(b.Context(), "w1")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { c.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for pathOf(c).Kind != kind {
		if time.Now().After(deadline) {
			b.Fatalf("path %v", pathOf(c))
		}
		time.Sleep(20 * time.Millisecond)
	}
	return c
}

func BenchmarkThroughput(b *testing.B) {
	for _, p := range benchPaths {
		b.Run(p.name, func(b *testing.B) {
			done := make(chan int64, 1)
			c := benchConn(b, p.mod, p.kind, func(c net.Conn) {
				n, _ := io.Copy(io.Discard, c)
				done <- n
			})
			buf := make([]byte, 64<<10)
			b.SetBytes(int64(len(buf)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := c.Write(buf); err != nil {
					b.Fatal(err)
				}
			}
			c.(interface{ CloseWrite() error }).CloseWrite()
			<-done
			b.ReportMetric(float64(int64(b.N)*int64(len(buf)))*8/1e6/b.Elapsed().Seconds(), "Mbit/s")
		})
	}
}

func BenchmarkRTT(b *testing.B) {
	for _, p := range benchPaths {
		b.Run(p.name, func(b *testing.B) {
			c := benchConn(b, p.mod, p.kind, func(c net.Conn) { io.Copy(c, c) })
			msg := make([]byte, 64)
			got := make([]byte, 64)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := c.Write(msg); err != nil {
					b.Fatal(err)
				}
				if _, err := io.ReadFull(c, got); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
