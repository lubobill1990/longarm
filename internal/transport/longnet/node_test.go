package longnet_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
	"github.com/lubobill1990/longarm/internal/relay/relaytest"
	"github.com/lubobill1990/longarm/internal/transport"
	"github.com/lubobill1990/longarm/internal/transport/longnet"
)

// fastTiming makes path changes observable in a test's time budget.
var fastTiming = longnet.Timing{
	PunchInterval: 50 * time.Millisecond, PunchWindow: time.Second, Keepalive: 150 * time.Millisecond,
	DirectTimeout: 600 * time.Millisecond, BackoffMin: 500 * time.Millisecond, BackoffMax: 2 * time.Second,
}

// faultConn drops packets on the node's UDP socket, in both directions.
type faultConn struct {
	net.PacketConn
	relayAddr    atomic.Pointer[netip.AddrPort]
	blockDirect  atomic.Bool // everything that isn't relay traffic
	blockRelay   atomic.Bool // UDP to/from the relay
	droppedCount atomic.Int64
}

func (f *faultConn) drop(a net.Addr) bool {
	ra := f.relayAddr.Load()
	ap := a.(*net.UDPAddr).AddrPort()
	isRelay := ra != nil && netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()) == *ra
	if (isRelay && f.blockRelay.Load()) || (!isRelay && f.blockDirect.Load()) {
		f.droppedCount.Add(1)
		return true
	}
	return false
}

func (f *faultConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, a, err := f.PacketConn.ReadFrom(b)
		if err != nil || !f.drop(a) {
			return n, a, err
		}
	}
}

func (f *faultConn) WriteTo(b []byte, a net.Addr) (int, error) {
	if f.drop(a) {
		return len(b), nil
	}
	return f.PacketConn.WriteTo(b, a)
}

type world struct {
	t     testing.TB
	pki   *relaytest.PKI
	relay *relaytest.Relay
	rb    *pki.Bundle
}

func newWorld(t testing.TB) *world {
	p := relaytest.NewPKI(t)
	rb := p.Bundle(t, pki.RoleRelay, "relay1")
	return &world{t: t, pki: p, rb: rb, relay: relaytest.StartRelay(t, rb, "")}
}

// blockRelayUDP is a node() fault option: UDP to the relay is blocked from
// the start.
func blockRelayUDP(fc *faultConn) { fc.blockRelay.Store(true) }

func (w *world) node(role pki.Role, name string, mod func(*longnet.Options), faults ...func(*faultConn)) (*longnet.Node, *faultConn) {
	w.t.Helper()
	fc := &faultConn{}
	for _, f := range faults {
		f(fc)
	}
	ra := netip.MustParseAddrPort(w.relay.Addr)
	fc.relayAddr.Store(&ra)
	o := longnet.Options{
		Relay: w.relay.Addr, Bundle: w.pki.Bundle(w.t, role, name), Bind: "127.0.0.1:0",
		Timing: fastTiming, UDPTimeout: 700 * time.Millisecond, Logger: relaytest.Logger(),
		WrapPacketConn: func(pc net.PacketConn) net.PacketConn { fc.PacketConn = pc; return fc },
	}
	if mod != nil {
		mod(&o)
	}
	n, err := longnet.NewNode(o)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { n.Close() })
	// The relay session comes up in the background: a client dialing a
	// worker that has none yet fails with ErrPeerNotConnected at once.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := n.WaitRelay(ctx); err != nil {
		w.t.Fatal(err)
	}
	return n, fc
}

// echoWorker starts a worker node that echoes every connection.
func (w *world) echoWorker(name string, mod func(*longnet.Options), faults ...func(*faultConn)) (*longnet.Node, *faultConn) {
	n, fc := w.node(pki.RoleWorker, name, mod, faults...)
	ln, err := n.Listen()
	if err != nil {
		w.t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return n, fc
}

func dial(t *testing.T, n *longnet.Node, name string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, pi, err := n.Dial(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if pi.Transport != "longnet" {
		t.Fatalf("path %v", pi)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func echo(t *testing.T, c net.Conn, size int) {
	t.Helper()
	msg := make([]byte, size)
	rand.Read(msg)
	c.SetDeadline(time.Now().Add(20 * time.Second))
	defer c.SetDeadline(time.Time{})
	errc := make(chan error, 1)
	go func() { _, err := c.Write(msg); errc <- err }()
	got := make([]byte, size)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("echo mismatch")
	}
}

func pathOf(c net.Conn) transport.PathInfo {
	return c.(interface{ PathInfo() transport.PathInfo }).PathInfo()
}

func waitPath(t *testing.T, c net.Conn, kind transport.PathKind, within time.Duration) transport.PathInfo {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		pi := pathOf(c)
		if pi.Kind == kind {
			return pi
		}
		if time.Now().After(deadline) {
			t.Fatalf("path is %v, want %s within %v", pi, kind, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// (a) direct UDP works → the path becomes direct; data keeps flowing.
func TestDirectPath(t *testing.T) {
	w := newWorld(t)
	wn, _ := w.echoWorker("w1", nil)
	cn, _ := w.node(pki.RoleClient, "alice", nil)
	c := dial(t, cn, "w1")
	echo(t, c, 10)
	pi := waitPath(t, c, transport.PathDirect, 5*time.Second)
	// (RTT 0 on loopback is real on Windows: its monotonic clock ticks
	// coarsely)
	if !strings.HasPrefix(pi.Detail, "127.0.0.1:") || pi.RTT < 0 {
		t.Fatalf("direct path %v", pi)
	}
	d0, r0 := cn.Stats()
	echo(t, c, 1<<20)
	d1, r1 := cn.Stats()
	if d1-d0 < 500 || r1-r0 > 50 {
		t.Fatalf("after going direct: %d direct, %d relayed packets", d1-d0, r1-r0)
	}
	_ = wn
	t.Logf("path %v; relay stats %+v", pi, w.relay.Server.Stats())
}

// (b) direct UDP blocked both ways → stays relayed, traffic flows.
func TestDirectBlockedStaysRelayed(t *testing.T) {
	w := newWorld(t)
	_, wf := w.echoWorker("w1", nil)
	cn, cf := w.node(pki.RoleClient, "alice", nil)
	wf.blockDirect.Store(true)
	cf.blockDirect.Store(true)
	c := dial(t, cn, "w1")
	echo(t, c, 1<<20)
	time.Sleep(2 * fastTiming.PunchWindow)
	if pi := pathOf(c); pi.Kind != transport.PathRelay || !strings.HasPrefix(pi.Detail, "udp ") {
		t.Fatalf("path %v", pi)
	}
	echo(t, c, 1<<20)
	if d, _ := cn.Stats(); d != 0 {
		t.Fatalf("%d packets sent direct", d)
	}
	if cf.droppedCount.Load() == 0 {
		t.Fatal("no probes were attempted")
	}
}

// Forced relay (LONGARM_NET_DISABLE_DIRECT): no probes at all.
func TestDisableDirect(t *testing.T) {
	w := newWorld(t)
	_, wf := w.echoWorker("w1", func(o *longnet.Options) { o.DisableDirect = true })
	cn, cf := w.node(pki.RoleClient, "alice", func(o *longnet.Options) { o.DisableDirect = true })
	wf.blockDirect.Store(true) // count any direct packet as a drop
	cf.blockDirect.Store(true)
	c := dial(t, cn, "w1")
	echo(t, c, 256<<10)
	time.Sleep(500 * time.Millisecond)
	if pathOf(c).Kind != transport.PathRelay || wf.droppedCount.Load()+cf.droppedCount.Load() != 0 {
		t.Fatalf("path %v, %d direct packets", pathOf(c), wf.droppedCount.Load()+cf.droppedCount.Load())
	}
}

// (d) UDP to the relay blocked → TLS/TCP relay session; traffic flows (and
// direct still works when only the relay's UDP is blocked).
func TestRelayTCPFallback(t *testing.T) {
	w := newWorld(t)
	_, wf := w.echoWorker("w1", func(o *longnet.Options) { o.DisableDirect = true })
	cn, _ := w.node(pki.RoleClient, "alice", func(o *longnet.Options) { o.DisableDirect = true }, blockRelayUDP)
	_ = wf
	c := dial(t, cn, "w1")
	echo(t, c, 1<<20)
	if pi := pathOf(c); pi.Kind != transport.PathRelay || !strings.HasPrefix(pi.Detail, "tcp ") {
		t.Fatalf("path %v", pi)
	}
	tcp := 0
	for _, s := range w.relay.Server.Stats().Sessions {
		if s.Transport == "tcp" {
			tcp++
		}
	}
	if tcp != 1 {
		t.Fatalf("relay sessions: %+v", w.relay.Server.Stats().Sessions)
	}

	// Both sides blocked from the relay's UDP, direct allowed: relay over TCP,
	// then direct via the LAN candidate.
	w2 := newWorld(t)
	w2.echoWorker("w2", nil, blockRelayUDP)
	cn2, _ := w2.node(pki.RoleClient, "bob", nil, blockRelayUDP)
	c2 := dial(t, cn2, "w2")
	echo(t, c2, 1000)
	waitPath(t, c2, transport.PathDirect, 5*time.Second)
	echo(t, c2, 256<<10)
}

// The TCP session moves back to UDP once UDP works again.
func TestRelayUpgradeToUDP(t *testing.T) {
	w := newWorld(t)
	w.echoWorker("w1", func(o *longnet.Options) { o.DisableDirect = true })
	cn, cf := w.node(pki.RoleClient, "alice", func(o *longnet.Options) {
		o.DisableDirect = true
		o.UDPRetry = 300 * time.Millisecond
	}, blockRelayUDP)
	c := dial(t, cn, "w1")
	echo(t, c, 1000)
	if !strings.HasPrefix(pathOf(c).Detail, "tcp ") {
		t.Fatalf("path %v", pathOf(c))
	}
	cf.blockRelay.Store(false)
	deadline := time.Now().Add(10 * time.Second)
	for !strings.HasPrefix(pathOf(c).Detail, "udp ") {
		if time.Now().After(deadline) {
			t.Fatalf("still %v", pathOf(c))
		}
		time.Sleep(50 * time.Millisecond)
	}
	echo(t, c, 256<<10)
}

func TestDialUnknownWorkerFailsFast(t *testing.T) {
	w := newWorld(t)
	cn, _ := w.node(pki.RoleClient, "alice", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err := cn.Dial(ctx, "nobody")
	if !errors.Is(err, longnet.ErrPeerNotConnected) || time.Since(start) > 3*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestListenNeedsWorkerCert(t *testing.T) {
	w := newWorld(t)
	cn, _ := w.node(pki.RoleClient, "alice", nil)
	if _, err := cn.Listen(); err == nil {
		t.Fatal("client listened")
	}
}

// ---- a long gRPC server stream (longarm logs -f style) ----

type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) { return *(v.(*[]byte)), nil }
func (rawCodec) Unmarshal(data []byte, v any) error {
	*(v.(*[]byte)) = append([]byte(nil), data...)
	return nil
}
func (rawCodec) Name() string { return "raw" }

var tickerDesc = grpc.ServiceDesc{
	ServiceName: "test.Ticker",
	HandlerType: (*any)(nil),
	Streams: []grpc.StreamDesc{{StreamName: "Tick", ServerStreams: true, Handler: func(_ any, ss grpc.ServerStream) error {
		var req []byte
		if err := ss.RecvMsg(&req); err != nil {
			return err
		}
		pad := make([]byte, 1500)
		for i := uint64(0); ; i++ {
			b := append(binary.BigEndian.AppendUint64(nil, i), pad...)
			if err := ss.SendMsg(&b); err != nil {
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
	}}},
}

type tickStream struct {
	mu       sync.Mutex
	n        uint64
	last     time.Time
	err      error
	maxStall time.Duration
}

func (s *tickStream) count() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n, s.err
}

// startTicks runs the stream from a gRPC server on wn to a client on cn.
func startTicks(t *testing.T, wn, cn *longnet.Node, name string) (*tickStream, net.Conn) {
	t.Helper()
	ln, err := wn.Listen()
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}))
	gs.RegisterService(&tickerDesc, struct{}{})
	go gs.Serve(ln)
	t.Cleanup(gs.Stop)
	var raw atomic.Pointer[net.Conn]
	cc, err := grpc.NewClient("passthrough:///"+name,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			c, _, err := cn.Dial(ctx, name)
			if err == nil {
				raw.Store(&c)
			}
			return c, err
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cs, err := cc.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/test.Ticker/Tick", grpc.ForceCodec(rawCodec{}))
	if err != nil {
		t.Fatal(err)
	}
	req := []byte("go")
	if err := cs.SendMsg(&req); err != nil {
		t.Fatal(err)
	}
	cs.CloseSend()
	ts := &tickStream{last: time.Now()}
	go func() {
		for {
			var b []byte
			err := cs.RecvMsg(&b)
			ts.mu.Lock()
			if err != nil {
				if ctx.Err() == nil {
					ts.err = err
				}
				ts.mu.Unlock()
				return
			}
			if seq := binary.BigEndian.Uint64(b); seq != ts.n {
				ts.err = errors.New("sequence gap")
				ts.mu.Unlock()
				return
			}
			ts.n++
			now := time.Now()
			ts.maxStall = max(ts.maxStall, now.Sub(ts.last))
			ts.last = now
			ts.mu.Unlock()
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	for raw.Load() == nil || func() bool { n, _ := ts.count(); return n < 5 }() {
		if time.Now().After(deadline) {
			t.Fatal("stream didn't start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ts, *raw.Load()
}

// progresses checks that the stream is alive and advancing.
func progresses(t *testing.T, ts *tickStream, what string) {
	t.Helper()
	n0, err := ts.count()
	if err != nil {
		t.Fatalf("%s: stream broke: %v", what, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		n, err := ts.count()
		if err != nil {
			t.Fatalf("%s: stream broke: %v", what, err)
		}
		if n >= n0+20 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: stream stalled at %d", what, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// (c) direct dies mid-stream → falls back to the relay without breaking the
// gRPC stream, then recovers to direct.
func TestDirectFailoverKeepsStream(t *testing.T) {
	w := newWorld(t)
	wn, wf := w.node(pki.RoleWorker, "w1", nil)
	cn, cf := w.node(pki.RoleClient, "alice", nil)
	ts, c := startTicks(t, wn, cn, "w1")
	waitPath(t, c, transport.PathDirect, 5*time.Second)
	progresses(t, ts, "direct")

	wf.blockDirect.Store(true)
	cf.blockDirect.Store(true)
	waitPath(t, c, transport.PathRelay, 3*time.Second)
	progresses(t, ts, "relay after direct died")
	d0, _ := cn.Stats()
	progresses(t, ts, "relay")
	if d1, _ := cn.Stats(); d1 != d0 {
		t.Fatalf("still sending direct after fallback (%d packets)", d1-d0)
	}

	wf.blockDirect.Store(false)
	cf.blockDirect.Store(false)
	waitPath(t, c, transport.PathDirect, 3*(fastTiming.BackoffMax+fastTiming.PunchWindow))
	progresses(t, ts, "direct again")
	ts.mu.Lock()
	t.Logf("%d messages, longest stall %v", ts.n, ts.maxStall)
	ts.mu.Unlock()
}

// (e) relay restart → sessions reconnect; a relayed stream survives.
func TestRelayRestart(t *testing.T) {
	w := newWorld(t)
	wn, _ := w.node(pki.RoleWorker, "w1", func(o *longnet.Options) { o.DisableDirect = true })
	cn, _ := w.node(pki.RoleClient, "alice", func(o *longnet.Options) { o.DisableDirect = true })
	ts, c := startTicks(t, wn, cn, "w1")
	progresses(t, ts, "before restart")
	if pathOf(c).Kind != transport.PathRelay {
		t.Fatalf("path %v", pathOf(c))
	}
	addr := w.relay.Addr
	w.relay.Close()
	time.Sleep(time.Second)
	w.relay = relaytest.StartRelay(t, w.rb, addr)
	progresses(t, ts, "after restart")
	deadline := time.Now().Add(10 * time.Second)
	for len(w.relay.Server.Stats().Sessions) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("sessions: %+v", w.relay.Server.Stats().Sessions)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// New connections work too.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c2, _, err := cn.Dial(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
	ts.mu.Lock()
	t.Logf("longest stall %v", ts.maxStall)
	ts.mu.Unlock()
}

func TestTransportURL(t *testing.T) {
	p := relaytest.NewPKI(t)
	wb := p.Bundle(t, pki.RoleWorker, "w1")
	env := transport.Env{Bundle: wb}
	tr, err := transport.ParseListenEnv("longnet://relay.example:7800?name=w1&mode=tcp&direct=0", env)
	if err != nil || tr.Name() != "longnet" {
		t.Fatalf("%v %v", tr, err)
	}
	if _, err := transport.ParseListenEnv("longnet://relay.example:7800?name=other", env); err == nil {
		t.Fatal("name mismatch accepted")
	}
	if _, err := transport.ParseListenEnv("longnet://relay.example:7800?mode=carrier-pigeon", env); err == nil {
		t.Fatal("bad mode accepted")
	}
	if _, err := transport.ParseListenEnv("longnet:///?name=w1", env); err == nil {
		t.Fatal("missing relay accepted")
	}
	if _, err := transport.ParseListenEnv("longnet://relay.example:7800", transport.Env{}); err == nil {
		t.Fatal("missing bundle accepted")
	}
	o, _ := longnet.OptionsFromURL(mustURL("longnet://r:1?tcp=r:443&bind=:7702"))
	if o.Relay != "r:1" || o.RelayTCP != "r:443" || o.Bind != ":7702" || o.Mode != relay.ModeAuto || o.DisableDirect {
		t.Fatalf("%+v", o)
	}
	t.Setenv("LONGARM_NET_DISABLE_DIRECT", "1")
	t.Setenv("LONGARM_NET_RELAY_MODE", "tcp")
	o, _ = longnet.OptionsFromURL(mustURL("longnet://r:1"))
	if !o.DisableDirect || o.Mode != relay.ModeTCP {
		t.Fatalf("env knobs: %+v", o)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func TestDialFailsFastWhenRelayRejects(t *testing.T) {
	w := newWorld(t)
	other := relaytest.NewPKI(t) // a different CA: the relay refuses us
	n, err := longnet.NewNode(longnet.Options{Relay: w.relay.Addr, Bundle: other.Bundle(t, pki.RoleClient, "mallory"),
		Bind: "127.0.0.1:0", UDPTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err = n.Dial(ctx, "w1")
	if err == nil || ctx.Err() != nil || time.Since(start) > 10*time.Second || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}
