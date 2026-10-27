package client

// Transport racing (DESIGN §4.1): a worker may be reachable over several
// legs (tcp, tailscale, longnet). Each leg is dialed and the app-level mTLS
// handshake is completed on it; the first leg with a finished handshake wins
// and the others are cancelled (or closed if they finished too late).
//
// Policies:
//   - race (default): all legs at once. When an earlier process already found
//     a winner for this worker (the sticky choice in transport-cache.json,
//     younger than [dial] reevaluate), that leg gets a head start - the
//     others only start if it hasn't finished its handshake within
//     2x its last handshake time (200 ms .. 2 s) or fails. This keeps a
//     working choice stable and cheap (no tsnet start-up for every longarm call)
//     while still surviving the loss of that leg.
//     When longarm daemon measures the legs (Dialer.LiveLegs), the sticky leg
//     loses its head start once its path got worse than when it won
//     (direct -> relay/derp/peer-relay), it is down, or another live leg
//     is clearly better (a better path kind unless slower, or >20 % and
//     >5 ms faster); without a trusted sticky winner the best measured leg
//     gets the head start instead. A sticky leg that
//     still wins over a worse path is no longer trusted for the next dial.
//   - tcp | tailscale | longnet: prefer that transport; the other legs start
//     only after the preferred ones failed or took longer than 2 s.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"

	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/transport"
)

// Policies.
const (
	PolicyRace = "race"
)

// DialConfig is the [dial] section of workers.toml.
type DialConfig struct {
	// Policy is no longer read here: the transport policy is the
	// settings.json setting (`longarm transport`); LoadConfig points there.
	Policy string `toml:"policy"`
	// ReEvaluate: how long a sticky winner is trusted before all legs race
	// again (default 10m; "0s" = always race).
	ReEvaluate string `toml:"reevaluate"`
}

// Leg timing knobs (variables for tests).
var (
	DefaultReEvaluate = 10 * time.Minute
	minHeadStart      = 200 * time.Millisecond
	maxHeadStart      = 2 * time.Second
	preferGrace       = 2 * time.Second
	// tsnetStartGrace extends preferGrace while the embedded tailscale node
	// of this process is not up yet (a fresh longarm process starts it first).
	tsnetStartGrace = 10 * time.Second
)

// CacheFile remembers the winning leg per worker across longarm invocations.
const CacheFile = "transport-cache.json"

// Endpoint is one way to reach a worker.
type Endpoint struct {
	Transport string // tcp | tailscale | longnet
	Address   string // host:port; relay host:port for longnet
}

// Key identifies the endpoint ("tcp://127.0.0.1:7701").
func (e Endpoint) Key() string { return e.Transport + "://" + e.Address }

// ParseEndpoint parses an endpoint URL as advertised by longarmd:
// tcp://host:port, tailscale://host[:port], longnet://relay:port[?name=..].
func ParseEndpoint(s string) (Endpoint, error) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return Endpoint{}, fmt.Errorf("endpoint %q: want scheme://host[:port]", s)
	}
	switch u.Scheme {
	case "tcp", "longnet":
		return Endpoint{Transport: u.Scheme, Address: u.Host}, nil
	case "tailscale", "tsnet":
		return Endpoint{Transport: "tailscale", Address: u.Host}, nil
	}
	return Endpoint{}, fmt.Errorf("endpoint %q: unknown transport %q", s, u.Scheme)
}

// Legs is the worker's primary endpoint followed by its alternatives,
// without duplicates.
func (w Worker) Legs() []Endpoint {
	var out []Endpoint
	seen := map[string]bool{}
	add := func(e Endpoint) {
		if e.Address == "" || seen[e.Key()] {
			return
		}
		seen[e.Key()] = true
		out = append(out, e)
	}
	add(Endpoint{Transport: w.Transport, Address: w.Address})
	for _, e := range w.Alt {
		add(e)
	}
	return out
}

// LegResult is the outcome of one leg in the most recent dial.
type LegResult struct {
	Endpoint Endpoint
	Took     time.Duration
	Won      bool
	Err      string // "" = finished (lost or won); "cancelled" = still running when another won
	Path     transport.PathInfo
}

// RaceSummary formats legs like "tcp 1.2ms ✓, longnet 40ms, tailscale: no route".
func RaceSummary(legs []LegResult) string {
	var parts []string
	for _, l := range legs {
		s := l.Endpoint.Transport
		switch {
		case l.Won:
			s += fmt.Sprintf(" %s won", l.Took.Round(100*time.Microsecond))
		case l.Err == "":
			s += fmt.Sprintf(" %s", l.Took.Round(100*time.Microsecond))
		default:
			s += ": " + l.Err
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// legCache is transport-cache.json: the sticky winner per worker.
type legCache struct {
	mu      sync.Mutex
	path    string
	Workers map[string]CachedLeg `json:"workers"`
}

// CachedLeg is the last winning leg of a worker.
type CachedLeg struct {
	Leg    string    `json:"leg"`  // Endpoint.Key()
	Path   string    `json:"path"` // PathInfo at dial time
	TookMs float64   `json:"took_ms"`
	At     time.Time `json:"at"`
}

func loadLegCache(dir string) *legCache {
	c := &legCache{path: filepath.Join(dir, CacheFile), Workers: map[string]CachedLeg{}}
	if b, err := os.ReadFile(c.path); err == nil {
		json.Unmarshal(b, c)
		if c.Workers == nil {
			c.Workers = map[string]CachedLeg{}
		}
	}
	return c
}

func (c *legCache) get(worker string) (CachedLeg, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.Workers[worker]
	return e, ok
}

func (c *legCache) put(worker string, e CachedLeg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Merge with what other longarm processes wrote meanwhile.
	if b, err := os.ReadFile(c.path); err == nil {
		var disk legCache
		if json.Unmarshal(b, &disk) == nil {
			for k, v := range disk.Workers {
				if old, ok := c.Workers[k]; !ok || v.At.After(old.At) {
					c.Workers[k] = v
				}
			}
		}
	}
	c.Workers[worker] = e
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil || c.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.%d.tmp", c.path, os.Getpid())
	if os.WriteFile(tmp, b, 0o600) == nil {
		if platform.ReplaceFile(tmp, c.path) != nil {
			os.Remove(tmp)
		}
	}
}

// Sticky returns the cached winning leg of a worker (for display).
func (d *Dialer) Sticky(worker string) (CachedLeg, bool) { return d.cache.get(worker) }

// Policy is the effective dial policy.
func (d *Dialer) Policy() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.policy
}

// SetPolicy overrides the dial policy (longarm --transport, longarm daemon after
// `longarm transport`); dials in flight keep the policy they started with.
func (d *Dialer) SetPolicy(p string) error {
	p, err := NormalizePolicy(p)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.policy = p
	d.mu.Unlock()
	return nil
}

// errSkip marks a leg this client cannot use at all (not a network error).
// guess: skipped on SkipLeg's word (the hub looked down), which may be
// wrong: tried anyway when every other leg failed.
type errSkip struct {
	why   string
	guess bool
}

func (e errSkip) Error() string { return e.why }

type legOutcome struct {
	i    int
	tc   *tls.Conn
	raw  net.Conn
	pi   transport.PathInfo
	took time.Duration
	err  error
}

// race dials the worker's legs according to the policy and returns the
// first connection whose mTLS handshake completed.
func (d *Dialer) race(ctx context.Context, w Worker, explicit bool) (*tls.Conn, net.Conn, transport.PathInfo, []LegResult, error) {
	legs := w.Legs()
	results := make([]LegResult, len(legs))
	for i, e := range legs {
		results[i] = LegResult{Endpoint: e, Err: "not tried"}
	}
	if len(legs) == 0 {
		return nil, nil, transport.PathInfo{}, results, errors.New("worker has no endpoint")
	}
	// Tier 0 starts at once; tier 1 after `delay` or when tier 0 all failed.
	tier := make([]int, len(legs))
	var delay time.Duration
	policy := d.Policy()
	var sticky *CachedLeg // the cached winner that got a head start
	switch {
	case policy != PolicyRace:
		hasPreferred := false
		for i, e := range legs {
			if e.Transport != policy {
				tier[i] = 1
			} else {
				hasPreferred = true
			}
		}
		if !hasPreferred {
			for i := range tier {
				tier[i] = 0
			}
		}
		delay = preferGrace
		if policy == "tailscale" {
			switch {
			case d.proxyUp(), d.tsnetUp():
				// longarm daemon's node (kept up) or this process' own one is up.
			case d.OneShot:
				// A cold own node takes seconds to come up: race it with
				// the other legs at once instead of making the user wait
				// (longarm daemon keeps one up for the next command).
				delay = 0
			default:
				delay += tsnetStartGrace
			}
		}
	case len(legs) > 1:
		var live []LiveLeg
		if d.LiveLegs != nil {
			lctx, lcancel := context.WithTimeout(ctx, liveLegsTimeout)
			live = d.LiveLegs(lctx, w.Name)
			lcancel()
		}
		head := ""
		if c, ok := d.cache.get(w.Name); ok && d.reEvaluate > 0 && time.Since(c.At) < d.reEvaluate &&
			containsKey(legs, c.Leg) && stickyDrop(c, live, legs) == "" {
			sticky, head = &c, c.Leg
			delay = time.Duration(2 * c.TookMs * float64(time.Millisecond))
		} else if best, ok := bestLive(live, legs); ok {
			// No (trusted) sticky winner, but the daemon measures every
			// leg: the fastest one gets the head start. A blind race
			// favours legs the daemon already holds open (only the mTLS
			// handshake left) over faster ones that still need a connect.
			head = best.Leg
			delay = time.Duration(3 * best.RTTMs * float64(time.Millisecond))
		}
		if head != "" {
			for i, e := range legs {
				if e.Key() != head {
					tier[i] = 1
				}
			}
			delay = min(max(delay, minHeadStart), maxHeadStart)
		}
	}

	// The other legs must get their chance before the caller's deadline.
	if dl, ok := ctx.Deadline(); ok {
		delay = min(delay, time.Until(dl)/2)
	}
	rctx, cancel := context.WithCancel(ctx)
	out := make(chan legOutcome, len(legs))
	launched := make([]bool, len(legs))
	running := 0
	t0 := time.Now()
	launch := func(t int) {
		for i := range legs {
			if tier[i] == t && !launched[i] {
				launched[i] = true
				running++
				results[i].Err = "cancelled"
				go func() { out <- d.dialLeg(rctx, w, legs[i], explicit && i == 0, i, false) }()
			}
		}
	}
	launch(0)
	tier1 := false
	if running == 0 {
		launch(1)
		tier1 = true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	var errs []string
	var refused *TrustError // a leg reached a worker the trust policy refuses
	var guessed []int       // legs SkipLeg skipped
	lastResort := false
	finish := func() {
		// Close late winners; record nothing (the caller already returned).
		n := running
		go func() {
			for ; n > 0; n-- {
				if r := <-out; r.tc != nil {
					r.tc.Close()
				}
			}
		}()
	}
	for {
		select {
		case r := <-out:
			running--
			results[r.i].Took = r.took
			if r.err == nil {
				results[r.i].Err, results[r.i].Won, results[r.i].Path = "", true, r.pi
				cancel()
				finish()
				if len(legs) > 1 {
					// A sticky leg that won with its head start keeps the time
					// of its last full race, so re-evaluation still happens.
					d.cache.put(w.Name, CachedLeg{Leg: legs[r.i].Key(), Path: r.pi.String(),
						TookMs: float64(r.took) / float64(time.Millisecond), At: stickyAt(sticky, legs[r.i].Key(), r.pi)})
				}
				return r.tc, r.raw, r.pi, results, nil
			}
			results[r.i].Err = shortErr(r.err)
			if te := trustRefusal(w, r.err); te != nil && refused == nil {
				refused = te
			}
			if sk, skip := r.err.(errSkip); !skip {
				errs = append(errs, legs[r.i].Key()+": "+r.err.Error())
			} else {
				errs = append(errs, legs[r.i].Key()+": skipped ("+r.err.Error()+")")
				if sk.guess {
					guessed = append(guessed, r.i)
				}
			}
			if !tier1 && running == 0 {
				tier1 = true
				launch(1)
			}
			if running == 0 && !lastResort && len(guessed) > 0 {
				// Nothing else worked: the hub may only have been slow (its
				// relay up): try the legs SkipLeg ruled out after all.
				lastResort = true
				for _, i := range guessed {
					running++
					results[i].Err = "cancelled"
					go func() { out <- d.dialLeg(rctx, w, legs[i], explicit && i == 0, i, true) }()
				}
			}
			if running == 0 {
				cancel()
				if refused != nil {
					// Not a connectivity problem: say what would fix it.
					return nil, nil, transport.PathInfo{}, results, refused
				}
				return nil, nil, transport.PathInfo{}, results, fmt.Errorf("all %d transport legs failed after %s: %s",
					len(legs), time.Since(t0).Round(time.Millisecond), strings.Join(errs, "; "))
			}
		case <-timer.C:
			if !tier1 {
				tier1 = true
				launch(1)
			}
		case <-ctx.Done():
			cancel()
			finish()
			return nil, nil, transport.PathInfo{}, results, ctx.Err()
		}
	}
}

// stickyAt is the time to cache for the winner key of a race. A sticky leg
// that won with its head start keeps the time of its last full race, so
// re-evaluation still happens - unless it won over a worse path than when
// it was chosen (direct -> relay): then the next dial races every leg again.
func stickyAt(sticky *CachedLeg, key string, pi transport.PathInfo) time.Time {
	if sticky == nil || sticky.Leg != key {
		return time.Now()
	}
	if downgraded(pathKindOf(sticky.Path), pi.Kind) {
		return time.Time{}
	}
	return sticky.At
}

func containsKey(legs []Endpoint, key string) bool {
	for _, e := range legs {
		if e.Key() == key {
			return true
		}
	}
	return false
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}

// dialLeg dials one leg and completes the mTLS handshake on it.
func (d *Dialer) dialLeg(ctx context.Context, w Worker, e Endpoint, explicit bool, i int, noSkip bool) legOutcome {
	start := time.Now()
	res := legOutcome{i: i}
	tr, node, err := d.legTransport(w, e, explicit, noSkip)
	if err != nil {
		res.err, res.took = err, time.Since(start)
		return res
	}
	var raw net.Conn
	var pi transport.PathInfo
	err = ErrNoProxy
	if d.Proxy != nil && proxied(tr) {
		raw, pi, err = d.Proxy.DialLeg(ctx, w, e, explicit)
	}
	if errors.Is(err, ErrNoProxy) {
		raw, pi, err = tr.Dial(ctx, node)
	}
	if err != nil {
		res.err, res.took = err, time.Since(start)
		return res
	}
	cfg := d.tlsConfig(w)
	cfg.NextProtos = []string{"h2"}
	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		res.err, res.took = fmt.Errorf("tls: %w", err), time.Since(start)
		return res
	}
	if pi.Transport == "" {
		pi.Transport = e.Transport
	}
	res.tc, res.raw, res.pi, res.took = tc, raw, pi, time.Since(start)
	return res
}

// legTransport returns the transport and node id for one leg.
func (d *Dialer) legTransport(w Worker, e Endpoint, explicit, noSkip bool) (transport.Transport, transport.NodeID, error) {
	if d.SkipLeg != nil && !noSkip {
		if why := d.SkipLeg(e); why != "" {
			return nil, "", errSkip{why: why, guess: true}
		}
	}
	if d.LegTransport != nil {
		if tr := d.LegTransport(e); tr != nil {
			node := transport.NodeID(e.Address)
			if e.Transport == "longnet" {
				node = transport.NodeID(w.CertName)
			}
			return tr, node, nil
		}
	}
	switch e.Transport {
	case "tcp":
		return &transport.TCP{}, transport.NodeID(e.Address), nil
	case "longnet":
		tr, err := d.longnetTransport(e.Address)
		// The relay is the transport's; the node is the worker's name.
		return tr, transport.NodeID(w.CertName), err
	case "tailscale":
		// The embedded node when this client has one (or the leg was
		// configured explicitly); else the system tailscaled if the address
		// is a tailnet IP; else this client can't use the leg.
		if explicit || d.tsnetConfigured() {
			return d.tsnetTransport(), transport.NodeID(e.Address), nil
		}
		if systemTailscale() && tailnetIP(e.Address) {
			return &tailscaledTCP{}, transport.NodeID(e.Address), nil
		}
		return nil, "", errSkip{why: "no tailscale on this client"}
	}
	return nil, "", fmt.Errorf("unknown transport %q", e.Transport)
}

// tailscaledTCP dials tailnet IPs through the system tailscaled (plain TCP
// over its interface), reported as the tailscale leg.
type tailscaledTCP struct{ transport.TCP }

func (t *tailscaledTCP) Name() string { return "tailscale" }

func (t *tailscaledTCP) Dial(ctx context.Context, node transport.NodeID) (net.Conn, transport.PathInfo, error) {
	c, pi, err := t.TCP.Dial(ctx, node)
	pi.Transport, pi.Detail = "tailscale", "tailscaled "+pi.Detail
	return c, pi, err
}

var cgnat = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()

// tailnetIP reports whether hostport's host is a tailnet (CGNAT-range or
// tailscale ULA) IP.
func tailnetIP(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return cgnat.Contains(ip) || strings.HasPrefix(strings.ToLower(ip.String()), "fd7a:115c:a1e0:")
}

var (
	sysTSOnce sync.Once
	sysTS     bool
)

// systemTailscale reports whether a tailscaled interface is up on this
// machine (an interface named like tailscale, or utun on macOS, holding a
// tailnet address).
func systemTailscale() bool {
	sysTSOnce.Do(func() {
		ifs, err := net.Interfaces()
		if err != nil {
			return
		}
		for _, ifc := range ifs {
			if ifc.Flags&net.FlagUp == 0 {
				continue
			}
			name := strings.ToLower(ifc.Name)
			if !strings.Contains(name, "tailscale") && !strings.HasPrefix(name, "utun") {
				continue
			}
			addrs, _ := ifc.Addrs()
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok && tailnetIP(n.IP.String()) {
					sysTS = true
					return
				}
			}
		}
	})
	return sysTS
}

// preHandshaked are gRPC transport credentials for connections whose TLS
// handshake the race already completed; anything else gets a normal TLS
// handshake.
type preHandshaked struct {
	credentials.TransportCredentials
}

func (p preHandshaked) ClientHandshake(ctx context.Context, authority string, c net.Conn) (net.Conn, credentials.AuthInfo, error) {
	if tc, ok := c.(*raceConn); ok {
		return tc, credentials.TLSInfo{State: tc.Conn.ConnectionState(),
			CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.PrivacyAndIntegrity}}, nil
	}
	return p.TransportCredentials.ClientHandshake(ctx, authority, c)
}

func (p preHandshaked) Clone() credentials.TransportCredentials {
	return preHandshaked{p.TransportCredentials.Clone()}
}

// raceConn is a TLS connection established by the race.
type raceConn struct{ *tls.Conn }

// tsnetUp reports whether this process' embedded tailscale node runs.
func (d *Dialer) tsnetUp() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ts != nil && d.ts.Started()
}

// StickyLeg is the endpoint key of the worker's sticky winner while it is
// still trusted (younger than [dial] reevaluate), else "".
func (d *Dialer) StickyLeg(worker string) string {
	c, ok := d.cache.get(worker)
	if !ok || d.reEvaluate <= 0 || time.Since(c.At) >= d.reEvaluate {
		return ""
	}
	return c.Leg
}

// LiveLeg is one leg of a worker as a long-lived monitor (longarm daemon's pool
// view) measures it right now.
type LiveLeg struct {
	Leg   string  `json:"leg"` // Endpoint.Key()
	Up    bool    `json:"up"`
	Kind  string  `json:"kind,omitempty"` // transport.PathKind, "" = not known yet
	RTTMs float64 `json:"rtt_ms,omitempty"`
}

// A leg is "slower" than another when its RTT is more than 20 % and more
// than 5 ms higher (a few ms between two good legs don't matter, and RTTs
// on a LAN are mostly noise).
const (
	slowerFactor = 1.2
	slowerMin    = 5.0 // ms
)

// liveLegsTimeout bounds the Dialer.LiveLegs lookup before a race.
var liveLegsTimeout = 300 * time.Millisecond

func slower(a, b LiveLeg) bool {
	return a.RTTMs > slowerFactor*b.RTTMs && a.RTTMs-b.RTTMs > slowerMin
}

// BetterLeg reports whether up leg a is clearly better than up leg b: a
// better path kind (direct over peer-relay over relay/derp) unless it is
// slower, or else clearly faster.
func BetterLeg(a, b LiveLeg) bool {
	ra, rb := pathRank(transport.PathKind(a.Kind)), pathRank(transport.PathKind(b.Kind))
	switch {
	case ra >= 0 && rb >= 0 && ra < rb:
		return !slower(a, b)
	case ra >= 0 && rb >= 0 && ra > rb:
		return slower(b, a)
	}
	return slower(b, a)
}

// stickyDrop names why the sticky winner c must not get its head start
// any more given the live view of the legs ("" = keep it): it is down,
// its path got worse than when it won (direct -> relay/derp/peer-relay),
// or another live leg is clearly better (BetterLeg).
func stickyDrop(c CachedLeg, live []LiveLeg, legs []Endpoint) string {
	var mine *LiveLeg
	for i, l := range live {
		if l.Leg == c.Leg {
			mine = &live[i]
		}
	}
	if mine == nil {
		return ""
	}
	if !mine.Up {
		return "sticky leg is down"
	}
	if downgraded(pathKindOf(c.Path), transport.PathKind(mine.Kind)) {
		return fmt.Sprintf("sticky leg's path went from %s to %s", pathKindOf(c.Path), mine.Kind)
	}
	for _, l := range live {
		if l.Leg != c.Leg && l.Up && l.RTTMs > 0 && containsKey(legs, l.Leg) && BetterLeg(l, *mine) {
			return fmt.Sprintf("%s (%s %.1f ms) beats the sticky leg (%s %.1f ms)", l.Leg, l.Kind, l.RTTMs, mine.Kind, mine.RTTMs)
		}
	}
	return ""
}

// bestLive is the best up leg (one of legs) by BetterLeg.
func bestLive(live []LiveLeg, legs []Endpoint) (LiveLeg, bool) {
	var best LiveLeg
	ok := false
	for _, l := range live {
		if l.Up && l.RTTMs > 0 && containsKey(legs, l.Leg) && (!ok || BetterLeg(l, best)) {
			best, ok = l, true
		}
	}
	return best, ok
}

// pathRank orders path kinds best first; -1 = unknown.
func pathRank(k transport.PathKind) int {
	switch k {
	case transport.PathDirect:
		return 0
	case transport.PathPeerRelay:
		return 1
	case transport.PathDERP, transport.PathRelay:
		return 2
	}
	return -1
}

// downgraded reports whether a path moved from old to a worse kind.
func downgraded(old, now transport.PathKind) bool {
	o, n := pathRank(old), pathRank(now)
	return o >= 0 && n >= 0 && n > o
}

// pathKindOf extracts the kind from a PathInfo.String() ("longnet/direct 77ms (...)").
func pathKindOf(s string) transport.PathKind {
	_, rest, ok := strings.Cut(s, "/")
	if !ok {
		return ""
	}
	k, _, _ := strings.Cut(rest, " ")
	return transport.PathKind(k)
}
