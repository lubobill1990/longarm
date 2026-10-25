package longnet

import (
	"crypto/rand"
	"net/netip"
	"time"

	"github.com/lubobill1990/longarm/internal/relay"
)

// Timing are the path-selection timings (DESIGN §4.3 defaults).
type Timing struct {
	PunchInterval time.Duration // resend probes while punching (200 ms)
	PunchWindow   time.Duration // how long one punch attempt lasts (5 s)
	Keepalive     time.Duration // ping on the direct path (2 s)
	DirectTimeout time.Duration // no pong for this long → back to relay (6.5 s)
	BackoffMin    time.Duration // first re-punch delay after a failure (30 s)
	BackoffMax    time.Duration // re-punch delay cap (5 min)
}

// DefaultTiming is DESIGN §4.3.
func DefaultTiming() Timing {
	return Timing{
		PunchInterval: 200 * time.Millisecond,
		PunchWindow:   5 * time.Second,
		Keepalive:     2 * time.Second,
		DirectTimeout: 6500 * time.Millisecond,
		BackoffMin:    30 * time.Second,
		BackoffMax:    5 * time.Minute,
	}
}

func (t Timing) withDefaults() Timing {
	d := DefaultTiming()
	if t.PunchInterval <= 0 {
		t.PunchInterval = d.PunchInterval
	}
	if t.PunchWindow <= 0 {
		t.PunchWindow = d.PunchWindow
	}
	if t.Keepalive <= 0 {
		t.Keepalive = d.Keepalive
	}
	if t.DirectTimeout <= 0 {
		t.DirectTimeout = d.DirectTimeout
	}
	if t.BackoffMin <= 0 {
		t.BackoffMin = d.BackoffMin
	}
	if t.BackoffMax < t.BackoffMin {
		t.BackoffMax = max(d.BackoffMax, t.BackoffMin)
	}
	return t
}

// maxPeerCandidates bounds the addresses probed per peer; learned
// (peer-reflexive) candidates without a pong expire after learnedTTL.
const (
	maxPeerCandidates = 16
	learnedTTL        = 2 * time.Minute
)

// validCandidate rejects addresses that can never be a peer's socket.
func validCandidate(a netip.AddrPort) bool {
	ip := a.Addr()
	return a.IsValid() && a.Port() != 0 && !ip.IsUnspecified() && !ip.IsMulticast() &&
		ip != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

type actionKind int

const (
	actPing        actionKind = iota // send a disco ping (tx) to addr with key
	actCallMeMaybe                   // send our candidates to peer via the relay (reply flag)
	actPathChange                    // informational: peer's path changed (direct/addr/rtt)
	actPunchFailed                   // informational: an attempt ended without a pong (stats)
)

type action struct {
	kind   actionKind
	peer   relay.ID
	addr   netip.AddrPort
	tx     txid
	key    discoKey
	seq    uint64
	reply  bool
	direct bool
	rtt    time.Duration
	stats  punchStats // actPunchFailed
}

// punchStats summarizes one punch attempt (logged when it fails).
type punchStats struct {
	cands     []netip.AddrPort
	pingsSent int
	pingsRecv int // verified pings from the peer during the attempt
	learned   int // new addresses learned from those pings
	selfNAT   string
	peerNAT   string
}

type candidate struct {
	fromCMM  bool // advertised via CallMeMaybe (otherwise learned from a ping's source)
	added    time.Time
	lastPong time.Time
	rtt      time.Duration
}

type pendingPing struct {
	addr netip.AddrPort
	sent time.Time
}

// peerPath is the path state for one peer.
//
//	          key added / CallMeMaybe / ping from peer
//	relay ─────────────────────────────────────────────▶ punching (probe every PunchInterval for PunchWindow)
//	  ▲  ◀── window over: next attempt after backoff ──────┘ │
//	  │      (BackoffMin, doubling up to BackoffMax)          │ pong
//	  │                                                       ▼
//	  └──────── no pong for DirectTimeout ─────────────── direct (keepalive every Keepalive)
//	            (next attempt after BackoffMin)
//
// Hard × hard NAT (both sides' STUN say the mapping varies by destination):
// no probing; a real attempt only every BackoffMax (see pointless).
type peerPath struct {
	keys  []discoKey // newest last
	cands map[netip.AddrPort]*candidate

	direct         bool
	directAddr     netip.AddrPort
	directRTT      time.Duration
	lastDirectPong time.Time
	nextKeepalive  time.Time

	punchUntil     time.Time // non-zero while a punch attempt runs (or just ended)
	lastPunchStart time.Time
	lastPunchPing  time.Time
	nextPunch      time.Time // when to start the next attempt (zero = not scheduled)
	backoff        time.Duration
	forced         bool       // the running attempt is a scheduled retry (not aborted by hard × hard)
	stats          punchStats // of the running attempt

	peerNAT string // from the peer's CallMeMaybe

	pending map[txid]pendingPing
	touched time.Time // last CallMeMaybe (forgets key-less peers)

	sendSeq uint64             // last ping sequence number sent
	recvSeq map[[8]byte]uint64 // highest ping sequence number seen per key
}

// pathManager is the per-node path-selection state machine. It does no I/O
// and reads no clock: callers pass now and execute the returned actions, so
// it is unit-testable. Not safe for concurrent use.
type pathManager struct {
	t        Timing
	disabled bool // LONGARM_NET_DISABLE_DIRECT: never probe, always relay
	peers    map[relay.ID]*peerPath
	selfNAT  string // our NAT type (relay.NAT*), from STUN

	selfPublic []netip.AddrPort // our STUN-mapped addresses
	selfV6     bool             // we have a global IPv6 address
}

// pointless reports hard × hard NAT: both mappings vary by destination, so
// neither side can learn the port the other's NAT uses toward it and probes
// can't meet (short of port guessing, which we don't do). Such peers stay
// relayed; a real attempt is only retried every hardRetry. Not pointless
// after all: peers behind the same public IP (same NAT: LAN candidates
// work) and peers that both have global IPv6 (no NAT on that path).
func (pm *pathManager) pointless(p *peerPath) bool {
	if pm.selfNAT != relay.NATHard || p.peerNAT != relay.NATHard {
		return false
	}
	for a, c := range p.cands {
		if !c.fromCMM {
			continue
		}
		if pm.selfV6 && publicV6(a) {
			return false
		}
		for _, m := range pm.selfPublic {
			if m.Addr() == a.Addr() && !a.Addr().IsLoopback() {
				return false
			}
		}
	}
	return true
}

func (pm *pathManager) hardRetry() time.Duration { return pm.t.BackoffMax }

func newPathManager(t Timing, disabled bool) *pathManager {
	return &pathManager{t: t.withDefaults(), disabled: disabled, peers: map[relay.ID]*peerPath{}}
}

func (pm *pathManager) peer(id relay.ID) *peerPath {
	p := pm.peers[id]
	if p == nil {
		p = &peerPath{cands: map[netip.AddrPort]*candidate{}, pending: map[txid]pendingPing{}, backoff: pm.t.BackoffMin,
			recvSeq: map[[8]byte]uint64{}}
		pm.peers[id] = p
	}
	return p
}

// keys lists the disco keys of a peer (for verifying its messages).
func (pm *pathManager) keys(id relay.ID) []discoKey {
	if p := pm.peers[id]; p != nil {
		return p.keys
	}
	return nil
}

// addKey registers the disco key of a new QUIC connection with id and starts
// punching if we're not direct yet.
func (pm *pathManager) addKey(id relay.ID, k discoKey, now time.Time) []action {
	p := pm.peer(id)
	for _, e := range p.keys {
		if e.id == k.id {
			return nil
		}
	}
	p.keys = append(p.keys, k)
	if pm.disabled || p.direct || pm.punching(p, now) {
		return nil
	}
	if pm.pointless(p) { // known from an earlier connection: just exchange candidates
		if p.nextPunch.IsZero() {
			p.nextPunch = now.Add(pm.hardRetry())
		}
		return []action{{kind: actCallMeMaybe, peer: id, reply: true}}
	}
	return pm.startPunch(id, p, now, true, true)
}

// removeKey drops a key when its connection ends; a peer without keys is
// forgotten (packets to it go via the relay).
func (pm *pathManager) removeKey(id relay.ID, kid [8]byte) {
	p := pm.peers[id]
	if p == nil {
		return
	}
	for i, e := range p.keys {
		if e.id == kid {
			p.keys = append(p.keys[:i:i], p.keys[i+1:]...)
			break
		}
	}
	delete(p.recvSeq, kid)
	if len(p.keys) == 0 {
		delete(pm.peers, id)
	}
}

func (pm *pathManager) punching(p *peerPath, now time.Time) bool {
	return !p.punchUntil.IsZero() && now.Before(p.punchUntil)
}

// startPunch begins an attempt: probes go out on the next tick, and, if cmm,
// our candidates are sent to the peer (asking for theirs when reply).
func (pm *pathManager) startPunch(id relay.ID, p *peerPath, now time.Time, cmm, reply bool) []action {
	p.punchUntil = now.Add(pm.t.PunchWindow)
	p.lastPunchStart = now
	p.lastPunchPing = time.Time{}
	p.nextPunch = time.Time{}
	p.forced = false
	p.stats = punchStats{}
	if !cmm {
		return nil
	}
	return []action{{kind: actCallMeMaybe, peer: id, reply: reply}}
}

// setSelfNAT records our NAT type and candidates changing (STUN): peers we
// aren't direct with get our new candidates, and an attempt unless one ran
// recently or the pair is hard × hard.
func (pm *pathManager) setSelfNAT(nat string, now time.Time) []action {
	pm.selfNAT = nat
	if pm.disabled {
		return nil
	}
	var acts []action
	for id, p := range pm.peers {
		if len(p.keys) == 0 || p.direct {
			continue
		}
		switch {
		case pm.pointless(p):
			if pm.punching(p, now) && !p.forced {
				pm.abortPunch(p, now)
			}
			acts = append(acts, action{kind: actCallMeMaybe, peer: id})
		case pm.punching(p, now) || !pm.peerMayTrigger(p, now):
			acts = append(acts, action{kind: actCallMeMaybe, peer: id})
		default:
			acts = append(acts, pm.startPunch(id, p, now, true, true)...)
		}
	}
	return acts
}

// abortPunch ends a pointless (hard × hard) attempt; the next one is a rare
// scheduled retry.
func (pm *pathManager) abortPunch(p *peerPath, now time.Time) {
	p.punchUntil = time.Time{}
	if p.nextPunch.IsZero() || p.nextPunch.Before(now.Add(pm.hardRetry())) {
		p.nextPunch = now.Add(pm.hardRetry())
	}
}

// onCallMeMaybe handles candidates delivered by the relay; nat is the
// peer's NAT type.
func (pm *pathManager) onCallMeMaybe(id relay.ID, cands []netip.AddrPort, reply bool, nat string, now time.Time) []action {
	if pm.disabled {
		if reply { // still answer, so a peer with direct enabled isn't confused
			return []action{{kind: actCallMeMaybe, peer: id}}
		}
		return nil
	}
	p := pm.peer(id)
	p.touched = now
	if relay.ValidNAT(nat) {
		p.peerNAT = nat
	}
	for a, c := range p.cands {
		if c.fromCMM {
			if c.lastPong.IsZero() && (!p.direct || a != p.directAddr) {
				delete(p.cands, a)
			} else {
				c.fromCMM = false // keep a working address, now as learned
			}
		}
	}
	for _, a := range cands {
		a = netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
		if validCandidate(a) {
			pm.addCand(p, a, true, now)
		}
	}
	var acts []action
	if reply {
		acts = append(acts, action{kind: actCallMeMaybe, peer: id})
	}
	if p.direct || len(p.keys) == 0 {
		return acts
	}
	if pm.pointless(p) {
		// Hard × hard: stop our own (unforced) attempt; join the peer's
		// only when it is a rare scheduled retry, i.e. we haven't tried for
		// a while either.
		if pm.punching(p, now) {
			if !p.forced {
				pm.abortPunch(p, now)
			}
		} else if reply && (p.lastPunchStart.IsZero() || now.Sub(p.lastPunchStart) >= pm.hardRetry()/2) {
			acts = append(acts, pm.startPunch(id, p, now, false, false)...)
			p.forced = true
		}
		return acts
	}
	// The peer is starting an attempt: probe at the same time (hole
	// punching needs both sides sending) — but at most once per two punch
	// windows, so a peer can't make us probe continuously.
	if !pm.punching(p, now) && pm.peerMayTrigger(p, now) {
		acts = append(acts, pm.startPunch(id, p, now, false, false)...)
	}
	return acts
}

func (pm *pathManager) peerMayTrigger(p *peerPath, now time.Time) bool {
	return p.lastPunchStart.IsZero() || now.Sub(p.lastPunchStart) >= 2*pm.t.PunchWindow
}

// addCand adds a candidate. When full, an advertised candidate evicts the
// oldest learned one (other than the direct address); learned ones are
// dropped.
func (pm *pathManager) addCand(p *peerPath, a netip.AddrPort, fromCMM bool, now time.Time) *candidate {
	if c := p.cands[a]; c != nil {
		c.fromCMM = c.fromCMM || fromCMM
		return c
	}
	if len(p.cands) >= maxPeerCandidates {
		if !fromCMM {
			return nil
		}
		var victim netip.AddrPort
		var oldest time.Time
		for va, c := range p.cands {
			if c.fromCMM || (p.direct && va == p.directAddr) {
				continue
			}
			if !victim.IsValid() || c.added.Before(oldest) {
				victim, oldest = va, c.added
			}
		}
		if !victim.IsValid() {
			return nil
		}
		delete(p.cands, victim)
	}
	c := &candidate{fromCMM: fromCMM, added: now}
	p.cands[a] = c
	return c
}

// onPing handles a verified ping from id received from addr. The caller
// answers with a pong; a source address we didn't know becomes a candidate
// (peer-reflexive), and we start probing too if idle.
func (pm *pathManager) onPing(id relay.ID, from netip.AddrPort, now time.Time) []action {
	if pm.disabled {
		return nil
	}
	p := pm.peers[id]
	if p == nil || len(p.keys) == 0 {
		return nil
	}
	from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
	p.stats.pingsRecv++
	var acts []action
	if validCandidate(from) && p.cands[from] == nil {
		// A new address (peer-reflexive): the peer's NAT mapping toward
		// us, which no rendezvous could have told us (it differs per
		// destination behind a hard NAT). The caller already answers to
		// it; probe it at once too, so this side goes direct without
		// waiting for the next probe interval.
		if c := pm.addCand(p, from, false, now); c != nil {
			p.stats.learned++
			if !p.direct {
				acts = append(acts, pm.ping(id, p, from, now))
			}
		}
	}
	if !p.direct && !pm.punching(p, now) && pm.peerMayTrigger(p, now) {
		acts = append(acts, pm.startPunch(id, p, now, false, false)...)
	}
	return acts
}

// onPong handles a verified pong; it may switch the peer to direct.
func (pm *pathManager) onPong(id relay.ID, tx txid, now time.Time) []action {
	p := pm.peers[id]
	if p == nil {
		return nil
	}
	pp, ok := p.pending[tx]
	if !ok {
		return nil // unknown or replayed
	}
	delete(p.pending, tx)
	rtt := now.Sub(pp.sent)
	c := pm.addCand(p, pp.addr, false, now)
	if c == nil {
		return nil
	}
	c.lastPong, c.rtt = now, rtt
	switch {
	case !p.direct, pp.addr != p.directAddr && rtt < p.directRTT*7/10:
		p.direct, p.directAddr, p.directRTT = true, pp.addr, rtt
		p.lastDirectPong = now
		p.nextKeepalive = now.Add(pm.t.Keepalive)
		p.punchUntil, p.nextPunch, p.backoff = time.Time{}, time.Time{}, pm.t.BackoffMin
		return []action{{kind: actPathChange, peer: id, direct: true, addr: pp.addr, rtt: rtt}}
	case pp.addr == p.directAddr:
		p.lastDirectPong = now
		p.directRTT = (p.directRTT*3 + rtt) / 4
	}
	return nil
}

func newTxid() txid {
	var t txid
	rand.Read(t[:])
	return t
}

func (pm *pathManager) ping(id relay.ID, p *peerPath, addr netip.AddrPort, now time.Time) action {
	t := newTxid()
	p.pending[t] = pendingPing{addr: addr, sent: now}
	p.sendSeq++
	return action{kind: actPing, peer: id, addr: addr, tx: t, key: p.keys[len(p.keys)-1], seq: p.sendSeq}
}

// acceptPing is the replay check for a verified ping: its sequence number
// must be higher than any seen under the same key (late, reordered pings are
// dropped like lost ones).
func (pm *pathManager) acceptPing(id relay.ID, kid [8]byte, seq uint64) bool {
	p := pm.peers[id]
	if p == nil {
		return false
	}
	if seq <= p.recvSeq[kid] {
		return false
	}
	p.recvSeq[kid] = seq
	return true
}

// tick advances timers: probes, keepalives, timeouts, backoff.
func (pm *pathManager) tick(now time.Time) []action {
	if pm.disabled {
		return nil
	}
	var acts []action
	expire := max(pm.t.PunchWindow, pm.t.DirectTimeout)
	for id, p := range pm.peers {
		for t, pp := range p.pending {
			if now.Sub(pp.sent) > expire {
				delete(p.pending, t)
			}
		}
		for a, c := range p.cands {
			if !c.fromCMM && now.Sub(c.added) > learnedTTL && now.Sub(c.lastPong) > learnedTTL &&
				(!p.direct || a != p.directAddr) {
				delete(p.cands, a)
			}
		}
		if len(p.keys) == 0 {
			if now.Sub(p.touched) > time.Minute { // candidates for a connection that never came
				delete(pm.peers, id)
			}
			continue
		}
		if p.direct {
			if now.Sub(p.lastDirectPong) > pm.t.DirectTimeout {
				// Direct path lost: back to the relay; retry later.
				// Losing the path counts as a failed attempt: retry after
				// BackoffMin, then 2×, 4× … up to BackoffMax.
				p.direct = false
				p.nextPunch = now.Add(pm.t.BackoffMin)
				p.backoff = min(2*pm.t.BackoffMin, pm.t.BackoffMax)
				p.punchUntil = time.Time{}
				acts = append(acts, action{kind: actPathChange, peer: id, direct: false, addr: p.directAddr})
				continue
			}
			if !now.Before(p.nextKeepalive) {
				p.nextKeepalive = now.Add(pm.t.Keepalive)
				acts = append(acts, pm.ping(id, p, p.directAddr, now))
			}
			continue
		}
		if !p.punchUntil.IsZero() && !now.Before(p.punchUntil) {
			// Attempt failed: schedule the next one with exponential backoff
			// (hard × hard: rarely).
			p.punchUntil = time.Time{}
			delay := p.backoff
			if pm.pointless(p) {
				delay = max(delay, pm.hardRetry())
			}
			p.nextPunch = now.Add(delay)
			p.backoff = min(2*p.backoff, pm.t.BackoffMax)
			st := p.stats
			st.selfNAT, st.peerNAT = pm.selfNAT, p.peerNAT
			for a := range p.cands {
				st.cands = append(st.cands, a)
			}
			acts = append(acts, action{kind: actPunchFailed, peer: id, stats: st})
		}
		if p.punchUntil.IsZero() && !p.nextPunch.IsZero() && !now.Before(p.nextPunch) {
			forced := pm.pointless(p)
			acts = append(acts, pm.startPunch(id, p, now, true, true)...)
			p.forced = forced
		}
		if pm.punching(p, now) && (p.lastPunchPing.IsZero() || now.Sub(p.lastPunchPing) >= pm.t.PunchInterval) {
			p.lastPunchPing = now
			for a := range p.cands {
				acts = append(acts, pm.ping(id, p, a, now))
				p.stats.pingsSent++
			}
		}
	}
	return acts
}

// route returns where packets to id go: a direct address, or the relay.
func (pm *pathManager) route(id relay.ID) (netip.AddrPort, bool) {
	if p := pm.peers[id]; p != nil && p.direct {
		return p.directAddr, true
	}
	return netip.AddrPort{}, false
}

// peerNAT is the NAT type the peer reported.
func (pm *pathManager) peerNAT(id relay.ID) string {
	if p := pm.peers[id]; p != nil {
		return p.peerNAT
	}
	return relay.NATUnknown
}

// directInfo is the direct path state of id.
func (pm *pathManager) directInfo(id relay.ID) (addr netip.AddrPort, rtt time.Duration, ok bool) {
	if p := pm.peers[id]; p != nil && p.direct {
		return p.directAddr, p.directRTT, true
	}
	return netip.AddrPort{}, 0, false
}
