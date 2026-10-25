package longnet_test

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
	"github.com/lubobill1990/longarm/internal/relay/relaytest"
	"github.com/lubobill1990/longarm/internal/transport"
	"github.com/lubobill1990/longarm/internal/transport/longnet"
)

// natSpec describes how a node is connected: open (no NAT) or behind a
// router with the given mapping and filtering behaviour.
type natSpec struct {
	open    bool
	mapping mappingKind
	filter  filterKind
}

var (
	natOpen     = natSpec{open: true}
	natEasyAPDF = natSpec{mapping: eim, filter: apdf}  // port-restricted cone
	natEasyADF  = natSpec{mapping: eim, filter: adf}   // restricted cone
	natEasyEIF  = natSpec{mapping: eim, filter: eif}   // full cone
	natHard     = natSpec{mapping: adpm, filter: apdf} // symmetric
)

func (s natSpec) String() string {
	if s.open {
		return "open"
	}
	m := map[mappingKind]string{eim: "EIM", adpm: "symmetric"}[s.mapping]
	f := map[filterKind]string{eif: "EIF", adf: "addr-restricted", apdf: "port-restricted"}[s.filter]
	return m + "/" + f
}

// natWorld is a relay plus an internet with routers.
type natWorld struct {
	*world
	in *internet
}

func newNATWorld(t *testing.T) *natWorld {
	w := newWorld(t)
	in := newInternet()
	in.own(apOf(w.relay.UDP.LocalAddr()).Port(), "relay")
	in.own(apOf(w.relay.STUN2.LocalAddr()).Port(), "relay")
	return &natWorld{world: w, in: in}
}

// natNode starts a node behind spec; the router is nil for open nodes.
func (w *natWorld) natNode(role pki.Role, name string, spec natSpec, timing longnet.Timing, mods ...func(*longnet.Options)) (*longnet.Node, *natRouter) {
	w.t.Helper()
	var r *natRouter
	o := longnet.Options{
		Relay: w.relay.Addr, Bundle: w.pki.Bundle(w.t, role, name), Bind: "127.0.0.1:0", Mode: relay.ModeUDP,
		Timing: timing, UDPTimeout: 2 * time.Second, STUNInterval: 300 * time.Millisecond, Logger: relaytest.Logger(),
	}
	if !spec.open {
		r = newNATRouter(w.in, "nat-"+name, spec.mapping, spec.filter)
		o.WrapPacketConn = func(pc net.PacketConn) net.PacketConn { r.inner = pc; return r }
	}
	for _, m := range mods {
		m(&o)
	}
	n, err := longnet.NewNode(o)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { n.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := n.WaitRelay(ctx); err != nil { // see world.node
		w.t.Fatal(err)
	}
	return n, r
}

func (w *natWorld) natEchoWorker(name string, spec natSpec, timing longnet.Timing) (*longnet.Node, *natRouter) {
	n, r := w.natNode(pki.RoleWorker, name, spec, timing)
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
			go func() {
				buf := make([]byte, 32<<10)
				for {
					k, err := c.Read(buf)
					if err != nil {
						c.Close()
						return
					}
					c.Write(buf[:k])
				}
			}()
		}
	}()
	return n, r
}

func waitNAT(t *testing.T, n *longnet.Node, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		typ, mapped := n.NAT()
		if typ == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("NAT type %q (mapped %v), want %q", typ, mapped, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func natType(s natSpec) string {
	switch {
	case s.open:
		return relay.NATOpen
	case s.mapping == adpm:
		return relay.NATHard
	}
	return relay.NATEasy
}

// stays asserts that c keeps path kind for d.
func stays(t *testing.T, c net.Conn, kind transport.PathKind, d time.Duration) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(25 * time.Millisecond) {
		if pi := pathOf(c); pi.Kind != kind {
			t.Fatalf("path became %v", pi)
		}
	}
}

// The NAT traversal matrix: the client and the worker each sit behind one
// router of the given kind (or none); the relay is "on the Internet".
func TestNATMatrix(t *testing.T) {
	cases := []struct {
		client, worker natSpec
		direct         bool
	}{
		// A common case (home CGNAT with EIM × cloud SNAT) — direct as
		// long as the easy side's filter admits the hard side's new port:
		// it does for full cone and restricted cone (opened by our own
		// probes to the peer's IP), as with Tailscale.
		{natEasyADF, natHard, true},
		{natEasyEIF, natHard, true},
		{natHard, natEasyADF, true},
		// Port-restricted × symmetric: neither side can learn the port the
		// symmetric NAT picks toward the other, and a port-restricted
		// filter admits only exactly that ip:port. Not traversable without
		// port guessing (Tailscale's "hard" case too) → relay.
		{natEasyAPDF, natHard, false},
		// easy × easy
		{natEasyAPDF, natEasyAPDF, true},
		{natEasyADF, natEasyAPDF, true},
		// hard × hard: detected, not even attempted
		{natHard, natHard, false},
		// open × anything
		{natOpen, natHard, true},
		{natHard, natOpen, true},
		{natOpen, natEasyAPDF, true},
		{natOpen, natOpen, true},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%s×%s", tc.client, tc.worker), func(t *testing.T) {
			t.Parallel()
			w := newNATWorld(t)
			timing := fastTiming
			timing.BackoffMax = time.Minute // no rare hard × hard retry within the test
			name := fmt.Sprintf("w%d", i)
			wn, _ := w.natEchoWorker(name, tc.worker, timing)
			cn, _ := w.natNode(pki.RoleClient, "alice", tc.client, timing)
			waitNAT(t, wn, natType(tc.worker), 5*time.Second)
			waitNAT(t, cn, natType(tc.client), 5*time.Second)
			c := dial(t, cn, name)
			echo(t, c, 100)
			if tc.direct {
				pi := waitPath(t, c, transport.PathDirect, 6*time.Second)
				d0, r0 := cn.Stats()
				echo(t, c, 256<<10)
				d1, r1 := cn.Stats()
				if d1-d0 < 100 || r1-r0 > 30 {
					t.Fatalf("direct %v: %d direct, %d relayed packets", pi, d1-d0, r1-r0)
				}
				t.Logf("direct: %v", pi)
				return
			}
			stays(t, c, transport.PathRelay, 3*timing.PunchWindow)
			echo(t, c, 64<<10)
			pi := pathOf(c)
			want := "nat " + natType(tc.client) + "/" + natType(tc.worker)
			if !strings.Contains(pi.Detail, want) {
				t.Fatalf("path %v, want %q in the detail", pi, want)
			}
			if tc.client.mapping == adpm && tc.worker.mapping == adpm {
				// hard × hard: only the few probes sent before both sides
				// know (a punch window is ~20 per candidate, 3 candidates
				// each: ~120 for the pair).
				p1, _ := cn.DiscoStats()
				p2, _ := wn.DiscoStats()
				t.Logf("hard × hard probes: %d + %d", p1, p2)
				if p1+p2 > 40 {
					t.Fatalf("hard × hard sent %d + %d probes", p1, p2)
				}
			}
			t.Logf("relayed as expected: %v", pi)
		})
	}
}

// The relay reports each session's NAT type, STUN-observed and reflexive
// addresses; loopback isn't forwarded to peers on another IP.
func TestRelaySessionsShowNAT(t *testing.T) {
	w := newNATWorld(t)
	wn, _ := w.natEchoWorker("w1", natHard, fastTiming)
	cn, _ := w.natNode(pki.RoleClient, "alice", natEasyADF, fastTiming)
	waitNAT(t, wn, relay.NATHard, 5*time.Second)
	waitNAT(t, cn, relay.NATEasy, 5*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := w.relay.Server.Stats()
		got := map[string]relay.SessionStats{}
		for _, s := range st.Sessions {
			got[s.Node] = s
		}
		ws, cs := got["worker/w1"], got["client/alice"]
		ok := ws.NAT == relay.NATHard && len(ws.Mapped) == 2 && cs.NAT == relay.NATEasy && len(cs.Mapped) == 1 &&
			ws.Reflexive != "" && st.STUNRequests >= 4
		// The STUN-observed public addresses are among the candidates.
		for _, m := range cs.Mapped {
			ok = ok && contains(cs.Candidates, m)
		}
		if ok {
			t.Logf("worker %+v\nclient %+v", ws, cs)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sessions: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func contains(l []netip.AddrPort, a netip.AddrPort) bool {
	for _, x := range l {
		if x == a {
			return true
		}
	}
	return false
}

// The direct path dies (the link between the two NATs is cut): back to the
// relay within DirectTimeout. Then the NATs lose their mappings (reboot /
// timeout) and the link comes back: STUN notices the new public ports, the
// peers exchange them and the path goes direct again.
func TestNATDirectPathRecovers(t *testing.T) {
	w := newNATWorld(t)
	wn, wr := w.natEchoWorker("w1", natEasyAPDF, fastTiming)
	cn, cr := w.natNode(pki.RoleClient, "alice", natEasyADF, fastTiming)
	waitNAT(t, wn, relay.NATEasy, 5*time.Second)
	_, before := wn.NAT()
	c := dial(t, cn, "w1")
	echo(t, c, 100)
	pi := waitPath(t, c, transport.PathDirect, 6*time.Second)
	t.Logf("direct: %v", pi)

	w.in.cut(wr.name, cr.name, true)
	waitPath(t, c, transport.PathRelay, fastTiming.DirectTimeout+time.Second)
	echo(t, c, 64<<10) // the stream survives on the relay

	wr.reset()
	cr.reset()
	w.in.cut(wr.name, cr.name, false)
	pi = waitPath(t, c, transport.PathDirect, 10*time.Second)
	echo(t, c, 64<<10)
	_, after := wn.NAT()
	if len(before) != 1 || len(after) != 1 || before[0] == after[0] {
		t.Fatalf("worker mapping before %v after %v: expected a new public port", before, after)
	}
	if !strings.HasPrefix(pi.Detail, after[0].String()) {
		t.Fatalf("direct path %v, want the new mapping %v", pi, after[0])
	}
	t.Logf("direct again: %v", pi)
}

// The client's relay session runs over TLS/TCP (QUIC to the relay blocked
// or throttled): the relay can't observe its UDP mapping, but STUN still
// finds it, so the hard-NAT worker learns where to send and the path goes
// direct. (Before STUN, such a client advertised only LAN addresses.)
func TestNATRelayOverTCPStillPunches(t *testing.T) {
	w := newNATWorld(t)
	wn, _ := w.natEchoWorker("w1", natHard, fastTiming)
	cn, _ := w.natNode(pki.RoleClient, "alice", natEasyADF, fastTiming, func(o *longnet.Options) { o.Mode = relay.ModeTCP })
	waitNAT(t, wn, relay.NATHard, 5*time.Second)
	waitNAT(t, cn, relay.NATEasy, 5*time.Second)
	c := dial(t, cn, "w1")
	echo(t, c, 100)
	if pi := pathOf(c); !strings.Contains(pi.String(), "tcp") && pi.Kind == transport.PathRelay {
		t.Fatalf("relay session isn't TCP: %v", pi)
	}
	pi := waitPath(t, c, transport.PathDirect, 6*time.Second)
	echo(t, c, 64<<10)
	t.Logf("direct: %v", pi)
}
