package hub

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/cryptobyte"
)

// alpnMux splits one TCP listener by the ALPN protocols in each TLS
// ClientHello, without terminating TLS: the hub's gRPC API ("h2") and the
// longnet relay's TLS/TCP fallback share :7443/tcp. The peeked bytes are
// replayed to whoever handles the connection.
type alpnMux struct {
	ln     net.Listener
	routes map[string]*chanListener // ALPN -> listener
	def    *chanListener
	done   chan struct{}
	once   sync.Once
}

const helloTimeout = 10 * time.Second

func newALPNMux(ln net.Listener) *alpnMux {
	m := &alpnMux{ln: ln, routes: map[string]*chanListener{}, done: make(chan struct{})}
	m.def = m.newListener()
	return m
}

func (m *alpnMux) newListener() *chanListener {
	return &chanListener{addr: m.ln.Addr(), ch: make(chan net.Conn), done: m.done, closed: make(chan struct{})}
}

// Route returns the listener for connections offering alpn.
func (m *alpnMux) Route(alpn string) net.Listener {
	l := m.newListener()
	m.routes[alpn] = l
	return l
}

// Default is the listener for everything else.
func (m *alpnMux) Default() net.Listener { return m.def }

// Serve accepts until the listener is closed.
func (m *alpnMux) Serve() error {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			m.Close()
			return err
		}
		go m.dispatch(c)
	}
}

// Close stops accepting; routed listeners report net.ErrClosed.
func (m *alpnMux) Close() error {
	m.once.Do(func() { close(m.done) })
	return m.ln.Close()
}

func (m *alpnMux) dispatch(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(helloTimeout))
	hello, protos := peekClientHello(c)
	c.SetReadDeadline(time.Time{})
	pc := &prefixConn{Conn: c, r: io.MultiReader(bytes.NewReader(hello), c)}
	dst := m.def
	for _, p := range protos {
		if l, ok := m.routes[p]; ok {
			dst = l
			break
		}
	}
	select {
	case dst.ch <- pc:
	case <-m.done:
		c.Close()
	case <-dst.closed:
		c.Close()
	}
}

// peekClientHello reads the TLS records holding the ClientHello and returns
// the bytes read and the offered ALPN protocols (nil if it is not TLS).
func peekClientHello(r io.Reader) (read []byte, alpn []string) {
	var buf bytes.Buffer
	var hs []byte // reassembled handshake bytes
	for len(buf.Bytes()) < 64<<10 {
		var hdr [5]byte
		if _, err := io.ReadFull(io.TeeReader(r, &buf), hdr[:]); err != nil {
			return buf.Bytes(), nil
		}
		if hdr[0] != 22 { // handshake
			return buf.Bytes(), nil
		}
		n := int(hdr[3])<<8 | int(hdr[4])
		rec := make([]byte, n)
		if _, err := io.ReadFull(io.TeeReader(r, &buf), rec); err != nil {
			return buf.Bytes(), nil
		}
		hs = append(hs, rec...)
		if len(hs) >= 4 {
			if hs[0] != 1 { // client_hello
				return buf.Bytes(), nil
			}
			total := 4 + (int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3]))
			if len(hs) >= total {
				return buf.Bytes(), parseALPN(hs[4:total])
			}
		}
	}
	return buf.Bytes(), nil
}

func parseALPN(b []byte) []string {
	s := cryptobyte.String(b)
	var sid, suites, comp, exts cryptobyte.String
	if !s.Skip(2+32) || !s.ReadUint8LengthPrefixed(&sid) || !s.ReadUint16LengthPrefixed(&suites) ||
		!s.ReadUint8LengthPrefixed(&comp) || !s.ReadUint16LengthPrefixed(&exts) {
		return nil
	}
	for !exts.Empty() {
		var typ uint16
		var data cryptobyte.String
		if !exts.ReadUint16(&typ) || !exts.ReadUint16LengthPrefixed(&data) {
			return nil
		}
		if typ != 16 { // application_layer_protocol_negotiation
			continue
		}
		var list cryptobyte.String
		if !data.ReadUint16LengthPrefixed(&list) {
			return nil
		}
		var out []string
		for !list.Empty() {
			var p cryptobyte.String
			if !list.ReadUint8LengthPrefixed(&p) {
				return nil
			}
			out = append(out, string(p))
		}
		return out
	}
	return nil
}

type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(b []byte) (int, error) { return c.r.Read(b) }

type chanListener struct {
	addr   net.Addr
	ch     chan net.Conn
	done   chan struct{} // the mux's
	closed chan struct{}
	once   sync.Once
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Close stops this route (the mux owns the socket).
func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }
