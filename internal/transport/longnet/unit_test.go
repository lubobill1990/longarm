package longnet

import (
	"bytes"
	"net/netip"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/relay"
)

func testKey(b byte) discoKey { return newDiscoKey(bytes.Repeat([]byte{b}, 32)) }

func TestDiscoSealOpen(t *testing.T) {
	k := testKey(1)
	keys := func(src relay.ID) []discoKey {
		if src == 10 {
			return []discoKey{testKey(9), k}
		}
		return nil
	}
	ping := discoMsg{typ: typePing, src: 10, dst: 20, tx: newTxid(), seq: 42}
	b := ping.seal(k)
	m, err := openDisco(b, keys)
	if err != nil || m.tx != ping.tx || m.src != 10 || m.dst != 20 || m.seq != 42 || m.keyID != k.id {
		t.Fatalf("ping round trip: %+v %v", m, err)
	}
	pong := discoMsg{typ: typePong, src: 10, dst: 20, tx: newTxid(), observed: netip.MustParseAddrPort("[2001:db8::1]:999")}
	m, err = openDisco(pong.seal(k), keys)
	if err != nil || m.observed != pong.observed {
		t.Fatalf("pong round trip: %+v %v", m, err)
	}

	// Any bit flip is detected.
	for i := range b {
		c := append([]byte(nil), b...)
		c[i] ^= 0x40
		if _, err := openDisco(c, keys); err == nil {
			t.Fatalf("flip at %d accepted", i)
		}
	}
	// Wrong key, unknown sender, truncation.
	if _, err := openDisco(ping.seal(testKey(2)), keys); err == nil {
		t.Fatal("wrong key accepted")
	}
	other := ping
	other.src = 11
	if _, err := openDisco(other.seal(k), keys); err == nil {
		t.Fatal("unknown sender accepted")
	}
	if _, err := openDisco(b[:len(b)-1], keys); err == nil {
		t.Fatal("truncated accepted")
	}
	// Reflection: a ping sent by 20 to 10 can't be replayed as coming from 10
	// (src is MACed, and the receiver looks keys up by src).
	refl := append([]byte(nil), b...)
	copy(refl[4:12], b[12:20])
	copy(refl[12:20], b[4:12])
	if _, err := openDisco(refl, keys); err == nil {
		t.Fatal("reflected ping accepted")
	}
}

func TestPingReplay(t *testing.T) {
	pm := newPathManager(tm, false)
	t0 := time.Unix(1000, 0)
	k1, k2 := testKey(1), testKey(2)
	if pm.acceptPing(7, k1.id, 1) {
		t.Fatal("ping from a peer without keys accepted")
	}
	pm.addKey(7, k1, t0)
	pm.addKey(7, k2, t0)
	if !pm.acceptPing(7, k1.id, 1) || !pm.acceptPing(7, k1.id, 5) {
		t.Fatal("fresh pings rejected")
	}
	if pm.acceptPing(7, k1.id, 5) || pm.acceptPing(7, k1.id, 3) {
		t.Fatal("replayed / old ping accepted")
	}
	if !pm.acceptPing(7, k2.id, 1) { // sequence numbers are per key
		t.Fatal("other key's ping rejected")
	}
	// Our own pings carry increasing sequence numbers.
	a1 := pm.ping(7, pm.peers[7], candA, t0)
	a2 := pm.ping(7, pm.peers[7], candA, t0)
	if a2.seq <= a1.seq || a2.key.id != k2.id {
		t.Fatalf("seq %d then %d, key %x", a1.seq, a2.seq, a2.key.id)
	}
}

func TestCandidates(t *testing.T) {
	pm := newPathManager(tm, false)
	t0 := time.Unix(1000, 0)
	const peer relay.ID = 7
	pm.addKey(peer, testKey(1), t0)
	// Bogus addresses are ignored.
	pm.onCallMeMaybe(peer, []netip.AddrPort{netip.MustParseAddrPort("255.255.255.255:9"),
		netip.MustParseAddrPort("0.0.0.0:9"), netip.MustParseAddrPort("224.0.0.1:9"), netip.MustParseAddrPort("192.0.2.9:0"), candA}, false, "", t0)
	if len(pm.peers[peer].cands) != 1 {
		t.Fatalf("cands %v", pm.peers[peer].cands)
	}
	// Learned addresses fill up, but advertised ones evict them.
	for i := 0; i < 30; i++ {
		pm.onPing(peer, netip.AddrPortFrom(netip.MustParseAddr("198.51.100.1"), uint16(1000+i)), t0.Add(time.Duration(i)*time.Millisecond))
	}
	if len(pm.peers[peer].cands) != maxPeerCandidates {
		t.Fatalf("%d cands", len(pm.peers[peer].cands))
	}
	pm.onCallMeMaybe(peer, []netip.AddrPort{candA, candB}, false, "", t0.Add(time.Second))
	if pm.peers[peer].cands[candB] == nil || pm.peers[peer].cands[candA] == nil {
		t.Fatal("advertised candidate dropped")
	}
	// Learned ones expire.
	pm.tick(t0.Add(learnedTTL + time.Minute))
	if n := len(pm.peers[peer].cands); n != 2 {
		t.Fatalf("%d cands after expiry", n)
	}
}

func TestPeerCannotForceContinuousProbing(t *testing.T) {
	pm := newPathManager(tm, false)
	t0 := time.Unix(1000, 0)
	const peer relay.ID = 7
	pm.addKey(peer, testKey(1), t0) // attempt 1 starts at t0
	probes := 0
	for now := t0; now.Before(t0.Add(time.Minute)); now = now.Add(tm.PunchInterval) {
		// a CallMeMaybe (with fresh candidates) every 5 s
		if now.Sub(t0)%(5*time.Second) == 0 {
			pm.onCallMeMaybe(peer, []netip.AddrPort{candA, candB}, true, "", now)
		}
		probes += len(pings(pm.tick(now)))
	}
	// Without the limit: 12 windows × 25 rounds × 2 = 600 probes.
	if probes > 350 {
		t.Fatalf("%d probes in a minute", probes)
	}
}

func TestHeader(t *testing.T) {
	b := appendHeader(nil, typeData, 1, 2)
	b = append(b, "payload"...)
	typ, src, dst, rest, ok := parseHeader(b)
	if !ok || typ != typeData || src != 1 || dst != 2 || string(rest) != "payload" {
		t.Fatal("header round trip")
	}
	if _, _, _, _, ok := parseHeader([]byte("XWN")); ok {
		t.Fatal("garbage accepted")
	}
	b[0] = 'X'
	if _, _, _, _, ok := parseHeader(b); ok {
		t.Fatal("bad magic accepted")
	}
}

var (
	tm = Timing{PunchInterval: 200 * time.Millisecond, PunchWindow: 5 * time.Second, Keepalive: 2 * time.Second,
		DirectTimeout: 6500 * time.Millisecond, BackoffMin: 30 * time.Second, BackoffMax: 5 * time.Minute}
	candA = netip.MustParseAddrPort("192.0.2.1:1000")
	candB = netip.MustParseAddrPort("198.51.100.7:2000")
)

func kinds(acts []action) map[actionKind]int {
	m := map[actionKind]int{}
	for _, a := range acts {
		m[a.kind]++
	}
	return m
}

func pings(acts []action) []action {
	var out []action
	for _, a := range acts {
		if a.kind == actPing {
			out = append(out, a)
		}
	}
	return out
}

func TestPathPunchToDirect(t *testing.T) {
	pm := newPathManager(tm, false)
	t0 := time.Unix(1000, 0)
	const peer relay.ID = 7
	// Candidates may arrive before the connection (no key yet): no probes.
	if acts := pm.onCallMeMaybe(peer, []netip.AddrPort{candA, candB}, true, "", t0); kinds(acts)[actCallMeMaybe] != 1 || len(pings(acts)) != 0 {
		t.Fatalf("cmm before key: %+v", acts)
	}
	if len(pm.tick(t0)) != 0 {
		t.Fatal("probes without a key")
	}
	// Connection established: punching starts (CallMeMaybe asking for a reply).
	acts := pm.addKey(peer, testKey(1), t0)
	if len(acts) != 1 || acts[0].kind != actCallMeMaybe || !acts[0].reply {
		t.Fatalf("addKey: %+v", acts)
	}
	acts = pm.tick(t0)
	if len(pings(acts)) != 2 {
		t.Fatalf("first probe round: %+v", acts)
	}
	if len(pings(pm.tick(t0.Add(100*time.Millisecond)))) != 0 {
		t.Fatal("probes faster than PunchInterval")
	}
	acts2 := pm.tick(t0.Add(200 * time.Millisecond))
	if len(pings(acts2)) != 2 {
		t.Fatal("no resend after PunchInterval")
	}
	if _, direct := pm.route(peer); direct {
		t.Fatal("direct before any pong")
	}
	// Pong for the probe to candB → direct via candB.
	var txB txid
	for _, a := range pings(acts2) {
		if a.addr == candB {
			txB = a.tx
		}
	}
	ch := pm.onPong(peer, txB, t0.Add(230*time.Millisecond))
	if len(ch) != 1 || ch[0].kind != actPathChange || !ch[0].direct || ch[0].addr != candB || ch[0].rtt != 30*time.Millisecond {
		t.Fatalf("pong: %+v", ch)
	}
	if a, direct := pm.route(peer); !direct || a != candB {
		t.Fatal("route not direct")
	}
	// A replayed pong does nothing.
	if pm.onPong(peer, txB, t0.Add(240*time.Millisecond)) != nil {
		t.Fatal("replayed pong accepted")
	}
	// Keepalive: one ping to the direct address every Keepalive.
	at := t0.Add(230*time.Millisecond + tm.Keepalive)
	ka := pings(pm.tick(at))
	if len(ka) != 1 || ka[0].addr != candB {
		t.Fatalf("keepalive: %+v", ka)
	}
	pm.onPong(peer, ka[0].tx, at.Add(10*time.Millisecond))
	if _, direct := pm.route(peer); !direct {
		t.Fatal("lost direct after keepalive pong")
	}
}

func TestPathFallbackAndBackoff(t *testing.T) {
	pm := newPathManager(tm, false)
	t0 := time.Unix(1000, 0)
	const peer relay.ID = 7
	pm.onCallMeMaybe(peer, []netip.AddrPort{candA}, false, "", t0)
	pm.addKey(peer, testKey(1), t0)
	p := pings(pm.tick(t0))
	pm.onPong(peer, p[0].tx, t0.Add(5*time.Millisecond))

	// Keepalives go unanswered → relay after DirectTimeout.
	now := t0
	lostAt := time.Time{}
	for i := 0; i < 100 && lostAt.IsZero(); i++ {
		now = now.Add(100 * time.Millisecond)
		for _, a := range pm.tick(now) {
			if a.kind == actPathChange && !a.direct {
				lostAt = now
			}
		}
	}
	if lostAt.IsZero() || lostAt.Sub(t0) < tm.DirectTimeout || lostAt.Sub(t0) > tm.DirectTimeout+200*time.Millisecond {
		t.Fatalf("fallback at %v", lostAt.Sub(t0))
	}
	if _, direct := pm.route(peer); direct {
		t.Fatal("still direct")
	}
	// No probing until BackoffMin has passed, then a new attempt with CallMeMaybe.
	if len(pm.tick(lostAt.Add(tm.BackoffMin-time.Second))) != 0 {
		t.Fatal("re-punch before backoff")
	}
	start := lostAt.Add(tm.BackoffMin)
	k := kinds(pm.tick(start))
	if k[actCallMeMaybe] != 1 || k[actPing] != 1 {
		t.Fatalf("re-punch: %v", k)
	}
	// Failed attempts back off exponentially up to BackoffMax.
	var gaps []time.Duration
	now = start
	last := start
	for i := 0; i < 20000 && len(gaps) < 6; i++ {
		now = now.Add(time.Second)
		for _, a := range pm.tick(now) {
			if a.kind == actCallMeMaybe {
				gaps = append(gaps, now.Sub(last))
				last = now
			}
		}
	}
	// attempt lasts PunchWindow; next starts backoff after it ends
	want := []time.Duration{60, 120, 240, 300, 300, 300}
	for i, g := range gaps {
		if w := want[i]*time.Second + tm.PunchWindow; g != w {
			t.Fatalf("gap %d = %v, want %v (all %v)", i, g, w, gaps)
		}
	}
	// A CallMeMaybe from the peer starts probing at once (both sides must send).
	acts := pm.onCallMeMaybe(peer, []netip.AddrPort{candA}, true, "", now.Add(time.Second))
	if kinds(acts)[actCallMeMaybe] != 1 || !pm.punching(pm.peers[peer], now.Add(time.Second)) {
		t.Fatalf("cmm from peer: %+v", acts)
	}
	// Success resets the backoff.
	p = pings(pm.tick(now.Add(time.Second)))
	pm.onPong(peer, p[0].tx, now.Add(time.Second+time.Millisecond))
	if pm.peers[peer].backoff != tm.BackoffMin {
		t.Fatal("backoff not reset")
	}
}

func TestPathPeerReflexiveAndBetterPath(t *testing.T) {
	pm := newPathManager(tm, false)
	t0 := time.Unix(1000, 0)
	const peer relay.ID = 7
	pm.addKey(peer, testKey(1), t0)
	pm.tick(t0) // no candidates yet: nothing to probe
	pm.tick(t0.Add(tm.PunchWindow + time.Millisecond))
	// A ping arrives from an unknown address while idle: it becomes a
	// candidate and we probe it at once (and then with the attempt it
	// starts).
	now := t0.Add(2*tm.PunchWindow + time.Second)
	if p := pings(pm.onPing(peer, candA, now)); len(p) != 1 || p[0].addr != candA {
		t.Fatalf("onPing: %+v", p)
	}
	if p := pings(pm.onPing(peer, candA, now)); len(p) != 0 { // known now: no extra probe
		t.Fatalf("onPing again: %+v", p)
	}
	p := pings(pm.tick(now))
	if len(p) != 1 || p[0].addr != candA {
		t.Fatalf("probe peer-reflexive: %+v", p)
	}
	pm.onPong(peer, p[0].tx, now.Add(50*time.Millisecond))
	// A much faster address replaces the direct one; a slightly faster one doesn't.
	pm.onCallMeMaybe(peer, []netip.AddrPort{candA, candB}, false, "", now)
	pm.peers[peer].pending[txid{1}] = pendingPing{addr: candB, sent: now}
	pm.onPong(peer, txid{1}, now.Add(45*time.Millisecond))
	if a, _ := pm.route(peer); a != candA {
		t.Fatal("switched for a marginal gain")
	}
	pm.peers[peer].pending[txid{2}] = pendingPing{addr: candB, sent: now}
	pm.onPong(peer, txid{2}, now.Add(5*time.Millisecond))
	if a, _ := pm.route(peer); a != candB {
		t.Fatal("did not switch to the faster path")
	}
	// Removing the last key forgets the peer.
	pm.removeKey(peer, testKey(1).id)
	if _, direct := pm.route(peer); direct || pm.peers[peer] != nil {
		t.Fatal("peer not forgotten")
	}
}

func TestPathDisabled(t *testing.T) {
	pm := newPathManager(tm, true)
	t0 := time.Unix(1000, 0)
	if acts := pm.addKey(7, testKey(1), t0); len(acts) != 0 {
		t.Fatal("punch while disabled")
	}
	acts := pm.onCallMeMaybe(7, []netip.AddrPort{candA}, true, "", t0)
	if len(acts) != 1 || acts[0].kind != actCallMeMaybe { // still answers
		t.Fatalf("%+v", acts)
	}
	if len(pm.tick(t0.Add(time.Second))) != 0 || len(pm.onPing(7, candA, t0)) != 0 {
		t.Fatal("activity while disabled")
	}
}

func TestPathForgetsKeylessPeers(t *testing.T) {
	pm := newPathManager(tm, false)
	t0 := time.Unix(1000, 0)
	pm.onCallMeMaybe(7, []netip.AddrPort{candA}, false, "", t0)
	pm.tick(t0.Add(30 * time.Second))
	if pm.peers[7] == nil {
		t.Fatal("forgot too early")
	}
	pm.tick(t0.Add(2 * time.Minute))
	if pm.peers[7] != nil {
		t.Fatal("key-less peer kept forever")
	}
}

func TestDeadline(t *testing.T) {
	c := newVConn(nil, 4, nil)
	c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	start := time.Now()
	if _, _, err := c.ReadFrom(make([]byte, 10)); err == nil || time.Since(start) < 40*time.Millisecond {
		t.Fatalf("deadline: %v after %v", err, time.Since(start))
	}
	c.SetReadDeadline(time.Time{})
	c.push(packet{data: []byte("x")})
	if n, _, err := c.ReadFrom(make([]byte, 10)); err != nil || n != 1 {
		t.Fatal("read after clearing deadline")
	}
	c.SetReadDeadline(time.Now().Add(-time.Second)) // past: immediate
	if _, _, err := c.ReadFrom(make([]byte, 10)); err == nil {
		t.Fatal("past deadline didn't fire")
	}
	c.SetReadDeadline(time.Now().Add(time.Hour))
	c.Close()
	if _, _, err := c.ReadFrom(make([]byte, 10)); err == nil {
		t.Fatal("read after close")
	}
	for i := 0; i < 10; i++ { // overflow drops instead of blocking
		c.push(packet{data: []byte("x")})
	}
}

func TestSkipLocalIP(t *testing.T) {
	for s, skip := range map[string]bool{
		"192.168.1.20": false, "10.0.0.5": false, "127.0.0.1": false, "2001:db8::1": false,
		"100.64.0.11": true, "fd7a:115c:a1e0::9434:a909": true, "fe80::1": true, "::1": true, "0.0.0.0": true,
	} {
		if got := skipLocalIP(netip.MustParseAddr(s)); got != skip {
			t.Errorf("%s: skip=%v", s, got)
		}
	}
}
