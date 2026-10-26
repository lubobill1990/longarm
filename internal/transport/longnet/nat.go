package longnet

import (
	"context"
	"crypto/rand"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/lubobill1990/longarm/internal/relay"
)

// NAT discovery. The node sends STUN binding requests from its one UDP
// socket (the one used for the relay session and the direct paths) to every
// STUN endpoint the relay advertises (its QUIC port and a second port) plus
// any configured servers, every STUNInterval and after each relay connect.
//
//   - The answers are our public (reflexive) addresses: they become
//     candidates, also when the relay session runs over TCP (the relay can
//     only observe a UDP address for UDP sessions).
//   - Comparing them gives the NAT type (as in tailscale netcheck's
//     MappingVariesByDestIP): one address for every destination = easy,
//     different ones = hard (symmetric), our own local address = open.
//   - A server on a public address that answers with a private address is
//     behind source NAT itself (a hub in Azure Container Instances sees
//     every client at an internal address): such answers are ignored, for
//     candidates and for the NAT type (see usableSTUN, classifyNAT). The
//     relay notices the same situation and stops advertising its own STUN
//     ports and its observed addresses (relay.snatVerdict); external STUN
//     servers (relay_stun_advertise, LONGARM_NET_STUN) then do the job.
//   - A pong whose "observed" address has our public IP but another port
//     hints that the mapping changed or varies: it only triggers an early
//     STUN round (a NAT rebinding looks the same, and a peer could lie).
//
// Hard × hard pairs don't punch (see pathManager.pointless); everything else
// does.

const (
	stunTries      = 3
	maxSTUNServers = 4                // bounds what a relay (or config) can make us query
	stunKickEvery  = 10 * time.Second // early rounds triggered by pongs, at most this often
)

type natState struct {
	pending map[relay.STUNTxID]netip.AddrPort // outstanding request → server
	obs     map[netip.AddrPort]netip.AddrPort // server → mapped address, this round
	want    int
	got     chan struct{}

	typ        string
	mapped     []netip.AddrPort
	primary    netip.AddrPort // mapping toward the first STUN server (the relay's UDP port: what peers learn from the relay)
	primarySrv netip.AddrPort
	cands      []netip.AddrPort // what we advertise (mapped first, then local)
	sentTo     *relay.Conn      // relay session that has our latest NodeInfo
	lastKick   time.Time
	snatLogged map[netip.AddrPort]bool // STUN servers whose SNATed answers we logged (once per server)
}

// usableSTUN reports whether a STUN answer (server → mapped) is a real
// reflexive address. A private (non-public: RFC 1918, CGNAT, link-local,
// loopback, ULA, ...) mapped address from a server at a public address is
// not: on the Internet nobody can see us from a private address, so the
// server sits behind its hosting platform's source NAT (e.g. Azure
// Container Instances answers every client with an internal 10.x address)
// and reports the platform's address, not ours. A private mapped address
// from a server at a private or loopback address is legitimate: a hub on
// the LAN, or the tests, where everything runs on 127.0.0.1. So is one
// equal to one of our own socket addresses (ip and port): no NAT between
// us and the server (a routed private network), "open" as before — a
// platform's SNAT can't come up with exactly our address.
func usableSTUN(server, mapped netip.AddrPort, local []netip.AddrPort) bool {
	return mapped.IsValid() && (relay.PublicAddr(mapped.Addr()) || !relay.PublicAddr(server.Addr()) || slices.Contains(local, mapped))
}

// classifyNAT derives the NAT type from STUN observations (server →
// mapped address) and returns it with the distinct usable mapped addresses
// (our reflexive candidates).
//
//   - Unusable answers (see usableSTUN) are ignored altogether.
//   - When some answers are public addresses, only those are compared:
//     a private answer (from a LAN server) next to public ones (from
//     Internet servers) says nothing about the NAT's mapping behaviour.
//   - Among the compared answers: one equal to a local address = open; two
//     or more of one address family all equal = easy, different = hard;
//     fewer than two = unknown (punch normally). Families are not mixed.
func classifyNAT(obs map[netip.AddrPort]netip.AddrPort, local []netip.AddrPort) (string, []netip.AddrPort) {
	use := map[netip.AddrPort]netip.AddrPort{}
	var mapped []netip.AddrPort
	anyPublic := false
	for s, m := range obs {
		if !usableSTUN(s, m, local) {
			continue
		}
		use[s] = m
		anyPublic = anyPublic || relay.PublicAddr(m.Addr())
		if !slices.Contains(mapped, m) {
			mapped = append(mapped, m)
		}
	}
	slices.SortFunc(mapped, func(a, b netip.AddrPort) int { return a.Compare(b) })
	if anyPublic {
		for s, m := range use {
			if !relay.PublicAddr(m.Addr()) {
				delete(use, s)
			}
		}
	}
	for _, m := range use {
		if slices.Contains(local, m) {
			return relay.NATOpen, mapped
		}
	}
	typ := relay.NATUnknown
	for _, v6 := range []bool{false, true} {
		var points int
		var distinct []netip.AddrPort
		for _, m := range use {
			if m.Addr().Is6() != v6 {
				continue
			}
			points++
			if !slices.Contains(distinct, m) {
				distinct = append(distinct, m)
			}
		}
		switch {
		case len(distinct) > 1:
			return relay.NATHard, mapped
		case points >= 2:
			typ = relay.NATEasy
		}
	}
	return typ, mapped
}

// mergeCandidates lists mapped addresses first, then local ones (no
// duplicates, at most 16).
func mergeCandidates(mapped, local []netip.AddrPort) []netip.AddrPort {
	var out []netip.AddrPort
	for _, a := range append(append([]netip.AddrPort{}, mapped...), local...) {
		if !slices.Contains(out, a) && len(out) < maxPeerCandidates {
			out = append(out, a)
		}
	}
	return out
}

func (n *Node) kickSTUN() {
	select {
	case n.stunKick <- struct{}{}:
	default:
	}
}

func (n *Node) stunLoop() {
	defer n.wg.Done()
	t := time.NewTimer(n.opts.STUNInterval)
	defer t.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-n.stunKick:
		case <-t.C:
		}
		n.stunRound()
		t.Reset(n.opts.STUNInterval)
	}
}

// stunServers resolves the STUN endpoints of the current relay session and
// the configured extra servers (at most maxSTUNServers of each). ":port"
// means the relay address we dialed, so that the relay's QUIC port is the
// very address our relay session's mapping points at.
func (n *Node) stunServers(ctx context.Context) []netip.AddrPort {
	n.mu.Lock()
	rc := n.rc
	n.mu.Unlock()
	var fromRelay []string
	if rc != nil {
		fromRelay = rc.STUN()
	}
	var out []netip.AddrPort
	resolve := func(names []string) {
		for _, s := range names[:min(len(names), maxSTUNServers)] {
			host, port, err := net.SplitHostPort(s)
			if err != nil {
				continue
			}
			var ua netip.AddrPort
			if host == "" { // the relay's own IP
				base := netip.AddrPort{}
				if ra := n.relayUDP.Load(); ra != nil {
					base = udpAddrPort(ra)
				} else if base, err = resolveUDP(ctx, n.opts.Relay); err != nil {
					continue
				}
				pn, err := net.DefaultResolver.LookupPort(ctx, "udp", port)
				if err != nil {
					continue
				}
				ua = netip.AddrPortFrom(base.Addr(), uint16(pn))
			} else if ua, err = resolveUDP(ctx, s); err != nil {
				n.log.Debug("longnet: STUN server", "server", s, "err", err)
				continue
			}
			if validCandidate(ua) && !slices.Contains(out, ua) {
				out = append(out, ua)
			}
		}
	}
	resolve(fromRelay)
	resolve(n.opts.STUN)
	return out
}

func resolveUDP(ctx context.Context, hostport string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if ap, err := netip.ParseAddrPort(hostport); err == nil {
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	pn, err := net.DefaultResolver.LookupPort(ctx, "udp", port)
	if err != nil {
		return netip.AddrPort{}, err
	}
	slices.SortStableFunc(ips, func(a, b netip.Addr) int { // IPv4 first
		switch {
		case a.Unmap().Is4() && !b.Unmap().Is4():
			return -1
		case !a.Unmap().Is4() && b.Unmap().Is4():
			return 1
		}
		return 0
	})
	if len(ips) == 0 {
		return netip.AddrPort{}, &net.DNSError{Err: "no addresses", Name: host}
	}
	return netip.AddrPortFrom(ips[0].Unmap(), uint16(pn)), nil
}

// stunRound queries every STUN server (with retries) and re-evaluates the
// NAT type and candidates.
func (n *Node) stunRound() {
	servers := n.stunServers(n.ctx)
	got := make(chan struct{}, 1)
	n.mu.Lock()
	n.nat.pending = map[relay.STUNTxID]netip.AddrPort{}
	n.nat.obs = map[netip.AddrPort]netip.AddrPort{}
	n.nat.want, n.nat.got = len(servers), got
	// The primary server is the relay's QUIC port (the mapping peers learn
	// from the relay), whatever order the relay lists its endpoints in.
	n.nat.primarySrv = netip.AddrPort{}
	if len(servers) > 0 {
		n.nat.primarySrv = servers[0]
		if ra := n.relayUDP.Load(); ra != nil && slices.Contains(servers, udpAddrPort(ra)) {
			n.nat.primarySrv = udpAddrPort(ra)
		}
	}
	n.mu.Unlock()
	for try := 0; try < stunTries && len(servers) > 0; try++ {
		var reqs [][]byte
		var dsts []netip.AddrPort
		n.mu.Lock()
		for _, s := range servers {
			if _, ok := n.nat.obs[s]; ok {
				continue
			}
			var tx relay.STUNTxID
			rand.Read(tx[:])
			n.nat.pending[tx] = s
			reqs, dsts = append(reqs, relay.STUNRequest(tx)), append(dsts, s)
		}
		n.mu.Unlock()
		if len(reqs) == 0 {
			break
		}
		for i := range reqs {
			n.sock.WriteTo(reqs[i], net.UDPAddrFromAddrPort(dsts[i]))
		}
		select {
		case <-n.ctx.Done():
			return
		case <-got:
		case <-time.After(time.Duration(try+1) * 500 * time.Millisecond):
		}
	}
	n.mu.Lock()
	obs := n.nat.obs
	n.nat.pending, n.nat.obs, n.nat.got = nil, nil, nil
	n.mu.Unlock()
	n.evaluateNAT(obs, len(obs) < len(servers))
}

// handleSTUN processes a STUN response read from the socket.
func (n *Node) handleSTUN(b []byte, from netip.AddrPort) {
	tx, mapped, err := relay.ParseSTUNResponse(b)
	if err != nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	srv, ok := n.nat.pending[tx]
	if !ok || srv != from {
		return
	}
	delete(n.nat.pending, tx)
	n.nat.obs[srv] = netip.AddrPortFrom(mapped.Addr().Unmap(), mapped.Port())
	if len(n.nat.obs) >= n.nat.want {
		select {
		case n.nat.got <- struct{}{}:
		default:
		}
	}
}

// notePeerObserved checks a pong's observed address (our address as the
// peer saw our ping). Our public IP with a port that isn't a known mapping
// means the NAT rebound us or maps per destination: run STUN early (rate
// limited) to find out. It never decides the NAT type by itself — a
// rebinding looks the same, and a peer could send anything.
func (n *Node) notePeerObserved(obs netip.AddrPort) {
	obs = netip.AddrPortFrom(obs.Addr().Unmap(), obs.Port())
	n.mu.Lock()
	kick := false
	if !slices.Contains(n.nat.mapped, obs) && time.Since(n.nat.lastKick) >= stunKickEvery {
		for _, m := range n.nat.mapped {
			if m.Addr() == obs.Addr() {
				kick = true
			}
		}
	}
	if kick {
		n.nat.lastKick = time.Now()
	}
	n.mu.Unlock()
	if kick {
		n.kickSTUN()
	}
}

// publicV6 reports whether a is a global IPv6 address (no NAT traversal
// needed between two of them, firewalls permitting).
func publicV6(a netip.AddrPort) bool {
	ip := a.Addr()
	return ip.Is6() && !ip.Is4In6() && ip.IsGlobalUnicast() && !ip.IsPrivate()
}

// evaluateNAT updates the NAT type and candidates from a STUN round. The
// relay always gets the new state; peers get it (and a punch attempt) only
// when it matters for them: the NAT type or our mapping toward the relay —
// the address they learn from it — changed. Other candidate churn (e.g. a
// hard NAT giving the second STUN port a new mapping every round) waits for
// the next regular attempt, so it can't bypass the punch backoff.
//
// An incomplete round (lost answers, a STUN server rate-limiting us) never
// downgrades what we know: no answers at all keep the previous state, and
// a type that can't be determined keeps the previous type.
func (n *Node) evaluateNAT(obs map[netip.AddrPort]netip.AddrPort, incomplete bool) {
	local := n.localCandidates()
	n.mu.Lock()
	rc := n.rc
	primary := obs[n.nat.primarySrv]
	if ra := n.relayUDP.Load(); ra != nil {
		if len(obs) == 0 && !incomplete && rc != nil && rc.Kind() == "udp" && rc.Reflexive().IsValid() {
			// No STUN servers (an old relay): the relay
			// session's own observation still gives one public address.
			// Not mixed with STUN answers: it is fixed at connect time
			// and goes stale when the NAT rebinds.
			primary = rc.Reflexive()
			obs[udpAddrPort(ra)] = primary
		}
	}
	// SNATed answers (see usableSTUN) are no candidates and don't count:
	// with fewer than two usable ones the type stays unknown.
	for s, m := range obs {
		if usableSTUN(s, m, local) {
			continue
		}
		delete(obs, s)
		if s == n.nat.primarySrv || m == primary {
			primary = netip.AddrPort{}
		}
		if n.nat.snatLogged == nil {
			n.nat.snatLogged = map[netip.AddrPort]bool{}
		}
		if !n.nat.snatLogged[s] {
			n.nat.snatLogged[s] = true
			n.log.Info("longnet: ignoring STUN answers of a server on a public address that sees us at a private one "+
				"(source NAT in front of the STUN server, e.g. a hub in a cloud container); "+
				"configure external STUN servers (hub relay_stun_advertise, LONGARM_NET_STUN) for direct connections",
				"server", s, "mapped", m)
		}
	}
	typ, mapped := classifyNAT(obs, local)
	if incomplete && len(obs) == 0 && len(n.nat.mapped) > 0 {
		typ, mapped = n.nat.typ, n.nat.mapped
	} else if incomplete && typ == relay.NATUnknown {
		typ = n.nat.typ
	}
	cands := mergeCandidates(mapped, local)
	changed := typ != n.nat.typ || !slices.Equal(cands, n.nat.cands)
	restart := typ != n.nat.typ || (primary.IsValid() && primary != n.nat.primary)
	n.nat.typ, n.nat.mapped, n.nat.cands = typ, mapped, cands
	if primary.IsValid() {
		n.nat.primary = primary
	}
	n.pm.selfPublic, n.pm.selfV6 = mapped, slices.ContainsFunc(local, publicV6)
	var acts []action
	if restart {
		acts = n.pm.setSelfNAT(typ, time.Now())
	}
	sendInfo := rc != nil && (changed || n.nat.sentTo != rc)
	if sendInfo {
		n.nat.sentTo = rc
	}
	n.mu.Unlock()
	if changed {
		n.log.Info("longnet: NAT", "type", natName(typ), "mapped", mapped, "candidates", cands)
	}
	if sendInfo {
		rc.SendNodeInfo(relay.NodeInfo{Candidates: cands, NAT: typ, Mapped: mapped})
	}
	n.run(acts)
}

func natName(t string) string {
	if t == relay.NATUnknown {
		return "unknown"
	}
	return t
}

// NAT reports this node's NAT type ("open", "easy", "hard" or "" =
// unknown) and its STUN-observed public addresses.
func (n *Node) NAT() (string, []netip.AddrPort) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.nat.typ, slices.Clone(n.nat.mapped)
}
