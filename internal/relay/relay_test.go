package relay_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
	"github.com/lubobill1990/longarm/internal/relay/relaytest"
)

func TestFrameCodec(t *testing.T) {
	var buf bytes.Buffer
	payloads := [][]byte{nil, []byte("x"), bytes.Repeat([]byte{7}, relay.MaxFramePayload)}
	for i, p := range payloads {
		f, err := relay.AppendFrame(nil, byte(i+1), p)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(f)
	}
	if _, err := relay.AppendFrame(nil, 1, make([]byte, relay.MaxFramePayload+1)); err != relay.ErrFrameTooLarge {
		t.Fatalf("oversize frame: %v", err)
	}
	for i, p := range payloads {
		typ, got, err := relay.ReadFrame(&buf, make([]byte, 16))
		if err != nil || typ != byte(i+1) || !bytes.Equal(got, p) {
			t.Fatalf("frame %d: typ=%d len=%d err=%v", i, typ, len(got), err)
		}
	}
	if _, _, err := relay.ReadFrame(&buf, nil); err != io.EOF {
		t.Fatalf("empty: %v", err)
	}
	// truncated payload
	f, _ := relay.AppendFrame(nil, 3, []byte("hello"))
	if _, _, err := relay.ReadFrame(bytes.NewReader(f[:5]), nil); err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated: %v", err)
	}

	pkt := relay.AppendPacket(nil, 0x0102030405060708, []byte("data"))
	id, p, err := relay.ParsePacket(pkt)
	if err != nil || id != 0x0102030405060708 || string(p) != "data" {
		t.Fatalf("packet: %v %v %q", err, id, p)
	}
	if _, _, err := relay.ParsePacket(pkt[:7]); err == nil {
		t.Fatal("short packet accepted")
	}
	if _, _, err := relay.ParsePacket(make([]byte, 8+relay.MaxPacket+1)); err == nil {
		t.Fatal("oversize packet accepted")
	}
}

func TestIDs(t *testing.T) {
	w := relay.WorkerID("w1")
	if w != relay.NodeID(pki.Identity{Role: pki.RoleWorker, Name: "w1"}, "whatever") {
		t.Fatal("worker id must ignore the instance")
	}
	if w == relay.WorkerID("w2") {
		t.Fatal("collision")
	}
	c1 := relay.NodeID(pki.Identity{Role: pki.RoleClient, Name: "w1"}, "a")
	c2 := relay.NodeID(pki.Identity{Role: pki.RoleClient, Name: "w1"}, "b")
	a1 := relay.NodeID(pki.Identity{Role: pki.RoleAdmin, Name: "w1"}, "a")
	if c1 == c2 || c1 == w || c1 == a1 {
		t.Fatal("ids must differ by instance and role")
	}
	got, err := relay.ParseID(c1.String())
	if err != nil || got != c1 {
		t.Fatal("ParseID round trip")
	}
	if _, err := relay.ParseID("xyz"); err == nil {
		t.Fatal("bad id accepted")
	}
}

// recorder is a relay.Handler collecting what arrives.
type recorder struct {
	mu   sync.Mutex
	data map[relay.ID][][]byte
	cmms []relay.CallMeMaybe
	gone []relay.ID
	ch   chan struct{}
}

func newRecorder() *recorder {
	return &recorder{data: map[relay.ID][][]byte{}, ch: make(chan struct{}, 1000)}
}

func (r *recorder) RelayData(src relay.ID, pkt []byte) {
	r.mu.Lock()
	r.data[src] = append(r.data[src], append([]byte(nil), pkt...))
	r.mu.Unlock()
	r.ch <- struct{}{}
}

func (r *recorder) RelayCallMeMaybe(src relay.ID, cm relay.CallMeMaybe) {
	r.mu.Lock()
	r.cmms = append(r.cmms, cm)
	r.mu.Unlock()
	r.ch <- struct{}{}
}

func (r *recorder) RelayPeerGone(dst relay.ID) {
	r.mu.Lock()
	r.gone = append(r.gone, dst)
	r.mu.Unlock()
	r.ch <- struct{}{}
}

func (r *recorder) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		r.mu.Lock()
		ok := cond()
		r.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-r.ch:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timeout waiting for %s", what)
		}
	}
}

type env struct {
	p     *relaytest.PKI
	relay *relaytest.Relay
}

func newEnv(t *testing.T) *env {
	p := relaytest.NewPKI(t)
	return &env{p: p, relay: relaytest.StartRelay(t, p.Bundle(t, pki.RoleRelay, "relay1"), "")}
}

func (e *env) connect(t *testing.T, b *pki.Bundle, mode relay.Mode, h relay.Handler) *relay.Conn {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tr := &quic.Transport{Conn: pc}
	t.Cleanup(func() { tr.Close(); pc.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := relay.Connect(ctx, relay.ClientConfig{
		UDPAddr: netip.MustParseAddrPort(e.relay.Addr), QUIC: tr, TCPAddr: e.relay.TCPAdr,
		TLS: relay.ClientTLS(b, ""), Instance: relay.NewInstance(), Mode: mode,
		Candidates: []netip.AddrPort{netip.MustParseAddrPort("10.1.2.3:4567")},
	}, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRelayForwarding(t *testing.T) {
	for _, tc := range []struct {
		name         string
		worker, user relay.Mode
	}{
		{"udp-udp", relay.ModeUDP, relay.ModeUDP},
		{"tcp-tcp", relay.ModeTCP, relay.ModeTCP},
		{"udp-tcp", relay.ModeUDP, relay.ModeTCP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			wh, ch := newRecorder(), newRecorder()
			w := e.connect(t, e.p.Bundle(t, pki.RoleWorker, "w1"), tc.worker, wh)
			c := e.connect(t, e.p.Bundle(t, pki.RoleClient, "alice"), tc.user, ch)
			if w.Self() != relay.WorkerID("w1") {
				t.Fatalf("worker id %s", w.Self())
			}
			if want := map[relay.Mode]string{relay.ModeUDP: "udp", relay.ModeTCP: "tcp"}[tc.user]; c.Kind() != want {
				t.Fatalf("kind %s", c.Kind())
			}
			if tc.worker == relay.ModeUDP && !w.Reflexive().IsValid() {
				t.Fatal("UDP session without reflexive address")
			}
			// client → worker and back, full-size packets
			big := bytes.Repeat([]byte("q"), 1200)
			for i := 0; i < 20; i++ {
				if err := c.SendData(w.Self(), big); err != nil {
					t.Fatal(err)
				}
			}
			wh.waitFor(t, "data at worker", func() bool { return len(wh.data[c.Self()]) >= 15 })
			if !bytes.Equal(wh.data[c.Self()][0], big) {
				t.Fatal("payload mangled")
			}
			w.SendData(c.Self(), []byte("pong"))
			ch.waitFor(t, "data at client", func() bool { return len(ch.data[w.Self()]) == 1 })

			// rendezvous: the relay adds the sender's reflexive address
			c.SendCallMeMaybe(w.Self(), relay.CallMeMaybe{Reply: true})
			wh.waitFor(t, "CallMeMaybe", func() bool { return len(wh.cmms) == 1 })
			cm := wh.cmms[0]
			if cm.Peer != c.Self().String() || !cm.Reply {
				t.Fatalf("cmm %+v", cm)
			}
			hasLAN, hasRefl := false, false
			for _, a := range cm.Candidates {
				hasLAN = hasLAN || a.String() == "10.1.2.3:4567"
				hasRefl = hasRefl || a.Addr().IsLoopback()
			}
			if !hasLAN || hasRefl != (tc.user == relay.ModeUDP) || cm.Reflexive.IsValid() != (tc.user == relay.ModeUDP) {
				t.Fatalf("candidates %v reflexive %v", cm.Candidates, cm.Reflexive)
			}
			if len(c.STUN()) == 0 {
				t.Fatal("relay advertises no STUN endpoints")
			}

			// unknown destination → PeerGone (rate-limited)
			for i := 0; i < 5; i++ {
				c.SendData(relay.WorkerID("nobody"), []byte("x"))
			}
			ch.waitFor(t, "PeerGone", func() bool { return len(ch.gone) >= 1 })
			time.Sleep(200 * time.Millisecond)
			ch.mu.Lock()
			if len(ch.gone) != 1 || ch.gone[0] != relay.WorkerID("nobody") {
				t.Fatalf("gone %v", ch.gone)
			}
			ch.mu.Unlock()

			st := e.relay.Server.Stats()
			if len(st.Sessions) != 2 || st.PacketsRelayed < 16 || st.DropNoPeer != 5 {
				t.Fatalf("stats %+v", st)
			}
		})
	}
}

func TestRelayPolicyAndReplace(t *testing.T) {
	e := newEnv(t)
	h1, h2, hw := newRecorder(), newRecorder(), newRecorder()
	c1 := e.connect(t, e.p.Bundle(t, pki.RoleClient, "alice"), relay.ModeUDP, h1)
	c2 := e.connect(t, e.p.Bundle(t, pki.RoleAdmin, "bob"), relay.ModeTCP, h2)
	c1.SendData(c2.Self(), []byte("client to client"))
	c1.SendCallMeMaybe(c2.Self(), relay.CallMeMaybe{Reply: true})
	time.Sleep(300 * time.Millisecond)
	if st := e.relay.Server.Stats(); st.DropPolicy != 2 {
		t.Fatalf("client→client must be dropped: %+v", st)
	}

	wb := e.p.Bundle(t, pki.RoleWorker, "w1")
	w1 := e.connect(t, wb, relay.ModeTCP, hw)
	w2 := e.connect(t, wb, relay.ModeUDP, hw) // same worker again: newest wins
	select {
	case <-w1.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("old session not closed")
	}
	if !strings.Contains(w1.Err().Error(), "replaced") {
		t.Fatalf("old session error: %v", w1.Err())
	}
	c1.SendData(w2.Self(), []byte("hi"))
	hw.waitFor(t, "data at new session", func() bool { return len(hw.data[c1.Self()]) == 1 })
}

func TestRelayAuth(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	try := func(b *pki.Bundle, mode relay.Mode) error {
		pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
		tr := &quic.Transport{Conn: pc}
		defer func() { tr.Close(); pc.Close() }()
		c, err := relay.Connect(ctx, relay.ClientConfig{
			UDPAddr: netip.MustParseAddrPort(e.relay.Addr), QUIC: tr, TCPAddr: e.relay.TCPAdr,
			TLS: relay.ClientTLS(b, ""), Mode: mode, UDPTimeout: 2 * time.Second,
		}, newRecorder())
		if err == nil {
			// The server may reject after the handshake; the session then dies.
			select {
			case <-c.Done():
				err = c.Err()
			case <-time.After(time.Second):
			}
			c.Close()
		}
		return err
	}
	for _, mode := range []relay.Mode{relay.ModeUDP, relay.ModeTCP} {
		other := relaytest.NewPKI(t)
		if err := try(other.Bundle(t, pki.RoleClient, "mallory"), mode); err == nil {
			t.Errorf("mode %d: certificate from another CA accepted", mode)
		}
		if err := try(e.p.Bundle(t, pki.RoleRelay, "relay2"), mode); err == nil {
			t.Errorf("mode %d: relay certificate accepted as a node", mode)
		}
		if err := try(e.p.Bundle(t, pki.RoleRelease, "ci"), mode); err == nil {
			t.Errorf("mode %d: release (CI) certificate accepted as a node", mode)
		}
	}
	// A server presenting a non-relay certificate (a worker's) is refused.
	wb := e.p.Bundle(t, pki.RoleWorker, "w9")
	fake := relaytest.StartRelayWith(t, &relay.Server{TLS: &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{wb.Cert},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: wb.Pool, NextProtos: []string{relay.ALPN},
	}}, "")
	e.relay = fake
	for _, mode := range []relay.Mode{relay.ModeUDP, relay.ModeTCP} {
		err := try(e.p.Bundle(t, pki.RoleClient, "alice"), mode)
		if err == nil || !strings.Contains(err.Error(), "not a relay certificate") {
			t.Errorf("mode %d: fake relay: %v", mode, err)
		}
	}
}

func TestRelayRateLimit(t *testing.T) {
	p := relaytest.NewPKI(t)
	tlsConf, err := relay.ServerTLS(p.Bundle(t, pki.RoleRelay, "relay1"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{p: p, relay: relaytest.StartRelayWith(t, &relay.Server{TLS: tlsConf, Rate: 100 << 10}, "")} // 100 KiB/s, burst 64 KiB
	hw, hc := newRecorder(), newRecorder()
	w := e.connect(t, e.p.Bundle(t, pki.RoleWorker, "w1"), relay.ModeTCP, hw)
	c := e.connect(t, e.p.Bundle(t, pki.RoleClient, "alice"), relay.ModeTCP, hc)
	pkt := make([]byte, 1000)
	for i := 0; i < 300; i++ { // 300 KB at once
		c.SendData(w.Self(), pkt)
	}
	time.Sleep(500 * time.Millisecond)
	st := e.relay.Server.Stats()
	if st.DropRateLimit == 0 || st.PacketsRelayed > 150 {
		t.Fatalf("rate limit not applied: %+v", st)
	}
}

func TestRelayCallMeMaybeRateLimit(t *testing.T) {
	e := newEnv(t)
	hw, hc := newRecorder(), newRecorder()
	w := e.connect(t, e.p.Bundle(t, pki.RoleWorker, "w1"), relay.ModeTCP, hw)
	c := e.connect(t, e.p.Bundle(t, pki.RoleClient, "alice"), relay.ModeUDP, hc)
	for i := 0; i < 50; i++ {
		c.SendCallMeMaybe(w.Self(), relay.CallMeMaybe{Candidates: []netip.AddrPort{netip.MustParseAddrPort("192.0.2.1:1")}})
	}
	time.Sleep(500 * time.Millisecond)
	hw.mu.Lock()
	n := len(hw.cmms)
	hw.mu.Unlock()
	if n < 5 || n > 13 {
		t.Fatalf("%d of 50 CallMeMaybes forwarded", n)
	}
}
