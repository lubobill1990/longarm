package longnet

import (
	"net"
	"os"
	"sync"
	"time"
)

// bufSize fits any UDP payload we accept (end-to-end QUIC packets are 1200
// bytes; relay QUIC packets 1350).
const bufSize = 2048

var bufPool = sync.Pool{New: func() any { b := make([]byte, bufSize); return &b }}

func getBuf() *[]byte  { return bufPool.Get().(*[]byte) }
func putBuf(b *[]byte) { bufPool.Put(b) }

type packet struct {
	buf  *[]byte // pooled backing buffer (nil if data is owned)
	data []byte
	addr net.Addr
}

// deadline is a resettable read deadline (the net.Pipe pattern).
type deadline struct {
	mu    sync.Mutex
	timer *time.Timer
	ch    chan struct{}
}

func newDeadline() *deadline { return &deadline{ch: make(chan struct{})} }

func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil && !d.timer.Stop() {
		<-d.ch // the timer fired and closed ch: wait for it (it already has)
	}
	d.timer = nil
	closed := false
	select {
	case <-d.ch:
		closed = true
	default:
	}
	if t.IsZero() {
		if closed {
			d.ch = make(chan struct{})
		}
		return
	}
	if dur := time.Until(t); dur > 0 {
		if closed {
			d.ch = make(chan struct{})
		}
		ch := d.ch
		d.timer = time.AfterFunc(dur, func() { close(ch) })
		return
	}
	if !closed {
		close(d.ch)
	}
}

func (d *deadline) wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ch
}

// vconn is a virtual net.PacketConn fed by the node's socket reader. Reads
// come from a bounded queue (overflow drops, like a socket buffer); writes go
// through the write function.
type vconn struct {
	local net.Addr
	q     chan packet
	rdl   *deadline
	write func(p []byte, addr net.Addr) (int, error)

	closeOnce sync.Once
	closed    chan struct{}
}

func newVConn(local net.Addr, depth int, write func([]byte, net.Addr) (int, error)) *vconn {
	return &vconn{local: local, q: make(chan packet, depth), rdl: newDeadline(), write: write, closed: make(chan struct{})}
}

// push hands a packet to the reader; it takes ownership of pk.buf.
func (c *vconn) push(pk packet) bool {
	select {
	case <-c.closed:
	default:
		select {
		case c.q <- pk:
			return true
		default:
		}
	}
	if pk.buf != nil {
		putBuf(pk.buf)
	}
	return false
}

func (c *vconn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case <-c.closed:
		return 0, nil, net.ErrClosed
	case <-c.rdl.wait():
		return 0, nil, os.ErrDeadlineExceeded
	case pk := <-c.q:
		n := copy(b, pk.data)
		if pk.buf != nil {
			putBuf(pk.buf)
		}
		return n, pk.addr, nil
	}
}

func (c *vconn) WriteTo(b []byte, addr net.Addr) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	return c.write(b, addr)
}

func (c *vconn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *vconn) LocalAddr() net.Addr                { return c.local }
func (c *vconn) SetDeadline(t time.Time) error      { return c.SetReadDeadline(t) }
func (c *vconn) SetReadDeadline(t time.Time) error  { c.rdl.set(t); return nil }
func (c *vconn) SetWriteDeadline(t time.Time) error { return nil }

// SetReadBuffer and SetWriteBuffer satisfy quic-go's buffer tuning; the node
// sizes the real socket's buffers itself.
func (c *vconn) SetReadBuffer(int) error  { return nil }
func (c *vconn) SetWriteBuffer(int) error { return nil }
