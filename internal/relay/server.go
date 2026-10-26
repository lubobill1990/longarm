package relay

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"golang.org/x/time/rate"

	"github.com/lubobill1990/longarm/internal/pki"
)

// Protocol timings (shared with the client).
const (
	helloTimeout    = 10 * time.Second
	tcpPingInterval = 5 * time.Second
	tcpReadTimeout  = 4 * tcpPingInterval
	quicIdleTimeout = 15 * time.Second
	quicKeepAlive   = 4 * time.Second
	peerGoneEvery   = time.Second // at most one PeerGone per (session, dst) per interval
	cmmRate         = 2           // CallMeMaybe frames per second per session (burst cmmBurst)
	cmmBurst        = 10
	stunRate        = 50  // STUN answers per second per source IP (nodes send a few per 25 s; the reply is
	stunBurst       = 100 // barely bigger than the request, but an unthrottled reflector is still a reflector)
)

// QUICConfig is the QUIC configuration of relay sessions (both ends):
// datagrams on, fixed 1350-byte packets so that a 1200-byte end-to-end
// packet plus the 8-byte id always fits into one datagram.
func QUICConfig() *quic.Config {
	return &quic.Config{
		EnableDatagrams:         true,
		InitialPacketSize:       1350,
		DisablePathMTUDiscovery: true,
		MaxIdleTimeout:          quicIdleTimeout,
		KeepAlivePeriod:         quicKeepAlive,
		HandshakeIdleTimeout:    helloTimeout,
		MaxIncomingStreams:      4,
		MaxIncomingUniStreams:   -1,
	}
}

// Server is the relay server. Configure the exported fields, then call
// ServeQUIC and/or ServeTCP (each blocks); Close stops everything.
type Server struct {
	TLS    *tls.Config // ServerTLS(bundle)
	Logger *slog.Logger
	// Rate limits the bytes per second each session may send through the
	// relay (token bucket with Burst bytes); 0 = unlimited.
	Rate  float64
	Burst int
	// StatelessResetKey lets a restarted relay reset the QUIC sessions of
	// its previous incarnation at once (nodes then reconnect immediately
	// instead of waiting for the idle timeout). Default: derived from the
	// relay's private key, so it is stable across restarts.
	StatelessResetKey *quic.StatelessResetKey
	// STUN is the list of STUN endpoints advertised to nodes in Welcome
	// (":port" = the relay's own IP as dialed by the node, or "host:port").
	// Default: the ports served by ServeQUIC and ServeSTUN. Two endpoints
	// that differ in port (better: in IP) let nodes detect whether their NAT
	// mapping varies by destination.
	STUN []string

	initOnce sync.Once
	ctx      context.Context
	cancel   context.CancelFunc

	mu        sync.Mutex
	sessions  map[ID]*session
	closers   []io.Closer
	stunPorts []string // ":port" of every socket answering STUN
	stunMu    sync.Mutex
	stunLim   map[netip.Addr]*rate.Limiter // per source IP
	serveWG   sync.WaitGroup
	sessionWG sync.WaitGroup

	sessionsTotal  atomic.Uint64
	packets        atomic.Uint64
	bytes          atomic.Uint64
	dropNoPeer     atomic.Uint64
	dropRate       atomic.Uint64
	dropQueue      atomic.Uint64
	dropPolicy     atomic.Uint64
	callMeMaybes   atomic.Uint64
	handshakeFails atomic.Uint64
	stunRequests   atomic.Uint64

	// workerPairs: worker pairs allowed to talk (SetWorkerPairs).
	workerPairs atomic.Pointer[map[pairKey]struct{}]
}

func (s *Server) init() {
	s.initOnce.Do(func() {
		s.ctx, s.cancel = context.WithCancel(context.Background())
		s.sessions = map[ID]*session{}
		if s.Logger == nil {
			s.Logger = slog.New(slog.DiscardHandler)
		}
		s.stunLim = map[netip.Addr]*rate.Limiter{}
	})
}

func (s *Server) addCloser(c io.Closer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return net.ErrClosed
	}
	s.closers = append(s.closers, c)
	return nil
}

// Close stops all listeners and sessions. Sessions are closed first, so
// nodes are told (QUIC CONNECTION_CLOSE / a TCP close frame) and reconnect
// right away.
func (s *Server) Close() error {
	s.init()
	s.mu.Lock()
	s.cancel()
	closers := s.closers
	s.closers = nil
	sessions := make([]*session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		sessions = append(sessions, ss)
	}
	s.mu.Unlock()
	for _, ss := range sessions {
		ss.close("relay shutting down")
	}
	s.sessionWG.Wait()
	time.Sleep(20 * time.Millisecond) // let QUIC flush the close packets
	for _, c := range closers {
		c.Close()
	}
	return nil
}

func (s *Server) resetKey() *quic.StatelessResetKey {
	if s.StatelessResetKey != nil {
		return s.StatelessResetKey
	}
	if len(s.TLS.Certificates) == 0 {
		return nil
	}
	der, err := x509.MarshalPKCS8PrivateKey(s.TLS.Certificates[0].PrivateKey)
	if err != nil {
		return nil
	}
	k := quic.StatelessResetKey(sha256.Sum256(append([]byte("longnet relay stateless reset\x00"), der...)))
	return &k
}

// ServeQUIC serves relay sessions over QUIC on pc until Close.
func (s *Server) ServeQUIC(pc net.PacketConn) error {
	s.init()
	if s.TLS == nil {
		return errors.New("relay: no TLS config")
	}
	tr := &quic.Transport{Conn: pc, StatelessResetKey: s.resetKey()}
	ln, err := tr.Listen(s.TLS, QUICConfig())
	if err != nil {
		return err
	}
	if err := s.addCloser(tr); err != nil {
		tr.Close()
		return err
	}
	s.addSTUNPort(pc.LocalAddr())
	go func() { // STUN on the QUIC port: quic-go hands over non-QUIC packets
		buf := make([]byte, 1500)
		for {
			n, from, err := tr.ReadNonQUICPacket(s.ctx, buf)
			if err != nil {
				return
			}
			if r := s.stunReply(buf[:n], from); r != nil {
				tr.WriteTo(r, from)
			}
		}
	}()
	for {
		c, err := ln.Accept(s.ctx)
		if err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !s.addSession() {
			c.CloseWithError(0, "relay shutting down")
			return nil
		}
		go func() {
			defer s.sessionWG.Done()
			s.handleQUIC(c)
		}()
	}
}

// ServeTCP serves relay sessions over TLS/TCP on ln until Close.
func (s *Server) ServeTCP(ln net.Listener) error {
	s.init()
	if s.TLS == nil {
		return errors.New("relay: no TLS config")
	}
	if err := s.addCloser(ln); err != nil {
		ln.Close()
		return err
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		if !s.addSession() {
			c.Close()
			return nil
		}
		go func() {
			defer s.sessionWG.Done()
			s.handleTCP(c)
		}()
	}
}

// ServeSTUN answers STUN binding requests on pc until Close (the second
// observation point for NAT type detection).
func (s *Server) ServeSTUN(pc net.PacketConn) error {
	s.init()
	if err := s.addCloser(pc); err != nil {
		pc.Close()
		return err
	}
	s.addSTUNPort(pc.LocalAddr())
	buf := make([]byte, 1500)
	errs := 0
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			errs++
			if errs > 100 { // a persistent error: don't spin
				time.Sleep(10 * time.Millisecond)
			}
			continue // e.g. Windows reports ICMP errors of earlier sends here
		}
		errs = 0
		if r := s.stunReply(buf[:n], from); r != nil {
			pc.WriteTo(r, from)
		}
	}
}

func (s *Server) addSTUNPort(a net.Addr) {
	ap := addrPortOf(a)
	s.mu.Lock()
	s.stunPorts = append(s.stunPorts, fmt.Sprintf(":%d", ap.Port()))
	s.mu.Unlock()
}

func (s *Server) stunEndpoints() []string {
	if s.STUN != nil {
		return s.STUN
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stunPorts...)
}

// stunReply answers a binding request (nil for anything else).
func (s *Server) stunReply(b []byte, from net.Addr) []byte {
	if !isSTUNBindingRequest(b) {
		return nil
	}
	ap := addrPortOf(from)
	if !ap.IsValid() || !s.stunAllow(ap.Addr()) {
		return nil
	}
	s.stunRequests.Add(1)
	return stunResponse(b, ap)
}

// addSession counts a session goroutine unless the server is closing (Close
// cancels under s.mu before waiting, so no Add races the Wait).
func (s *Server) addSession() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return false
	}
	s.sessionWG.Add(1)
	return true
}

// session is one connected node.
type session struct {
	srv    *Server
	id     ID
	ident  pki.Identity
	kind   string // "udp" | "tcp"
	remote netip.AddrPort
	udp    *quic.Conn // UDP sessions: the current remote address follows NAT rebinding
	since  time.Time

	infoMu sync.Mutex
	cands  []netip.AddrPort
	nat    string
	mapped []netip.AddrPort

	lim    *rate.Limiter // data bytes (nil = unlimited)
	cmmLim *rate.Limiter // CallMeMaybe frames

	ctrl chan []byte // encoded frames
	data chan []byte // id-prefixed packets (src id)

	done      chan struct{}
	closeOnce sync.Once
	kick      func(reason string) // tears down the transport

	bytesIn, bytesOut, pktsIn, pktsOut atomic.Uint64

	goneMu sync.Mutex
	goneAt map[ID]time.Time
}

// reflexive is the node's UDP address as seen by the relay now (UDP
// sessions only; QUIC follows NAT rebinding).
func (ss *session) reflexive() netip.AddrPort {
	if ss.udp == nil {
		return netip.AddrPort{}
	}
	return addrPortOf(ss.udp.RemoteAddr())
}

// remoteIP is the node's current IP as seen by the relay.
func (ss *session) remoteIP() netip.Addr {
	if r := ss.reflexive(); r.IsValid() {
		return r.Addr()
	}
	return ss.remote.Addr()
}

func (ss *session) close(reason string) {
	ss.closeOnce.Do(func() {
		ss.kick(reason) // before done: the TCP writer reads the reason when done closes
		close(ss.done)
	})
}

func (ss *session) sendCtrl(frame []byte) {
	select {
	case ss.ctrl <- frame:
	case <-ss.done:
	default: // control queue full: the node is not reading; drop
	}
}

func (s *Server) newSession(ident pki.Identity, kind string, remote netip.AddrPort, h Hello) *session {
	ss := &session{
		srv: s, id: NodeID(ident, h.Instance), ident: ident, kind: kind, remote: remote,
		cands: cleanCandidates(h.Candidates), since: time.Now(),
		ctrl: make(chan []byte, 64), data: make(chan []byte, 1024), done: make(chan struct{}),
		goneAt: map[ID]time.Time{}, cmmLim: rate.NewLimiter(cmmRate, cmmBurst),
	}
	if s.Rate > 0 {
		burst := s.Burst
		if burst < 64<<10 {
			burst = 64 << 10
		}
		ss.lim = rate.NewLimiter(rate.Limit(s.Rate), burst)
	}
	return ss
}

// errIDInUse rejects a session whose id is held by a different identity.
var errIDInUse = errors.New("node id in use by another identity")

// register makes ss the session for its id; an older session of the same
// identity (a reconnect, or a duplicate worker) is closed: newest wins. A
// different identity with the same id (a hash collision, or an attempt to
// grind one) is refused.
func (s *Server) register(ss *session) error {
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return net.ErrClosed
	}
	old := s.sessions[ss.id]
	if old != nil && old.ident != ss.ident {
		s.mu.Unlock()
		s.Logger.Warn("relay: refused session: id held by another identity", "node", ss.ident, "id", ss.id, "holder", old.ident)
		return errIDInUse
	}
	s.sessions[ss.id] = ss
	s.mu.Unlock()
	s.sessionsTotal.Add(1)
	if old != nil {
		s.Logger.Warn("relay: session replaced by a newer one", "node", ss.ident, "id", ss.id, "old", old.remote, "new", ss.remote)
		old.close("replaced by a newer session")
	}
	s.Logger.Info("relay: session up", "node", ss.ident, "id", ss.id, "transport", ss.kind, "remote", ss.remote)
	return nil
}

func (s *Server) unregister(ss *session) {
	s.mu.Lock()
	if s.sessions[ss.id] == ss {
		delete(s.sessions, ss.id)
	}
	s.mu.Unlock()
	s.Logger.Info("relay: session down", "node", ss.ident, "id", ss.id, "transport", ss.kind,
		"bytes_in", ss.bytesIn.Load(), "bytes_out", ss.bytesOut.Load(), "duration", time.Since(ss.since).Round(time.Second))
}

func (s *Server) lookup(id ID) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *Server) welcome(ss *session) []byte {
	f, _ := jsonFrame(FrameWelcome, Welcome{ID: ss.id.String(), Reflexive: ss.reflexive(), Transport: ss.kind,
		STUN: s.stunEndpoints()})
	return f
}

// forward relays one packet from src to dst.
func (s *Server) forward(src *session, dst ID, pkt []byte) {
	src.pktsIn.Add(1)
	src.bytesIn.Add(uint64(len(pkt)))
	d := s.lookup(dst)
	if d == nil {
		s.dropNoPeer.Add(1)
		src.peerGone(dst)
		return
	}
	if !s.allowed(src.ident, d.ident) {
		s.dropPolicy.Add(1)
		return
	}
	if src.lim != nil && !src.lim.AllowN(time.Now(), len(pkt)) {
		s.dropRate.Add(1)
		return
	}
	b := AppendPacket(make([]byte, 0, 8+len(pkt)), src.id, pkt)
	select {
	case d.data <- b:
		s.packets.Add(1)
		s.bytes.Add(uint64(len(pkt)))
	default:
		s.dropQueue.Add(1)
	}
}

func (ss *session) peerGone(dst ID) {
	now := time.Now()
	ss.goneMu.Lock()
	if t, ok := ss.goneAt[dst]; ok && now.Sub(t) < peerGoneEvery {
		ss.goneMu.Unlock()
		return
	}
	if len(ss.goneAt) > 1024 {
		clear(ss.goneAt)
	}
	ss.goneAt[dst] = now
	ss.goneMu.Unlock()
	f, _ := AppendFrame(nil, FramePeerGone, encodeID(dst))
	ss.sendCtrl(f)
}

// handleCtrl processes one control frame from a node.
func (s *Server) handleCtrl(ss *session, typ byte, p []byte) error {
	switch typ {
	case FrameCallMeMaybe:
		if !ss.cmmLim.Allow() {
			s.dropRate.Add(1)
			return nil
		}
		var cm CallMeMaybe
		if err := json.Unmarshal(p, &cm); err != nil {
			return fmt.Errorf("bad CallMeMaybe: %w", err)
		}
		dst, err := ParseID(cm.Peer)
		if err != nil {
			return err
		}
		d := s.lookup(dst)
		if d == nil {
			ss.peerGone(dst)
			return nil
		}
		if !s.allowed(ss.ident, d.ident) {
			s.dropPolicy.Add(1)
			return nil
		}
		ss.infoMu.Lock()
		cands, nat := cm.Candidates, cm.NAT
		if len(cands) == 0 {
			cands = ss.cands
		}
		if nat == "" {
			nat = ss.nat
		}
		ss.infoMu.Unlock()
		if !ValidNAT(nat) {
			nat = NATUnknown
		}
		// The relay-observed address first (the one that works behind an
		// easy NAT); loopback only for a peer on the same IP (same host or
		// behind the same NAT) — elsewhere it would hit the peer's own host.
		refl := ss.reflexive()
		var extra []netip.AddrPort
		if refl.IsValid() {
			extra = append(extra, refl)
		}
		out := CallMeMaybe{Peer: ss.id.String(), Reply: cm.Reply, NAT: nat, Reflexive: refl}
		sameIP := ss.remoteIP() == d.remoteIP()
		for _, a := range cleanCandidates(cands, extra...) {
			if a.Addr().IsLoopback() && !sameIP {
				continue
			}
			out.Candidates = append(out.Candidates, a)
		}
		f, err := jsonFrame(FrameCallMeMaybe, out)
		if err != nil {
			return err
		}
		s.callMeMaybes.Add(1)
		d.sendCtrl(f)
	case FrameNodeInfo:
		var ni NodeInfo
		if err := json.Unmarshal(p, &ni); err != nil {
			return fmt.Errorf("bad NodeInfo: %w", err)
		}
		if !ValidNAT(ni.NAT) {
			ni.NAT = NATUnknown
		}
		ss.infoMu.Lock()
		ss.cands, ss.nat, ss.mapped = cleanCandidates(ni.Candidates), ni.NAT, cleanCandidates(ni.Mapped)
		ss.infoMu.Unlock()
	case FramePing:
		f, _ := AppendFrame(nil, FramePong, p)
		ss.sendCtrl(f)
	case FramePong:
	case FrameHello, FrameWelcome, FrameData:
		return fmt.Errorf("unexpected frame type %d", typ)
	default:
		// unknown frames are ignored for forward compatibility
	}
	return nil
}

func readHello(r io.Reader) (Hello, error) {
	typ, p, err := ReadFrame(r, nil)
	if err != nil {
		return Hello{}, err
	}
	if typ != FrameHello {
		return Hello{}, fmt.Errorf("expected hello, got frame type %d", typ)
	}
	var h Hello
	if err := json.Unmarshal(p, &h); err != nil {
		return Hello{}, fmt.Errorf("bad hello: %w", err)
	}
	return h, nil
}

func addrPortOf(a net.Addr) netip.AddrPort {
	switch a := a.(type) {
	case *net.UDPAddr:
		ap := a.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	case *net.TCPAddr:
		ap := a.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	ap, _ := netip.ParseAddrPort(a.String())
	return ap
}

func (s *Server) handleQUIC(c *quic.Conn) {
	ident, err := peerIdentity(c.ConnectionState().TLS)
	if err != nil {
		s.handshakeFails.Add(1)
		s.Logger.Warn("relay: rejected node", "remote", c.RemoteAddr(), "err", err)
		c.CloseWithError(1, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, helloTimeout)
	st, err := c.AcceptStream(ctx)
	cancel()
	if err != nil {
		c.CloseWithError(1, "no control stream")
		return
	}
	st.SetReadDeadline(time.Now().Add(helloTimeout))
	h, err := readHello(st)
	if err != nil {
		s.handshakeFails.Add(1)
		c.CloseWithError(1, err.Error())
		return
	}
	st.SetReadDeadline(time.Time{})
	ss := s.newSession(ident, "udp", addrPortOf(c.RemoteAddr()), h)
	ss.udp = c
	ss.kick = func(reason string) { c.CloseWithError(0, reason) }
	if err := s.register(ss); err != nil {
		c.CloseWithError(1, err.Error())
		return
	}
	defer s.unregister(ss)
	defer ss.close("")
	if _, err := st.Write(s.welcome(ss)); err != nil {
		return
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // control writer
		defer wg.Done()
		for {
			select {
			case f := <-ss.ctrl:
				if _, err := st.Write(f); err != nil {
					ss.close("")
					return
				}
			case <-ss.done:
				return
			}
		}
	}()
	go func() { // datagram writer
		defer wg.Done()
		for {
			select {
			case b := <-ss.data:
				if err := c.SendDatagram(b); err != nil {
					var tl *quic.DatagramTooLargeError
					if errors.As(err, &tl) {
						s.dropQueue.Add(1)
						continue
					}
					ss.close("")
					return
				}
				ss.pktsOut.Add(1)
				ss.bytesOut.Add(uint64(len(b) - 8))
			case <-ss.done:
				return
			}
		}
	}()
	go func() { // datagram reader
		defer wg.Done()
		for {
			b, err := c.ReceiveDatagram(s.ctx)
			if err != nil {
				ss.close("")
				return
			}
			dst, pkt, err := ParsePacket(b)
			if err != nil {
				continue
			}
			s.forward(ss, dst, pkt)
		}
	}()
	buf := make([]byte, 4096)
	for { // control reader
		typ, p, err := ReadFrame(st, buf)
		if err != nil {
			break
		}
		if err := s.handleCtrl(ss, typ, p); err != nil {
			s.Logger.Warn("relay: protocol error", "node", ss.ident, "err", err)
			break
		}
	}
	ss.close("")
	wg.Wait()
}

func (s *Server) handleTCP(raw net.Conn) {
	defer raw.Close()
	tc := tls.Server(raw, s.TLS)
	tc.SetDeadline(time.Now().Add(helloTimeout))
	if err := tc.HandshakeContext(s.ctx); err != nil {
		s.handshakeFails.Add(1)
		s.Logger.Debug("relay: TLS handshake failed", "remote", raw.RemoteAddr(), "err", err)
		return
	}
	ident, err := peerIdentity(tc.ConnectionState())
	if err != nil {
		s.handshakeFails.Add(1)
		s.Logger.Warn("relay: rejected node", "remote", raw.RemoteAddr(), "err", err)
		return
	}
	br := bufio.NewReaderSize(tc, 32<<10)
	h, err := readHello(br)
	if err != nil {
		s.handshakeFails.Add(1)
		return
	}
	tc.SetDeadline(time.Time{})
	ss := s.newSession(ident, "tcp", addrPortOf(raw.RemoteAddr()), h)
	var closeReason atomic.Value
	ss.kick = func(reason string) {
		closeReason.Store(reason)
		raw.SetReadDeadline(time.Now()) // reader exits; writer sends the close frame
	}
	if err := s.register(ss); err != nil {
		f, _ := AppendFrame(nil, FrameClose, []byte(err.Error()))
		tc.Write(f)
		return
	}
	defer s.unregister(ss)
	defer ss.close("")

	writerDone := make(chan struct{})
	go func() { // writer: control first, then data; flush when idle
		defer close(writerDone)
		bw := bufio.NewWriterSize(tc, 64<<10)
		write := func(f []byte) bool {
			tc.SetWriteDeadline(time.Now().Add(tcpReadTimeout))
			_, err := bw.Write(f)
			return err == nil
		}
		if !write(s.welcome(ss)) || bw.Flush() != nil {
			ss.close("")
			return
		}
		var frame []byte
		for {
			select {
			case f := <-ss.ctrl:
				if !write(f) {
					ss.close("")
					return
				}
			case b := <-ss.data:
				frame, _ = AppendFrame(frame[:0], FrameData, b)
				if !write(frame) {
					ss.close("")
					return
				}
				ss.pktsOut.Add(1)
				ss.bytesOut.Add(uint64(len(b) - 8))
			case <-ss.done:
				if r, _ := closeReason.Load().(string); r != "" {
					f, _ := AppendFrame(nil, FrameClose, []byte(r))
					write(f)
				}
				bw.Flush()
				raw.Close() // ends the reader even if it re-armed its deadline after kick
				return
			}
			if len(ss.ctrl) == 0 && len(ss.data) == 0 {
				if bw.Flush() != nil {
					ss.close("")
					return
				}
			}
		}
	}()

	buf := make([]byte, 4096)
	for {
		tc.SetReadDeadline(time.Now().Add(tcpReadTimeout))
		typ, p, err := ReadFrame(br, buf)
		if err != nil {
			break
		}
		if typ == FrameData {
			dst, pkt, err := ParsePacket(p)
			if err != nil {
				break
			}
			s.forward(ss, dst, pkt)
			continue
		}
		if err := s.handleCtrl(ss, typ, p); err != nil {
			s.Logger.Warn("relay: protocol error", "node", ss.ident, "err", err)
			break
		}
	}
	ss.close("")
	<-writerDone
}

// SessionStats describes one connected node.
type SessionStats struct {
	ID         string           `json:"id"`
	Node       string           `json:"node"`
	Transport  string           `json:"transport"`
	Remote     string           `json:"remote"`              // current source address of the session
	Reflexive  string           `json:"reflexive,omitempty"` // UDP sessions: the node's public UDP address
	NAT        string           `json:"nat,omitempty"`       // reported by the node: open | easy | hard ("" = unknown)
	Mapped     []netip.AddrPort `json:"mapped,omitempty"`    // node's STUN-observed addresses (≥2 distinct = hard NAT)
	Candidates []netip.AddrPort `json:"candidates,omitempty"`
	Since      time.Time        `json:"since"`
	BytesIn    uint64           `json:"bytes_in"`
	BytesOut   uint64           `json:"bytes_out"`
	PacketsIn  uint64           `json:"packets_in"`
	PacketsOut uint64           `json:"packets_out"`
}

// Stats is a snapshot of the relay's counters.
type Stats struct {
	Sessions       []SessionStats `json:"sessions"`
	SessionsTotal  uint64         `json:"sessions_total"`
	PacketsRelayed uint64         `json:"packets_relayed"`
	BytesRelayed   uint64         `json:"bytes_relayed"`
	DropNoPeer     uint64         `json:"drop_no_peer"`
	DropRateLimit  uint64         `json:"drop_rate_limit"`
	DropQueueFull  uint64         `json:"drop_queue_full"`
	DropPolicy     uint64         `json:"drop_policy"`
	CallMeMaybes   uint64         `json:"call_me_maybes"`
	HandshakeFails uint64         `json:"handshake_fails"`
	STUNRequests   uint64         `json:"stun_requests"`
}

// Stats returns a snapshot.
func (s *Server) Stats() Stats {
	s.init()
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		sessions = append(sessions, ss)
	}
	s.mu.Unlock()
	st := Stats{}
	for _, ss := range sessions {
		remote := ss.remote
		refl := ss.reflexive()
		if refl.IsValid() {
			remote = refl
		}
		ss.infoMu.Lock()
		cands, nat, mapped := ss.cands, ss.nat, ss.mapped
		ss.infoMu.Unlock()
		e := SessionStats{
			ID: ss.id.String(), Node: ss.ident.String(), Transport: ss.kind, Remote: remote.String(),
			NAT: nat, Mapped: mapped, Candidates: cands, Since: ss.since,
			BytesIn: ss.bytesIn.Load(), BytesOut: ss.bytesOut.Load(), PacketsIn: ss.pktsIn.Load(), PacketsOut: ss.pktsOut.Load(),
		}
		if refl.IsValid() {
			e.Reflexive = refl.String()
		}
		st.Sessions = append(st.Sessions, e)
	}
	sort.Slice(st.Sessions, func(i, j int) bool { return st.Sessions[i].Node < st.Sessions[j].Node })
	st.SessionsTotal = s.sessionsTotal.Load()
	st.PacketsRelayed = s.packets.Load()
	st.BytesRelayed = s.bytes.Load()
	st.DropNoPeer = s.dropNoPeer.Load()
	st.DropRateLimit = s.dropRate.Load()
	st.DropQueueFull = s.dropQueue.Load()
	st.DropPolicy = s.dropPolicy.Load()
	st.CallMeMaybes = s.callMeMaybes.Load()
	st.HandshakeFails = s.handshakeFails.Load()
	st.STUNRequests = s.stunRequests.Load()
	return st
}

// MetricsHandler serves /metrics (Prometheus text format) and /sessions
// (JSON Stats).
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		st := s.Stats()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		byKind := map[string]int{}
		for _, ss := range st.Sessions {
			byKind[ss.Transport]++
		}
		fmt.Fprintf(w, "# TYPE longnet_relay_sessions gauge\n")
		fmt.Fprintf(w, "longnet_relay_sessions{transport=\"udp\"} %d\n", byKind["udp"])
		fmt.Fprintf(w, "longnet_relay_sessions{transport=\"tcp\"} %d\n", byKind["tcp"])
		fmt.Fprintf(w, "# TYPE longnet_relay_sessions_total counter\nlongnet_relay_sessions_total %d\n", st.SessionsTotal)
		fmt.Fprintf(w, "# TYPE longnet_relay_packets_total counter\nlongnet_relay_packets_total %d\n", st.PacketsRelayed)
		fmt.Fprintf(w, "# TYPE longnet_relay_bytes_total counter\nlongnet_relay_bytes_total %d\n", st.BytesRelayed)
		fmt.Fprintf(w, "# TYPE longnet_relay_dropped_total counter\n")
		fmt.Fprintf(w, "longnet_relay_dropped_total{reason=\"no_peer\"} %d\n", st.DropNoPeer)
		fmt.Fprintf(w, "longnet_relay_dropped_total{reason=\"rate_limit\"} %d\n", st.DropRateLimit)
		fmt.Fprintf(w, "longnet_relay_dropped_total{reason=\"queue_full\"} %d\n", st.DropQueueFull)
		fmt.Fprintf(w, "longnet_relay_dropped_total{reason=\"policy\"} %d\n", st.DropPolicy)
		fmt.Fprintf(w, "# TYPE longnet_relay_call_me_maybe_total counter\nlongnet_relay_call_me_maybe_total %d\n", st.CallMeMaybes)
		fmt.Fprintf(w, "# TYPE longnet_relay_handshake_failures_total counter\nlongnet_relay_handshake_failures_total %d\n", st.HandshakeFails)
		fmt.Fprintf(w, "# TYPE longnet_relay_stun_requests_total counter\nlongnet_relay_stun_requests_total %d\n", st.STUNRequests)
		byNAT := map[string]int{}
		for _, ss := range st.Sessions {
			byNAT[ss.NAT]++
		}
		fmt.Fprintf(w, "# TYPE longnet_relay_sessions_by_nat gauge\n")
		for _, k := range []string{NATUnknown, NATOpen, NATEasy, NATHard} {
			name := k
			if name == NATUnknown {
				name = "unknown"
			}
			fmt.Fprintf(w, "longnet_relay_sessions_by_nat{nat=%q} %d\n", name, byNAT[k])
		}
	})
	mux.HandleFunc("/sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(s.Stats())
	})
	return mux
}

// stunAllow rate-limits STUN answers per source IP, so a flood from one
// source can't starve the nodes' rounds.
func (s *Server) stunAllow(ip netip.Addr) bool {
	s.stunMu.Lock()
	defer s.stunMu.Unlock()
	l := s.stunLim[ip]
	if l == nil {
		if len(s.stunLim) >= 4096 { // bounded (spoofed sources): start over
			clear(s.stunLim)
		}
		l = rate.NewLimiter(stunRate, stunBurst)
		s.stunLim[ip] = l
	}
	return l.Allow()
}
