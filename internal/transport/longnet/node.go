package longnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
	"github.com/lubobill1990/longarm/internal/transport"
)

// ALPN of the end-to-end QUIC connection between longarm and longarmd.
const ALPN = "longnet/1"

// Options configure a Node.
type Options struct {
	Relay    string // relay address "host:port" (UDP; also TCP unless RelayTCP is set)
	RelayTCP string // relay TLS/TCP address (default Relay)
	// RelayName, if set, must be a DNS name in the relay's certificate.
	RelayName string
	Bundle    *pki.Bundle
	// Bind is the local UDP address (default ":0").
	Bind string
	// Mode selects the relay transport (auto: UDP, falling back to TCP).
	Mode relay.Mode
	// DisableDirect keeps all traffic on the relay (debug knob,
	// LONGARM_NET_DISABLE_DIRECT=1).
	DisableDirect bool
	Timing        Timing
	// UDPTimeout is the QUIC budget per relay connect before TCP fallback
	// (default 3 s); UDPRetry is how often a TCP session tries to move back
	// to UDP (default 60 s).
	UDPTimeout time.Duration
	UDPRetry   time.Duration
	// STUN lists extra STUN servers ("host:port", e.g. a public one) queried
	// besides the relay's; STUNInterval is how often (default 25 s).
	STUN         []string
	STUNInterval time.Duration
	// WrapPacketConn wraps the node's UDP socket (tests: fault injection).
	WrapPacketConn func(net.PacketConn) net.PacketConn
	Logger         *slog.Logger
}

// peerAddr is the virtual address of a peer node as seen by the end-to-end
// QUIC: it never changes when the path does, so QUIC needs no migration.
type peerAddr struct{ id relay.ID }

func (a peerAddr) Network() string { return "longnet" }
func (a peerAddr) String() string  { return a.id.String() }

// Node is one longnet endpoint: a UDP socket shared by the relay session and
// the direct paths, a relay session, the path manager, and the end-to-end
// QUIC transport. Workers Listen; clients Dial.
type Node struct {
	opts     Options
	log      *slog.Logger
	instance string
	self     relay.ID

	sock     net.PacketConn
	port     uint16
	relayUDP atomic.Pointer[net.UDPAddr] // current relay UDP address (demux)

	relayPC *vconn // packets from the relay address → relay QUIC
	pathPC  *vconn // end-to-end QUIC packets, addressed by peerAddr
	relayTr *quic.Transport
	innerTr *quic.Transport

	mu       sync.Mutex
	pm       *pathManager
	rc       *relay.Conn
	relayErr error         // last connect error while rc == nil
	rcChange chan struct{} // closed when rc or relayErr changes
	waiters  map[relay.ID][]*dialWaiter
	nat      natState
	stunKick chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	listenOnce sync.Once
	ln         *listener
	lnErr      error

	directSent, relaySent atomic.Uint64
	pingsSent, pongsRecv  atomic.Uint64
}

type dialWaiter struct{ cancel context.CancelCauseFunc }

// ErrPeerNotConnected is the dial error when the relay reports that the
// target isn't connected to it.
var ErrPeerNotConnected = errors.New("longnet: peer is not connected to the relay")

// NewNode binds the UDP socket and starts the relay session in the
// background (Dial waits for it).
func NewNode(opts Options) (*Node, error) {
	if opts.Bundle == nil {
		return nil, errors.New("longnet: no TLS bundle")
	}
	if opts.Relay == "" && opts.RelayTCP == "" {
		return nil, errors.New("longnet: no relay address")
	}
	if opts.RelayTCP == "" {
		opts.RelayTCP = opts.Relay
	}
	if opts.Bind == "" {
		opts.Bind = ":0"
	}
	if opts.UDPTimeout == 0 {
		opts.UDPTimeout = 3 * time.Second
	}
	if opts.UDPRetry == 0 {
		opts.UDPRetry = time.Minute
	}
	if opts.STUNInterval <= 0 {
		opts.STUNInterval = 25 * time.Second
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	pc, err := net.ListenPacket("udp", opts.Bind)
	if err != nil {
		return nil, fmt.Errorf("longnet: bind %s: %w", opts.Bind, err)
	}
	port := uint16(0)
	if ua, ok := pc.LocalAddr().(*net.UDPAddr); ok {
		port = uint16(ua.Port)
	}
	if uc, ok := pc.(*net.UDPConn); ok { // what quic-go asks for; best effort
		uc.SetReadBuffer(7 << 20)
		uc.SetWriteBuffer(7 << 20)
	}
	if opts.WrapPacketConn != nil {
		pc = opts.WrapPacketConn(pc)
	}
	n := &Node{
		opts: opts, log: log, instance: relay.NewInstance(), sock: pc, port: port,
		pm:       newPathManager(opts.Timing, opts.DisableDirect),
		rcChange: make(chan struct{}),
		waiters:  map[relay.ID][]*dialWaiter{},
		stunKick: make(chan struct{}, 1),
	}
	n.self = relay.NodeID(opts.Bundle.Identity, n.instance)
	n.relayPC = newVConn(pc.LocalAddr(), 512, n.writeRelayUDP)
	n.pathPC = newVConn(peerAddr{n.self}, 2048, n.writePath)
	n.relayTr = &quic.Transport{Conn: n.relayPC}
	n.innerTr = &quic.Transport{Conn: n.pathPC}
	n.ctx, n.cancel = context.WithCancel(context.Background())
	n.wg.Add(3)
	go n.readLoop()
	go n.relayLoop()
	go n.tickLoop()
	if !opts.DisableDirect {
		n.wg.Add(1)
		go n.stunLoop()
	}
	return n, nil
}

// ID is this node's routing id.
func (n *Node) ID() relay.ID { return n.self }

// LocalAddr is the node's UDP socket address.
func (n *Node) LocalAddr() net.Addr { return n.sock.LocalAddr() }

// Close shuts the node down.
func (n *Node) Close() error {
	n.cancel()
	n.mu.Lock()
	ln := n.ln
	n.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	n.innerTr.Close()
	n.mu.Lock()
	rc := n.rc
	n.mu.Unlock()
	if rc != nil {
		rc.Close()
	}
	n.relayTr.Close()
	n.relayPC.Close()
	n.pathPC.Close()
	err := n.sock.Close()
	n.wg.Wait()
	return err
}

// ---- socket I/O ----

func udpAddrPort(a net.Addr) netip.AddrPort {
	if ua, ok := a.(*net.UDPAddr); ok {
		ap := ua.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	ap, _ := netip.ParseAddrPort(a.String())
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

func (n *Node) readLoop() {
	defer n.wg.Done()
	var errs int
	for {
		bp := getBuf()
		sz, from, err := n.sock.ReadFrom(*bp)
		if err != nil {
			putBuf(bp)
			if n.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// Windows reports ICMP errors from earlier sends here; skip them.
			if errs++; errs > 100 {
				time.Sleep(10 * time.Millisecond)
			}
			continue
		}
		errs = 0
		b := (*bp)[:sz]
		if relay.IsSTUN(b) { // never a QUIC or LGN packet (top bits clear)
			n.handleSTUN(b, udpAddrPort(from))
			putBuf(bp)
			continue
		}
		if ra := n.relayUDP.Load(); ra != nil && udpAddrPort(from) == udpAddrPort(ra) {
			n.relayPC.push(packet{buf: bp, data: b, addr: ra})
			continue
		}
		typ, src, dst, rest, ok := parseHeader(b)
		if !ok || dst != n.self || src == n.self {
			putBuf(bp)
			continue
		}
		switch typ {
		case typeData:
			n.pathPC.push(packet{buf: bp, data: rest, addr: peerAddr{src}})
		case typePing, typePong:
			n.handleDisco(b, udpAddrPort(from))
			putBuf(bp)
		default:
			putBuf(bp)
		}
	}
}

func (n *Node) writeRelayUDP(p []byte, addr net.Addr) (int, error) {
	ra := n.relayUDP.Load()
	if ra == nil || udpAddrPort(addr) != udpAddrPort(ra) {
		return len(p), nil // stale relay address: drop
	}
	return n.sock.WriteTo(p, ra)
}

// writePath sends an end-to-end QUIC packet to a peer on its current path.
func (n *Node) writePath(p []byte, addr net.Addr) (int, error) {
	pa, ok := addr.(peerAddr)
	if !ok {
		return 0, fmt.Errorf("longnet: bad peer address %T", addr)
	}
	n.mu.Lock()
	dst, direct := n.pm.route(pa.id)
	rc := n.rc
	n.mu.Unlock()
	if direct {
		bp := getBuf()
		b := append(appendHeader((*bp)[:0], typeData, n.self, pa.id), p...)
		_, err := n.sock.WriteTo(b, net.UDPAddrFromAddrPort(dst))
		putBuf(bp)
		n.directSent.Add(1)
		if err == nil {
			return len(p), nil
		}
		// fall through: try the relay for this packet
	}
	if rc != nil {
		if err := rc.SendData(pa.id, p); err == nil {
			n.relaySent.Add(1)
		}
	}
	return len(p), nil // lossy link semantics: QUIC retransmits
}

func (n *Node) sendDisco(m discoMsg, k discoKey, to netip.AddrPort) {
	if m.typ == typePing {
		n.pingsSent.Add(1)
	}
	n.sock.WriteTo(m.seal(k), net.UDPAddrFromAddrPort(to))
}

func (n *Node) handleDisco(b []byte, from netip.AddrPort) {
	now := time.Now()
	n.mu.Lock()
	m, err := openDisco(b, n.pm.keys)
	if err != nil {
		n.mu.Unlock()
		return
	}
	var acts []action
	var pong *discoMsg
	var key discoKey
	switch m.typ {
	case typePing:
		if n.opts.DisableDirect || !n.pm.acceptPing(m.src, m.keyID, m.seq) {
			n.mu.Unlock()
			return
		}
		for _, k := range n.pm.keys(m.src) {
			if k.id == m.keyID {
				key = k
			}
		}
		pong = &discoMsg{typ: typePong, src: n.self, dst: m.src, tx: m.tx, observed: from}
		acts = n.pm.onPing(m.src, from, now)
	case typePong:
		acts = n.pm.onPong(m.src, m.tx, now)
	}
	n.mu.Unlock()
	if m.typ == typePong {
		n.pongsRecv.Add(1)
		if m.observed.IsValid() {
			n.notePeerObserved(m.observed)
		}
	}
	if pong != nil {
		n.sendDisco(*pong, key, from)
	}
	n.run(acts)
}

// run executes path-manager actions (without holding n.mu).
func (n *Node) run(acts []action) {
	for _, a := range acts {
		switch a.kind {
		case actPing:
			n.sendDisco(discoMsg{typ: typePing, src: n.self, dst: a.peer, tx: a.tx, seq: a.seq}, a.key, a.addr)
		case actCallMeMaybe:
			n.mu.Lock()
			rc := n.rc
			cm := relay.CallMeMaybe{Candidates: n.nat.cands, NAT: n.nat.typ, Reply: a.reply}
			n.mu.Unlock()
			if rc != nil {
				// No candidates yet (before the first STUN round): the relay
				// uses those of our Hello.
				rc.SendCallMeMaybe(a.peer, cm)
			}
		case actPathChange:
			if a.direct {
				n.log.Info("longnet: direct path up", "peer", a.peer, "addr", a.addr, "rtt", a.rtt)
			} else {
				n.log.Info("longnet: direct path lost, using relay", "peer", a.peer, "addr", a.addr)
			}
		case actPunchFailed:
			s := a.stats
			if h := PunchFailedHook; h != nil {
				h(a.peer.String(), s.pingsSent, s.pingsRecv)
			}
			n.log.Info("longnet: punch attempt failed, staying on the relay", "peer", a.peer, "candidates", s.cands,
				"pings_sent", s.pingsSent, "pings_received", s.pingsRecv, "learned", s.learned,
				"nat", natName(s.selfNAT), "peer_nat", natName(s.peerNAT))
		}
	}
}

func (n *Node) tickLoop() {
	defer n.wg.Done()
	iv := min(n.pm.t.PunchInterval/2, 100*time.Millisecond)
	t := time.NewTicker(max(iv, 10*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case now := <-t.C:
			n.mu.Lock()
			acts := n.pm.tick(now)
			n.mu.Unlock()
			n.run(acts)
		}
	}
}

// ---- relay session ----

// RelayData implements relay.Handler.
func (n *Node) RelayData(src relay.ID, pkt []byte) {
	bp := getBuf()
	if len(pkt) > len(*bp) {
		putBuf(bp)
		return
	}
	b := append((*bp)[:0], pkt...)
	n.pathPC.push(packet{buf: bp, data: b, addr: peerAddr{src}})
}

// RelayCallMeMaybe implements relay.Handler.
func (n *Node) RelayCallMeMaybe(src relay.ID, cm relay.CallMeMaybe) {
	cands := cm.Candidates
	// The peer's address as the relay saw it: when that is private while
	// the relay is on a public address, the relay sits behind its platform's
	// source NAT (see usableSTUN) and the address is the platform's, not the
	// peer's. Newer relays leave it out themselves once they notice.
	if refl := cm.Reflexive; refl.IsValid() && !relay.PublicAddr(refl.Addr()) {
		if ra := n.relayUDP.Load(); ra != nil && relay.PublicAddr(udpAddrPort(ra).Addr()) {
			cands = slices.DeleteFunc(slices.Clone(cands), func(a netip.AddrPort) bool { return a == refl })
		}
	}
	n.mu.Lock()
	acts := n.pm.onCallMeMaybe(src, cands, cm.Reply, cm.NAT, time.Now())
	n.mu.Unlock()
	n.run(acts)
}

// RelayPeerGone implements relay.Handler: fail pending dials fast.
func (n *Node) RelayPeerGone(dst relay.ID) {
	n.mu.Lock()
	ws := n.waiters[dst]
	delete(n.waiters, dst)
	n.mu.Unlock()
	for _, w := range ws {
		w.cancel(ErrPeerNotConnected)
	}
}

func (n *Node) setRelay(rc *relay.Conn) { n.setRelayState(rc, nil) }

func (n *Node) setRelayState(rc *relay.Conn, err error) {
	n.mu.Lock()
	n.rc, n.relayErr = rc, err
	close(n.rcChange)
	n.rcChange = make(chan struct{})
	n.mu.Unlock()
}

// relayConn returns the current session (or the last connect error) and a
// channel closed on change.
func (n *Node) relayConn() (*relay.Conn, <-chan struct{}, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.rc, n.rcChange, n.relayErr
}

// Overlay ranges never advertised: Tailscale's CGNAT and ULA prefixes
// (longnet is the independent second leg next to Tailscale; going "direct"
// through the tailnet would tie the two together).
var overlayPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// skipLocalIP reports whether an interface address is unusable as a
// candidate.
func skipLocalIP(ip netip.Addr) bool {
	if ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() || (ip.Is6() && ip.IsLoopback()) {
		return true
	}
	for _, p := range overlayPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// localCandidates lists the socket's LAN addresses (and loopback, which lets
// two nodes on one host go direct).
func (n *Node) localCandidates() []netip.AddrPort {
	if ua, ok := n.sock.LocalAddr().(*net.UDPAddr); ok && ua.IP != nil && !ua.IP.IsUnspecified() {
		ap := ua.AddrPort()
		return []netip.AddrPort{netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())}
	}
	var out []netip.AddrPort
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			if ip = ip.Unmap(); skipLocalIP(ip) {
				continue
			}
			out = append(out, netip.AddrPortFrom(ip, n.port))
		}
	}
	// IPv4 first, loopback last; at most 8.
	var v4, v6, lo []netip.AddrPort
	for _, a := range out {
		switch {
		case a.Addr().IsLoopback():
			lo = append(lo, a)
		case a.Addr().Is4():
			v4 = append(v4, a)
		default:
			v6 = append(v6, a)
		}
	}
	out = append(append(v4, v6...), lo...)
	if len(out) > 8 {
		out = append(out[:7], lo...)[:8]
	}
	return out
}

func (n *Node) connectRelay(mode relay.Mode) (*relay.Conn, error) {
	cfg := relay.ClientConfig{
		TCPAddr: n.opts.RelayTCP, TLS: relay.ClientTLS(n.opts.Bundle, n.opts.RelayName),
		Instance: n.instance, Candidates: n.localCandidates(), Mode: mode, UDPTimeout: n.opts.UDPTimeout,
		Logger: n.log,
	}
	if mode != relay.ModeTCP && n.opts.Relay != "" {
		if ua, err := net.ResolveUDPAddr("udp", n.opts.Relay); err == nil {
			ap := ua.AddrPort()
			ua = net.UDPAddrFromAddrPort(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()))
			n.relayUDP.Store(ua)
			cfg.UDPAddr, cfg.QUIC = udpAddrPort(ua), n.relayTr
		} else if mode == relay.ModeUDP {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(n.ctx, n.opts.UDPTimeout+10*time.Second)
	defer cancel()
	rc, err := relay.Connect(ctx, cfg, n)
	if err != nil {
		return nil, err
	}
	if rc.Self() != n.self {
		rc.Close()
		return nil, fmt.Errorf("longnet: relay assigned id %s, expected %s", rc.Self(), n.self)
	}
	return rc, nil
}

func (n *Node) relayLoop() {
	defer n.wg.Done()
	backoff := 200 * time.Millisecond
	for n.ctx.Err() == nil {
		rc, err := n.connectRelay(n.opts.Mode)
		if err != nil {
			if n.ctx.Err() != nil {
				return
			}
			n.log.Warn("longnet: relay connect failed", "relay", n.opts.Relay, "err", err, "retry_in", backoff)
			n.setRelayState(nil, err)
			select {
			case <-n.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, 5*time.Second)
			continue
		}
		backoff = 200 * time.Millisecond
		n.log.Info("longnet: relay session up", "relay", rc.Remote(), "transport", rc.Kind(), "id", rc.Self(),
			"reflexive", rc.Reflexive())
		n.setRelay(rc)
		n.reannounce()
		n.kickSTUN()
		var upgrade <-chan time.Time
		var upTimer *time.Ticker
		if rc.Kind() == "tcp" && n.opts.Mode == relay.ModeAuto {
			upTimer = time.NewTicker(n.opts.UDPRetry)
			upgrade = upTimer.C
		}
	session:
		for {
			select {
			case <-n.ctx.Done():
				break session
			case <-rc.Done():
				break session
			case <-upgrade:
				nrc, err := n.connectRelay(relay.ModeUDP)
				if err != nil {
					continue
				}
				n.log.Info("longnet: relay session moved to UDP", "relay", nrc.Remote())
				old := rc
				rc = nrc
				n.setRelay(rc)
				old.Close()
				upTimer.Stop()
				upgrade = nil
				n.reannounce()
				n.kickSTUN()
			}
		}
		if upTimer != nil {
			upTimer.Stop()
		}
		n.setRelay(nil)
		rc.Close()
		if n.ctx.Err() == nil {
			n.log.Warn("longnet: relay session lost; reconnecting", "err", rc.Err())
		}
	}
}

// reannounce sends fresh candidates to every peer we have a connection with
// after a relay (re)connect (our reflexive address may have changed).
func (n *Node) reannounce() {
	n.mu.Lock()
	var acts []action
	for id, p := range n.pm.peers {
		if len(p.keys) > 0 && !p.direct {
			acts = append(acts, action{kind: actCallMeMaybe, peer: id, reply: true})
		}
	}
	n.mu.Unlock()
	n.run(acts)
}

// waitRelay blocks until a relay session exists, or fails with the error
// of the last connect attempt (the node keeps retrying in the background).
// WaitRelay blocks until the node has a session with its relay (a worker
// is then reachable by name; a client dialing a worker that has none yet
// gets ErrPeerNotConnected).
func (n *Node) WaitRelay(ctx context.Context) error {
	_, err := n.waitRelay(ctx)
	return err
}

func (n *Node) waitRelay(ctx context.Context) (*relay.Conn, error) {
	for {
		rc, ch, err := n.relayConn()
		if rc != nil {
			return rc, nil
		}
		if err != nil {
			return nil, fmt.Errorf("longnet: relay %s: %w", n.opts.Relay, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("longnet: no relay session with %s: %w", n.opts.Relay, ctx.Err())
		case <-n.ctx.Done():
			return nil, net.ErrClosed
		case <-ch:
		}
	}
}

// ---- end-to-end QUIC ----

// innerIdleTimeout: an end-to-end connection survives a relay outage
// (restart, hub redeploy) this long; LONGARM_NET_IDLE_TIMEOUT overrides it
// (tests, at least 3 s; the keepalive is a third of it, at most 10 s).
func innerIdleTimeout() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("LONGARM_NET_IDLE_TIMEOUT")); err == nil && d >= 3*time.Second {
		return d
	}
	return 45 * time.Second
}

func innerQUICConfig() *quic.Config {
	idle := innerIdleTimeout()
	return &quic.Config{
		InitialPacketSize:       1200, // fits a relay datagram; never grows (no PMTUD)
		DisablePathMTUDiscovery: true,
		HandshakeIdleTimeout:    10 * time.Second,
		MaxIdleTimeout:          idle,
		KeepAlivePeriod:         min(10*time.Second, idle/3),
		MaxIncomingStreams:      4,
		MaxIncomingUniStreams:   -1,
	}
}

func withALPN(c *tls.Config) *tls.Config {
	c = c.Clone()
	c.NextProtos = []string{ALPN}
	return c
}

// track registers the disco key of an established connection for its
// lifetime.
func (n *Node) track(qc *quic.Conn) error {
	pa, ok := qc.RemoteAddr().(peerAddr)
	if !ok {
		return fmt.Errorf("longnet: unexpected remote address %T", qc.RemoteAddr())
	}
	cs := qc.ConnectionState().TLS
	km, err := cs.ExportKeyingMaterial(discoExporterLabel, nil, 32)
	if err != nil {
		return err
	}
	k := newDiscoKey(km)
	n.mu.Lock()
	acts := n.pm.addKey(pa.id, k, time.Now())
	n.mu.Unlock()
	n.run(acts)
	go func() { // not in n.wg: Dial may run concurrently with Close; this exits with n.ctx
		select {
		case <-qc.Context().Done():
		case <-n.ctx.Done():
		}
		n.mu.Lock()
		n.pm.removeKey(pa.id, k.id)
		n.mu.Unlock()
	}()
	return nil
}

// pathInfo describes how packets to peer currently travel.
func (n *Node) pathInfo(peer relay.ID, qc *quic.Conn) transport.PathInfo {
	n.mu.Lock()
	addr, rtt, direct := n.pm.directInfo(peer)
	rc := n.rc
	selfNAT, peerNAT := n.nat.typ, n.pm.peerNAT(peer)
	n.mu.Unlock()
	// "nat <ours>/<peer's>" when known, e.g. "nat easy/hard".
	natInfo := ""
	if selfNAT != relay.NATUnknown || peerNAT != relay.NATUnknown {
		short := func(s string) string {
			if s == relay.NATUnknown {
				return "?"
			}
			return s
		}
		natInfo = ", nat " + short(selfNAT) + "/" + short(peerNAT)
	}
	pi := transport.PathInfo{Transport: "longnet"}
	if direct {
		pi.Kind, pi.RTT, pi.Detail = transport.PathDirect, rtt, addr.String()+natInfo
		return pi
	}
	pi.Kind = transport.PathRelay
	if qc != nil {
		pi.RTT = qc.ConnectionStats().SmoothedRTT
	}
	if rc != nil {
		pi.Detail = rc.Kind() + " " + rc.Remote() + natInfo
	} else {
		pi.Detail = "relay down"
	}
	return pi
}

// Stats counts packets sent per path (end-to-end QUIC packets).
func (n *Node) Stats() (direct, relayed uint64) { return n.directSent.Load(), n.relaySent.Load() }

// DiscoStats counts disco pings sent and pongs received (all peers).
func (n *Node) DiscoStats() (pingsSent, pongsRecv uint64) {
	return n.pingsSent.Load(), n.pongsRecv.Load()
}

// Dial connects to the worker named name (its certificate name). The
// connection goes through the relay at once; hole punching runs in the
// background and moves it to a direct path when possible.
func (n *Node) Dial(ctx context.Context, name string) (net.Conn, transport.PathInfo, error) {
	if _, err := n.waitRelay(ctx); err != nil {
		return nil, transport.PathInfo{}, err
	}
	peer := relay.WorkerID(name)
	dctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	waiter := &dialWaiter{cancel: cancel}
	n.mu.Lock()
	n.waiters[peer] = append(n.waiters[peer], waiter)
	n.mu.Unlock()
	defer func() { // drop our waiter
		n.mu.Lock()
		ws := n.waiters[peer]
		for i := range ws {
			if ws[i] == waiter {
				ws = append(ws[:i:i], ws[i+1:]...)
				break
			}
		}
		if len(ws) == 0 {
			delete(n.waiters, peer)
		} else {
			n.waiters[peer] = ws
		}
		n.mu.Unlock()
	}()
	tlsConf := withALPN(n.opts.Bundle.ClientConfig(name))
	qc, err := n.innerTr.Dial(dctx, peerAddr{peer}, tlsConf, innerQUICConfig())
	if err != nil {
		if errors.Is(context.Cause(dctx), ErrPeerNotConnected) {
			return nil, transport.PathInfo{}, fmt.Errorf("worker %q: %w", name, ErrPeerNotConnected)
		}
		return nil, transport.PathInfo{}, err
	}
	if err := n.track(qc); err != nil {
		qc.CloseWithError(1, "")
		return nil, transport.PathInfo{}, err
	}
	st, err := qc.OpenStreamSync(dctx)
	if err != nil {
		qc.CloseWithError(1, "")
		return nil, transport.PathInfo{}, err
	}
	c := &streamConn{Stream: st, qc: qc, node: n, peer: peer}
	return c, n.pathInfo(peer, qc), nil
}

// Listen accepts end-to-end connections (workers). Only one listener per
// node; closing it doesn't close the node.
func (n *Node) Listen() (net.Listener, error) {
	n.listenOnce.Do(func() {
		if n.opts.Bundle.Identity.Role != pki.RoleWorker {
			n.lnErr = fmt.Errorf("longnet: listening needs a worker certificate, got %s", n.opts.Bundle.Identity)
			return
		}
		sc, err := n.opts.Bundle.ServerConfig()
		if err != nil {
			n.lnErr = err
			return
		}
		qc := innerQUICConfig()
		qc.MaxIncomingStreams = 1
		ql, err := n.innerTr.Listen(withALPN(sc), qc)
		if err != nil {
			n.lnErr = err
			return
		}
		ln := &listener{n: n, ql: ql, conns: make(chan net.Conn), done: make(chan struct{})}
		n.mu.Lock()
		n.ln = ln
		n.mu.Unlock()
		n.wg.Add(1)
		go ln.acceptLoop()
	})
	if n.lnErr != nil {
		return nil, n.lnErr
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ln, nil
}

// Addr is the listener address: "<relay>?name=<worker>".
type Addr struct{ Relay, Name string }

func (a Addr) Network() string { return "longnet" }
func (a Addr) String() string  { return a.Relay + "?name=" + a.Name }

type listener struct {
	n         *Node
	ql        *quic.Listener
	conns     chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func (l *listener) acceptLoop() {
	defer l.n.wg.Done()
	for {
		qc, err := l.ql.Accept(l.n.ctx)
		if err != nil {
			l.Close()
			return
		}
		l.n.wg.Add(1) // safe: acceptLoop itself is counted, so the WaitGroup isn't at zero
		go func() {
			defer l.n.wg.Done()
			if err := l.n.track(qc); err != nil {
				qc.CloseWithError(1, "")
				return
			}
			ctx, cancel := context.WithTimeout(l.n.ctx, 10*time.Second)
			st, err := qc.AcceptStream(ctx)
			cancel()
			if err != nil {
				qc.CloseWithError(1, "no stream")
				return
			}
			c := &streamConn{Stream: st, qc: qc, node: l.n, peer: qc.RemoteAddr().(peerAddr).id}
			select {
			case l.conns <- c:
			case <-l.done:
				c.Close()
			}
		}()
	}
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *listener) Close() error {
	l.closeOnce.Do(func() {
		close(l.done)
		l.ql.Close()
	})
	return nil
}

func (l *listener) Addr() net.Addr {
	return Addr{Relay: l.n.opts.Relay, Name: l.n.opts.Bundle.Identity.Name}
}

// streamConn is the net.Conn handed to gRPC: one QUIC stream on its own
// connection.
type streamConn struct {
	*quic.Stream
	qc   *quic.Conn
	node *Node
	peer relay.ID
	once sync.Once
}

func (c *streamConn) LocalAddr() net.Addr  { return c.qc.LocalAddr() }
func (c *streamConn) RemoteAddr() net.Addr { return c.qc.RemoteAddr() }

func (c *streamConn) Close() error {
	c.once.Do(func() {
		// Abort rather than Stream.Close (FIN): with quic-go v0.64, a Write
		// blocked on flow control when Close is called is never woken up by
		// the connection's shutdown (the writer waits forever). CancelWrite
		// wakes it; closing the connection then frees everything.
		c.Stream.CancelWrite(0)
		c.Stream.CancelRead(0)
		c.qc.CloseWithError(0, "")
	})
	return nil
}

// CloseWrite half-closes the stream (FIN); the read side stays open.
func (c *streamConn) CloseWrite() error { return c.Stream.Close() }

// PathInfo is the live path of this connection (client.Conn.Path uses it).
func (c *streamConn) PathInfo() transport.PathInfo { return c.node.pathInfo(c.peer, c.qc) }

// PunchFailedHook, if set, is called (from the node's event loop; it must
// not block) when a hole-punching attempt ended without a direct path. longarm
// and longarmd use it to point at a missing host firewall rule.
var PunchFailedHook func(peer string, pingsSent, pingsReceived int)
