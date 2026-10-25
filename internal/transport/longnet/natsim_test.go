package longnet_test

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"
)

// A userspace NAT simulator on loopback. Every NAT mapping is a real UDP
// socket on 127.0.0.1 (its port plays the "public ip:port"); a registry
// attributes ports to hosts, so that address-dependent filtering can treat
// all ports of one router (or of the relay) as one public IP even though
// everything is 127.0.0.1. Nodes behind a router never receive anything on
// their own socket: their LAN candidates are dead, like private addresses
// seen from the Internet.

type mappingKind int

const (
	eim  mappingKind = iota // endpoint-independent mapping ("easy")
	adpm                    // address-and-port-dependent mapping ("hard", symmetric)
)

type filterKind int

const (
	eif  filterKind = iota // endpoint-independent filtering (full cone)
	adf                    // address-dependent (restricted cone): any port of a host we sent to
	apdf                   // address-and-port-dependent (port-restricted): exactly what we sent to
)

// internet attributes outside ports to hosts and can cut host pairs.
type internet struct {
	mu    sync.Mutex
	owner map[uint16]string
	cuts  map[[2]string]bool
}

func newInternet() *internet {
	return &internet{owner: map[uint16]string{}, cuts: map[[2]string]bool{}}
}

func (in *internet) own(port uint16, host string) {
	in.mu.Lock()
	in.owner[port] = host
	in.mu.Unlock()
}

// host is the "public IP" of an address: its registered owner, or the
// address itself (an un-NATed node is its own host).
func (in *internet) host(a netip.AddrPort) string {
	in.mu.Lock()
	defer in.mu.Unlock()
	if h, ok := in.owner[a.Port()]; ok {
		return h
	}
	return a.String()
}

func (in *internet) cut(a, b string, on bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if on {
		in.cuts[[2]string{a, b}], in.cuts[[2]string{b, a}] = true, true
	} else {
		delete(in.cuts, [2]string{a, b})
		delete(in.cuts, [2]string{b, a})
	}
}

func (in *internet) blocked(a, b string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.cuts[[2]string{a, b}]
}

type natPkt struct {
	b    []byte
	from netip.AddrPort
}

type natMapping struct {
	pc       net.PacketConn
	sentTo   map[netip.AddrPort]bool
	sentHost map[string]bool
}

// natRouter is one NAT box with one inside node. It is the node's
// net.PacketConn (WrapPacketConn); the node's real socket is only kept for
// LocalAddr.
type natRouter struct {
	in      *internet
	name    string
	mapping mappingKind
	filter  filterKind
	inner   net.PacketConn

	mu     sync.Mutex
	maps   map[netip.AddrPort]*natMapping // key: destination (adpm) or zero (eim)
	closed bool

	inbox     chan natPkt
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

func newNATRouter(in *internet, name string, m mappingKind, f filterKind) *natRouter {
	return &natRouter{in: in, name: name, mapping: m, filter: f, maps: map[netip.AddrPort]*natMapping{},
		inbox: make(chan natPkt, 4096), done: make(chan struct{})}
}

func apOf(a net.Addr) netip.AddrPort {
	ap := a.(*net.UDPAddr).AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

func (r *natRouter) WriteTo(b []byte, a net.Addr) (int, error) {
	dst := apOf(a)
	dstHost := r.in.host(dst)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, net.ErrClosed
	}
	var key netip.AddrPort
	if r.mapping == adpm {
		key = dst
	}
	m := r.maps[key]
	if m == nil {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			r.mu.Unlock()
			return 0, err
		}
		m = &natMapping{pc: pc, sentTo: map[netip.AddrPort]bool{}, sentHost: map[string]bool{}}
		r.maps[key] = m
		r.in.own(apOf(pc.LocalAddr()).Port(), r.name)
		r.wg.Add(1)
		go r.readOutside(m)
	}
	m.sentTo[dst], m.sentHost[dstHost] = true, true
	pc := m.pc
	r.mu.Unlock()
	if r.in.blocked(r.name, dstHost) {
		return len(b), nil
	}
	pc.WriteTo(b, a)
	return len(b), nil
}

func (r *natRouter) readOutside(m *natMapping) {
	defer r.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, a, err := m.pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		src := apOf(a)
		srcHost := r.in.host(src)
		r.mu.Lock()
		ok := !r.closed
		switch r.filter {
		case adf:
			ok = ok && m.sentHost[srcHost]
		case apdf:
			ok = ok && m.sentTo[src]
		}
		r.mu.Unlock()
		if !ok || r.in.blocked(r.name, srcHost) {
			continue
		}
		select {
		case r.inbox <- natPkt{b: append([]byte(nil), buf[:n]...), from: src}:
		default: // full: drop like a real link
		}
	}
}

func (r *natRouter) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case p := <-r.inbox:
		return copy(b, p.b), net.UDPAddrFromAddrPort(p.from), nil
	case <-r.done:
		return 0, nil, net.ErrClosed
	}
}

// reset forgets all mappings (a NAT reboot / mapping timeout): the next
// packets get new public ports.
func (r *natRouter) reset() {
	r.mu.Lock()
	for _, m := range r.maps {
		m.pc.Close()
	}
	r.maps = map[netip.AddrPort]*natMapping{}
	r.mu.Unlock()
}

func (r *natRouter) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		for _, m := range r.maps {
			m.pc.Close()
		}
		r.mu.Unlock()
		close(r.done)
		r.inner.Close()
	})
	r.wg.Wait()
	return nil
}

func (r *natRouter) LocalAddr() net.Addr              { return r.inner.LocalAddr() }
func (r *natRouter) SetDeadline(time.Time) error      { return nil }
func (r *natRouter) SetReadDeadline(time.Time) error  { return nil }
func (r *natRouter) SetWriteDeadline(time.Time) error { return nil }
