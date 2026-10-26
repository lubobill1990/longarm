package relay

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

// Handler receives what the relay delivers to a node. Calls come from the
// session's reader goroutines and must not block for long; pkt is only valid
// during the call.
type Handler interface {
	RelayData(src ID, pkt []byte)
	RelayCallMeMaybe(src ID, cm CallMeMaybe)
	RelayPeerGone(dst ID)
}

// Mode selects the relay transport.
type Mode int

const (
	ModeAuto Mode = iota // QUIC/UDP, falling back to TLS/TCP
	ModeUDP              // QUIC/UDP only
	ModeTCP              // TLS/TCP only
)

// ClientConfig configures Connect.
type ClientConfig struct {
	// UDPAddr is the relay's resolved UDP address; QUIC is a quic.Transport
	// on the node's socket (sharing it is what makes the relay-observed
	// reflexive address usable for hole punching). Both are needed for UDP.
	UDPAddr netip.AddrPort
	QUIC    *quic.Transport
	// TCPAddr is the relay's TLS/TCP address ("host:port").
	TCPAddr string
	TLS     *tls.Config // ClientTLS(bundle, ...)

	Instance   string
	Candidates []netip.AddrPort
	Mode       Mode
	UDPTimeout time.Duration // QUIC handshake budget before falling back to TCP (default 3 s)
	Logger     *slog.Logger
}

// Conn is a live session with the relay.
type Conn struct {
	kind      string
	remote    string
	self      ID
	reflexive netip.AddrPort
	stun      []string
	nodeInfo  bool // the relay understands FrameNodeInfo
	h         Handler
	log       *slog.Logger

	qc *quic.Conn
	st *quic.Stream
	tc *tls.Conn

	ctrl chan []byte
	data chan []byte // id-prefixed packets to send

	rtt atomic.Int64

	done      chan struct{}
	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
	wg        sync.WaitGroup
}

// Connect establishes a session (UDP first unless Mode says otherwise).
func Connect(ctx context.Context, cfg ClientConfig, h Handler) (*Conn, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.UDPTimeout == 0 {
		cfg.UDPTimeout = 3 * time.Second
	}
	hello := Hello{Version: 1, Instance: cfg.Instance, Candidates: cfg.Candidates}
	var errs []error
	if cfg.Mode != ModeTCP && cfg.QUIC != nil && cfg.UDPAddr.IsValid() {
		uctx, cancel := context.WithTimeout(ctx, cfg.UDPTimeout)
		c, err := connectQUIC(uctx, cfg, hello, h)
		cancel()
		if err == nil {
			return c, nil
		}
		errs = append(errs, fmt.Errorf("udp %s: %w", cfg.UDPAddr, err))
		if ctx.Err() != nil {
			return nil, errors.Join(errs...)
		}
	}
	if cfg.Mode != ModeUDP && cfg.TCPAddr != "" {
		c, err := connectTCP(ctx, cfg, hello, h)
		if err == nil {
			return c, nil
		}
		errs = append(errs, fmt.Errorf("tcp %s: %w", cfg.TCPAddr, err))
	}
	if len(errs) == 0 {
		return nil, errors.New("relay: no usable relay address")
	}
	return nil, errors.Join(errs...)
}

func newConn(kind, remote string, h Handler, log *slog.Logger) *Conn {
	return &Conn{kind: kind, remote: remote, h: h, log: log, ctrl: make(chan []byte, 64),
		data: make(chan []byte, 1024), done: make(chan struct{})}
}

func readWelcome(c *Conn, typ byte, p []byte) error {
	if typ == FrameClose {
		return fmt.Errorf("relay closed the session: %s", p)
	}
	if typ != FrameWelcome {
		return fmt.Errorf("expected welcome, got frame type %d", typ)
	}
	var w Welcome
	if err := json.Unmarshal(p, &w); err != nil {
		return err
	}
	id, err := ParseID(w.ID)
	if err != nil {
		return err
	}
	c.self, c.reflexive, c.stun, c.nodeInfo = id, w.Reflexive, w.STUN, w.NodeInfo || len(w.STUN) > 0
	return nil
}

func connectQUIC(ctx context.Context, cfg ClientConfig, hello Hello, h Handler) (*Conn, error) {
	start := time.Now()
	qc, err := cfg.QUIC.Dial(ctx, net.UDPAddrFromAddrPort(cfg.UDPAddr), cfg.TLS, QUICConfig())
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Conn, error) {
		qc.CloseWithError(0, "")
		return nil, err
	}
	st, err := qc.OpenStreamSync(ctx)
	if err != nil {
		return fail(err)
	}
	f, err := jsonFrame(FrameHello, hello)
	if err != nil {
		return fail(err)
	}
	if _, err := st.Write(f); err != nil {
		return fail(err)
	}
	if dl, ok := ctx.Deadline(); ok {
		st.SetReadDeadline(dl)
	}
	typ, p, err := ReadFrame(st, nil)
	if err != nil {
		return fail(err)
	}
	st.SetReadDeadline(time.Time{})
	c := newConn("udp", cfg.UDPAddr.String(), h, cfg.Logger)
	c.qc, c.st = qc, st
	if err := readWelcome(c, typ, p); err != nil {
		return fail(err)
	}
	c.rtt.Store(int64(time.Since(start) / 2)) // 2 round trips (handshake + hello)
	c.wg.Add(4)
	go c.quicCtrlReader()
	go c.quicDatagramReader()
	go c.quicDatagramWriter()
	go c.ctrlWriter(func(f []byte) error { _, err := st.Write(f); return err })
	go func() { // tie our lifetime to the QUIC connection's
		select {
		case <-qc.Context().Done():
			c.fail(context.Cause(qc.Context()))
		case <-c.done:
		}
	}()
	return c, nil
}

func connectTCP(ctx context.Context, cfg ClientConfig, hello Hello, h Handler) (*Conn, error) {
	start := time.Now()
	d := net.Dialer{KeepAlive: 15 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", cfg.TCPAddr)
	if err != nil {
		return nil, err
	}
	if tc, ok := raw.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	tc := tls.Client(raw, cfg.TLS)
	if dl, ok := ctx.Deadline(); ok {
		tc.SetDeadline(dl)
	} else {
		tc.SetDeadline(time.Now().Add(helloTimeout))
	}
	fail := func(err error) (*Conn, error) {
		raw.Close()
		return nil, err
	}
	if err := tc.HandshakeContext(ctx); err != nil {
		return fail(err)
	}
	f, err := jsonFrame(FrameHello, hello)
	if err != nil {
		return fail(err)
	}
	if _, err := tc.Write(f); err != nil {
		return fail(err)
	}
	br := bufio.NewReaderSize(tc, 32<<10)
	typ, p, err := ReadFrame(br, nil)
	if err != nil {
		return fail(err)
	}
	tc.SetDeadline(time.Time{})
	c := newConn("tcp", cfg.TCPAddr, h, cfg.Logger)
	c.tc = tc
	if err := readWelcome(c, typ, p); err != nil {
		return fail(err)
	}
	c.rtt.Store(int64(time.Since(start) / 3)) // TCP + TLS + hello
	c.wg.Add(3)
	go c.tcpReader(br)
	go c.tcpWriter()
	go c.tcpPinger()
	return c, nil
}

// Kind is "udp" or "tcp".
func (c *Conn) Kind() string { return c.kind }

// Remote is the relay address of this session.
func (c *Conn) Remote() string { return c.remote }

// Self is this node's id as assigned by the relay.
func (c *Conn) Self() ID { return c.self }

// Reflexive is this node's address as seen by the relay (UDP sessions only).
func (c *Conn) Reflexive() netip.AddrPort { return c.reflexive }

// STUN lists the relay's STUN endpoints from Welcome (":port" = relay IP).
func (c *Conn) STUN() []string { return c.stun }

// RTT to the relay (smoothed).
func (c *Conn) RTT() time.Duration {
	if c.qc != nil {
		if r := c.qc.ConnectionStats().SmoothedRTT; r > 0 {
			return r
		}
	}
	return time.Duration(c.rtt.Load())
}

// Done is closed when the session ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the session ended.
func (c *Conn) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

func (c *Conn) fail(err error) {
	c.closeOnce.Do(func() {
		if err == nil {
			err = net.ErrClosed
		}
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()
		close(c.done)
		if c.qc != nil {
			c.qc.CloseWithError(0, "")
		}
		if c.tc != nil {
			c.tc.NetConn().Close()
		}
	})
}

// Close ends the session and waits for its goroutines.
func (c *Conn) Close() error {
	c.fail(net.ErrClosed)
	c.wg.Wait()
	return nil
}

// ErrClosed is returned when sending on a finished session.
var ErrClosed = errors.New("relay: session closed")

// SendData sends a packet to dst through the relay. It never blocks for
// long: when the session is congested the packet is dropped (the end-to-end
// QUIC retransmits), like a full NIC queue would.
func (c *Conn) SendData(dst ID, pkt []byte) error {
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	if len(pkt) > MaxPacket {
		return errors.New("relay: packet too large")
	}
	b := AppendPacket(make([]byte, 0, 8+len(pkt)), dst, pkt)
	select {
	case c.data <- b:
	default:
	}
	return nil
}

// SendCallMeMaybe asks the relay to pass cm (our candidates and NAT type;
// no candidates = those of Hello/NodeInfo) to dst.
func (c *Conn) SendCallMeMaybe(dst ID, cm CallMeMaybe) error {
	cm.Peer, cm.Reflexive = dst.String(), netip.AddrPort{}
	f, err := jsonFrame(FrameCallMeMaybe, cm)
	if err != nil {
		return err
	}
	return c.sendCtrl(f)
}

// SendNodeInfo tells the relay our candidates and NAT type (only when the
// relay understands it: Welcome.NodeInfo or a STUN list; otherwise a no-op).
func (c *Conn) SendNodeInfo(ni NodeInfo) error {
	if !c.nodeInfo {
		return nil
	}
	f, err := jsonFrame(FrameNodeInfo, ni)
	if err != nil {
		return err
	}
	return c.sendCtrl(f)
}

func (c *Conn) sendCtrl(f []byte) error {
	select {
	case c.ctrl <- f:
		return nil
	case <-c.done:
		return ErrClosed
	default:
		return errors.New("relay: control queue full")
	}
}

func (c *Conn) handleFrame(typ byte, p []byte) error {
	switch typ {
	case FrameData:
		src, pkt, err := ParsePacket(p)
		if err != nil {
			return err
		}
		c.h.RelayData(src, pkt)
	case FrameCallMeMaybe:
		var cm CallMeMaybe
		if err := json.Unmarshal(p, &cm); err != nil {
			return err
		}
		src, err := ParseID(cm.Peer)
		if err != nil {
			return err
		}
		c.h.RelayCallMeMaybe(src, cm)
	case FramePeerGone:
		id, err := decodeID(p)
		if err != nil {
			return err
		}
		c.h.RelayPeerGone(id)
	case FramePing:
		f, _ := AppendFrame(nil, FramePong, p)
		c.sendCtrl(f)
	case FramePong:
		if len(p) == 8 {
			sent := time.Unix(0, int64(binary.BigEndian.Uint64(p)))
			if d := time.Since(sent); d >= 0 && d < time.Minute {
				old := time.Duration(c.rtt.Load())
				if old > 0 {
					d = (7*old + d) / 8
				}
				c.rtt.Store(int64(d))
			}
		}
	case FrameClose:
		return fmt.Errorf("relay closed the session: %s", p)
	default:
		// unknown frames are ignored for forward compatibility
	}
	return nil
}

func (c *Conn) quicCtrlReader() {
	defer c.wg.Done()
	buf := make([]byte, 4096)
	for {
		typ, p, err := ReadFrame(c.st, buf)
		if err != nil {
			c.fail(err)
			return
		}
		if err := c.handleFrame(typ, p); err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *Conn) quicDatagramReader() {
	defer c.wg.Done()
	ctx := c.qc.Context()
	for {
		b, err := c.qc.ReceiveDatagram(ctx)
		if err != nil {
			c.fail(err)
			return
		}
		src, pkt, err := ParsePacket(b)
		if err != nil {
			continue
		}
		c.h.RelayData(src, pkt)
	}
}

// quicDatagramWriter feeds QUIC datagrams from the queue (SendDatagram
// blocks while quic-go's own small queue is full; SendData must not).
func (c *Conn) quicDatagramWriter() {
	defer c.wg.Done()
	for {
		select {
		case b := <-c.data:
			if err := c.qc.SendDatagram(b); err != nil {
				var tl *quic.DatagramTooLargeError
				if errors.As(err, &tl) {
					continue
				}
				c.fail(err)
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *Conn) ctrlWriter(write func([]byte) error) {
	defer c.wg.Done()
	for {
		select {
		case f := <-c.ctrl:
			if err := write(f); err != nil {
				c.fail(err)
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *Conn) tcpReader(br *bufio.Reader) {
	defer c.wg.Done()
	buf := make([]byte, 4096)
	for {
		c.tc.SetReadDeadline(time.Now().Add(tcpReadTimeout))
		typ, p, err := ReadFrame(br, buf)
		if err != nil {
			c.fail(err)
			return
		}
		if err := c.handleFrame(typ, p); err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *Conn) tcpWriter() {
	defer c.wg.Done()
	bw := bufio.NewWriterSize(c.tc, 64<<10)
	var frame []byte
	write := func(f []byte) bool {
		c.tc.SetWriteDeadline(time.Now().Add(tcpReadTimeout))
		if _, err := bw.Write(f); err != nil {
			c.fail(err)
			return false
		}
		return true
	}
	for {
		select {
		case f := <-c.ctrl:
			if !write(f) {
				return
			}
		case b := <-c.data:
			frame, _ = AppendFrame(frame[:0], FrameData, b)
			if !write(frame) {
				return
			}
		case <-c.done:
			return
		}
		if len(c.ctrl) == 0 && len(c.data) == 0 {
			if err := bw.Flush(); err != nil {
				c.fail(err)
				return
			}
		}
	}
}

func (c *Conn) tcpPinger() {
	defer c.wg.Done()
	t := time.NewTicker(tcpPingInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			f, _ := AppendFrame(nil, FramePing, binary.BigEndian.AppendUint64(nil, uint64(time.Now().UnixNano())))
			c.sendCtrl(f)
		case <-c.done:
			return
		}
	}
}

// NewInstance returns a random per-process instance string.
func NewInstance() string {
	var b [12]byte
	rand.Read(b[:])
	return fmt.Sprintf("%x", b)
}
