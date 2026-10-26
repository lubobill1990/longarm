package longnet

import (
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/relay"
)

func TestClassifyNAT(t *testing.T) {
	ap := netip.MustParseAddrPort
	relayMain, relay2, google := ap("203.0.113.10:7443"), ap("203.0.113.10:7444"), ap("[2001:4860::1]:19302")
	local := []netip.AddrPort{ap("192.168.1.5:5000"), ap("127.0.0.1:5000")}
	for _, tc := range []struct {
		name   string
		obs    map[netip.AddrPort]netip.AddrPort
		want   string
		mapped int
	}{
		{"nothing", nil, relay.NATUnknown, 0},
		{"one point", map[netip.AddrPort]netip.AddrPort{relayMain: ap("198.51.100.1:4000")}, relay.NATUnknown, 1},
		{"easy", map[netip.AddrPort]netip.AddrPort{relayMain: ap("198.51.100.1:4000"), relay2: ap("198.51.100.1:4000")}, relay.NATEasy, 1},
		{"hard", map[netip.AddrPort]netip.AddrPort{relayMain: ap("203.0.113.78:17258"), relay2: ap("203.0.113.78:27225")}, relay.NATHard, 2},
		{"open", map[netip.AddrPort]netip.AddrPort{relayMain: ap("192.168.1.5:5000"), relay2: ap("192.168.1.5:5000")}, relay.NATOpen, 1},
		// Different families are not compared: v4 + v6 answers aren't "hard".
		{"dual stack", map[netip.AddrPort]netip.AddrPort{relayMain: ap("198.51.100.1:4000"), relay2: ap("198.51.100.1:4000"),
			google: ap("[2408::1]:5000")}, relay.NATEasy, 2},
	} {
		typ, mapped := classifyNAT(tc.obs, local)
		if typ != tc.want || len(mapped) != tc.mapped {
			t.Errorf("%s: %q %v, want %q with %d mapped", tc.name, typ, mapped, tc.want, tc.mapped)
		}
	}

	// A hub behind its platform's source NAT (Azure Container Instances)
	// answers every client with a private address: ignored, the type comes
	// from the public answers of the external servers only.
	ext1, ext2, lan := ap("198.51.100.53:3478"), ap("203.0.113.53:19302"), ap("192.168.1.1:3478")
	snat1, snat2 := ap("10.0.0.7:4000"), ap("10.0.0.8:4000")
	pubLocal := append([]netip.AddrPort{ap("198.51.100.9:5000")}, local...)
	for _, tc := range []struct {
		name   string
		obs    map[netip.AddrPort]netip.AddrPort
		local  []netip.AddrPort
		want   string
		mapped []netip.AddrPort
	}{
		{"only SNATed answers", map[netip.AddrPort]netip.AddrPort{relayMain: snat1, relay2: snat2}, local, relay.NATUnknown, nil},
		{"SNATed + one public", map[netip.AddrPort]netip.AddrPort{relayMain: snat1, relay2: snat1, ext1: ap("198.51.100.1:4000")},
			local, relay.NATUnknown, []netip.AddrPort{ap("198.51.100.1:4000")}},
		{"SNATed + two equal public", map[netip.AddrPort]netip.AddrPort{relayMain: snat1, relay2: snat2,
			ext1: ap("198.51.100.1:4000"), ext2: ap("198.51.100.1:4000")}, local, relay.NATEasy, []netip.AddrPort{ap("198.51.100.1:4000")}},
		{"SNATed + two different public", map[netip.AddrPort]netip.AddrPort{relayMain: snat1,
			ext1: ap("198.51.100.1:4000"), ext2: ap("198.51.100.1:4001")}, local, relay.NATHard,
			[]netip.AddrPort{ap("198.51.100.1:4000"), ap("198.51.100.1:4001")}},
		// A LAN server's private answer is a candidate, but isn't compared
		// with the public ones (no "hard" from mixing the two).
		{"LAN + public", map[netip.AddrPort]netip.AddrPort{lan: ap("192.168.1.5:5000"), ext1: ap("198.51.100.1:4000"),
			ext2: ap("198.51.100.1:4000")}, local, relay.NATEasy, []netip.AddrPort{ap("192.168.1.5:5000"), ap("198.51.100.1:4000")}},
		{"LAN + one public", map[netip.AddrPort]netip.AddrPort{lan: ap("192.168.1.5:5000"), ext1: ap("198.51.100.1:4000")},
			local, relay.NATUnknown, []netip.AddrPort{ap("192.168.1.5:5000"), ap("198.51.100.1:4000")}},
		{"LAN only: open as before", map[netip.AddrPort]netip.AddrPort{lan: ap("192.168.1.5:5000")}, local, relay.NATOpen,
			[]netip.AddrPort{ap("192.168.1.5:5000")}},
		// Everything on loopback (the tests): private answers from a
		// private server are kept.
		{"loopback hard", map[netip.AddrPort]netip.AddrPort{ap("127.0.0.1:7443"): ap("127.0.0.1:4000"), ap("127.0.0.1:7444"): ap("127.0.0.1:4001")},
			nil, relay.NATHard, []netip.AddrPort{ap("127.0.0.1:4000"), ap("127.0.0.1:4001")}},
		{"loopback open", map[netip.AddrPort]netip.AddrPort{ap("127.0.0.1:7443"): ap("127.0.0.1:5000")}, local, relay.NATOpen,
			[]netip.AddrPort{ap("127.0.0.1:5000")}},
		// A host with a public address of its own.
		{"public open", map[netip.AddrPort]netip.AddrPort{relayMain: snat1, ext1: ap("198.51.100.9:5000")}, pubLocal, relay.NATOpen,
			[]netip.AddrPort{ap("198.51.100.9:5000")}},
	} {
		typ, mapped := classifyNAT(tc.obs, tc.local)
		if typ != tc.want || !slices.Equal(mapped, tc.mapped) {
			t.Errorf("%s: %q %v, want %q %v", tc.name, typ, mapped, tc.want, tc.mapped)
		}
	}
	if !usableSTUN(relayMain, ap("198.51.100.1:1"), local) || usableSTUN(relayMain, ap("100.64.0.1:1"), local) || usableSTUN(relayMain, ap("[fd00::1]:1"), local) ||
		!usableSTUN(ap("10.0.0.1:3478"), ap("10.0.0.2:1"), local) || usableSTUN(relayMain, netip.AddrPort{}, local) ||
		!usableSTUN(relayMain, ap("192.168.1.5:5000"), local) || usableSTUN(relayMain, ap("192.168.1.5:5001"), local) {
		t.Error("usableSTUN")
	}

	if c := mergeCandidates([]netip.AddrPort{ap("1.2.3.4:5")}, []netip.AddrPort{ap("10.0.0.1:5"), ap("1.2.3.4:5")}); len(c) != 2 || c[0] != ap("1.2.3.4:5") {
		t.Fatalf("merge %v", c)
	}
}

// hard × hard: the attempt started on connect stops as soon as the peer's
// NAT type is known; the peer can't make us probe either; a real attempt
// (on both sides) only every hardRetry.
func TestPathHardHard(t *testing.T) {
	pm := newPathManager(tm, false)
	pm.selfNAT = relay.NATHard
	t0 := time.Unix(1000, 0)
	const peer relay.ID = 7
	if k := kinds(pm.addKey(peer, testKey(1), t0)); k[actCallMeMaybe] != 1 {
		t.Fatalf("addKey: %v", k)
	}
	// The peer's CallMeMaybe says hard: our attempt is aborted.
	pm.onCallMeMaybe(peer, []netip.AddrPort{candA}, true, relay.NATHard, t0.Add(10*time.Millisecond))
	if p := pings(pm.tick(t0.Add(20 * time.Millisecond))); len(p) != 0 {
		t.Fatalf("probing hard × hard: %v", p)
	}
	// More CallMeMaybes from the peer don't start probing either.
	pm.onCallMeMaybe(peer, []netip.AddrPort{candA}, true, relay.NATHard, t0.Add(tm.PunchWindow*3))
	if p := pings(pm.tick(t0.Add(tm.PunchWindow*3 + time.Millisecond))); len(p) != 0 {
		t.Fatalf("peer made us probe: %v", p)
	}
	// After hardRetry: one real attempt (forced: a CallMeMaybe reply from
	// the peer doesn't abort it), CallMeMaybe asks the peer to join.
	at := t0.Add(10*time.Millisecond + pm.hardRetry())
	acts := pm.tick(at)
	if k := kinds(acts); k[actCallMeMaybe] != 1 || k[actPing] != 1 {
		t.Fatalf("retry: %v", k)
	}
	pm.onCallMeMaybe(peer, []netip.AddrPort{candA}, false, relay.NATHard, at.Add(time.Millisecond))
	if p := pings(pm.tick(at.Add(tm.PunchInterval))); len(p) != 1 {
		t.Fatalf("forced attempt aborted: %v", p)
	}
	// It fails → next one hardRetry later, with a failure report.
	acts = pm.tick(at.Add(tm.PunchWindow))
	if k := kinds(acts); k[actPunchFailed] != 1 {
		t.Fatalf("no failure report: %v", k)
	}
	for _, a := range acts {
		if a.kind == actPunchFailed && (a.stats.selfNAT != relay.NATHard || a.stats.peerNAT != relay.NATHard || a.stats.pingsSent == 0) {
			t.Fatalf("stats %+v", a.stats)
		}
	}
	if next := pm.peers[peer].nextPunch; next.Sub(at.Add(tm.PunchWindow)) < pm.hardRetry() {
		t.Fatalf("next attempt at %v", next.Sub(at))
	}

	// The other side: the peer's rare retry (reply=true, after a long
	// quiet period) is joined.
	pm2 := newPathManager(tm, false)
	pm2.selfNAT = relay.NATHard
	pm2.onCallMeMaybe(peer, []netip.AddrPort{candA}, false, relay.NATHard, t0)
	pm2.addKey(peer, testKey(1), t0)
	if p := pings(pm2.tick(t0.Add(time.Millisecond))); len(p) != 0 {
		t.Fatalf("known hard × hard probed on connect: %v", p)
	}
	later := t0.Add(pm2.hardRetry())
	pm2.onCallMeMaybe(peer, []netip.AddrPort{candA}, true, relay.NATHard, later)
	if p := pings(pm2.tick(later.Add(time.Millisecond))); len(p) != 1 {
		t.Fatalf("didn't join the peer's retry: %v", p)
	}
}

// Our NAT type / candidates changing re-announces to peers and starts an
// attempt (unless one ran recently).
func TestPathSelfNATChange(t *testing.T) {
	pm := newPathManager(tm, false)
	t0 := time.Unix(1000, 0)
	const peer relay.ID = 7
	pm.onCallMeMaybe(peer, []netip.AddrPort{candA}, false, relay.NATEasy, t0)
	pm.addKey(peer, testKey(1), t0)
	// During the first attempt: candidates only.
	if k := kinds(pm.setSelfNAT(relay.NATEasy, t0.Add(time.Millisecond))); k[actCallMeMaybe] != 1 {
		t.Fatalf("during attempt: %v", k)
	}
	// Long after: a new attempt.
	later := t0.Add(10 * tm.PunchWindow)
	pm.tick(t0.Add(tm.PunchWindow)) // first attempt fails
	acts := pm.setSelfNAT(relay.NATEasy, later)
	if k := kinds(acts); k[actCallMeMaybe] != 1 || !pm.punching(pm.peers[peer], later) {
		t.Fatalf("after: %v", k)
	}
	// Becoming hard with a hard peer aborts.
	pm.onCallMeMaybe(peer, []netip.AddrPort{candA}, false, relay.NATHard, later)
	pm.setSelfNAT(relay.NATHard, later.Add(time.Millisecond))
	if pm.punching(pm.peers[peer], later.Add(2*time.Millisecond)) {
		t.Fatal("hard × hard still punching")
	}
}

// Hard × hard is not pointless for peers behind the same public IP or with
// global IPv6 on both sides.
func TestPathHardHardExceptions(t *testing.T) {
	const peer relay.ID = 7
	t0 := time.Unix(1000, 0)
	ap := netip.MustParseAddrPort
	for _, tc := range []struct {
		name       string
		selfPublic []netip.AddrPort
		selfV6     bool
		peerCands  []netip.AddrPort
		pointless  bool
	}{
		{"different NATs", []netip.AddrPort{ap("203.0.113.78:1000")}, false, []netip.AddrPort{ap("198.51.100.20:2000"), ap("10.0.0.2:3000")}, true},
		{"same public IP", []netip.AddrPort{ap("203.0.113.78:1000")}, false, []netip.AddrPort{ap("203.0.113.78:2000"), ap("172.16.0.5:3000")}, false},
		{"both IPv6", nil, true, []netip.AddrPort{ap("198.51.100.20:2000"), ap("[2001:db8::7]:3000")}, false},
		{"peer IPv6 only", nil, false, []netip.AddrPort{ap("198.51.100.20:2000"), ap("[2001:db8::7]:3000")}, true},
		{"loopback isn't a public IP", []netip.AddrPort{ap("127.0.0.1:1000")}, false, []netip.AddrPort{ap("127.0.0.1:2000")}, true},
	} {
		pm := newPathManager(tm, false)
		pm.selfNAT, pm.selfPublic, pm.selfV6 = relay.NATHard, tc.selfPublic, tc.selfV6
		pm.onCallMeMaybe(peer, tc.peerCands, false, relay.NATHard, t0)
		if got := pm.pointless(pm.peers[peer]); got != tc.pointless {
			t.Errorf("%s: pointless=%v", tc.name, got)
		}
	}
}

// Incomplete STUN rounds (lost answers, rate limits) never downgrade what
// we know.
func TestEvaluateNATIncompleteRounds(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	n := &Node{log: slog.New(slog.DiscardHandler), sock: pc, pm: newPathManager(tm, false)}
	ap := netip.MustParseAddrPort
	a, b := ap("203.0.113.10:7443"), ap("203.0.113.10:7444")
	n.nat.primarySrv = a
	check := func(step, wantType string, wantMapped int) {
		t.Helper()
		typ, mapped := n.NAT()
		if typ != wantType || len(mapped) != wantMapped {
			t.Fatalf("%s: %q %v", step, typ, mapped)
		}
	}
	n.evaluateNAT(map[netip.AddrPort]netip.AddrPort{a: ap("203.0.113.78:1"), b: ap("203.0.113.78:2")}, false)
	check("full round", relay.NATHard, 2)
	n.evaluateNAT(map[netip.AddrPort]netip.AddrPort{a: ap("203.0.113.78:1")}, true)
	check("one answer missing", relay.NATHard, 1)
	n.evaluateNAT(map[netip.AddrPort]netip.AddrPort{}, true)
	check("no answers", relay.NATHard, 1)
	if !slices.Contains(n.nat.cands, ap("203.0.113.78:1")) {
		t.Fatalf("public candidate lost: %v", n.nat.cands)
	}
	n.evaluateNAT(map[netip.AddrPort]netip.AddrPort{a: ap("203.0.113.78:3"), b: ap("203.0.113.78:3")}, false)
	check("full round, now easy", relay.NATEasy, 1)
}
