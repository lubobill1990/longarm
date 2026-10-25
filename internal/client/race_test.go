package client

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/transport"
)

// raceEnv: a CA, a client dialer and helpers for TLS "workers" whose
// handshake can be delayed.
type raceEnv struct {
	t      *testing.T
	ca     *pki.CA
	worker *pki.Bundle
	d      *Dialer
}

func newRaceEnv(t *testing.T) *raceEnv {
	root := t.TempDir()
	ca, err := pki.InitCA(filepath.Join(root, "ca"), "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bundle := func(role pki.Role, name, dir string) *pki.Bundle {
		cert, key, err := ca.Issue(pki.IssueOptions{Role: role, Name: name, Validity: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if err := pki.WriteBundle(dir, ca.CertPEM, cert, key); err != nil {
			t.Fatal(err)
		}
		b, err := pki.LoadBundle(dir)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	w := bundle(pki.RoleWorker, "w1", filepath.Join(root, "worker"))
	cdir := filepath.Join(root, "client")
	bundle(pki.RoleClient, "alice", filepath.Join(cdir, TLSDir))
	cfg, err := LoadConfig(cdir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDialer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return &raceEnv{t: t, ca: ca, worker: w, d: d}
}

// serve starts a TLS listener that waits delay before each handshake.
func (e *raceEnv) serve(delay time.Duration) net.Listener {
	cfg, err := e.worker.ServerConfig()
	if err != nil {
		e.t.Fatal(err)
	}
	cfg.NextProtos = []string{"h2"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				time.Sleep(delay)
				tc := tls.Server(c, cfg)
				if tc.Handshake() != nil {
					return
				}
				buf := make([]byte, 1)
				tc.Read(buf) // hold until the client closes
			}()
		}
	}()
	return ln
}

func (e *raceEnv) race(w Worker) (Endpoint, []LegResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tc, _, _, legs, err := e.d.race(ctx, w, false)
	if err != nil {
		return Endpoint{}, legs, err
	}
	tc.Close()
	for _, l := range legs {
		if l.Won {
			return l.Endpoint, legs, nil
		}
	}
	e.t.Fatal("no winner reported")
	return Endpoint{}, legs, nil
}

func TestRacePicksFasterLegAndSurvivesLoss(t *testing.T) {
	e := newRaceEnv(t)
	slow := e.serve(400 * time.Millisecond)
	fast := e.serve(0)
	w := Worker{Name: "w1", CertName: "w1", Transport: "tcp", Address: slow.Addr().String(),
		Alt: []Endpoint{{Transport: "tcp", Address: fast.Addr().String()}}}

	won, legs, err := e.race(w)
	if err != nil || won.Address != fast.Addr().String() {
		t.Fatalf("race: won %v, %v (%s)", won, err, RaceSummary(legs))
	}
	if !strings.Contains(RaceSummary(legs), "won") {
		t.Fatalf("summary: %s", RaceSummary(legs))
	}
	c, ok := e.d.Sticky("w1")
	if !ok || c.Leg != "tcp://"+fast.Addr().String() {
		t.Fatalf("sticky = %+v %v", c, ok)
	}
	// Persisted for the next longarm process.
	if b, err := os.ReadFile(filepath.Join(e.d.cfg.Dir, CacheFile)); err != nil || !strings.Contains(string(b), fast.Addr().String()) {
		t.Fatalf("cache file: %s %v", b, err)
	}

	// The sticky (fast) leg goes down: the slow one takes over at once
	// (the sticky leg's failure starts the others without waiting).
	fast.Close()
	start := time.Now()
	won, legs, err = e.race(w)
	if err != nil || won.Address != slow.Addr().String() {
		t.Fatalf("after loss: won %v, %v (%s)", won, err, RaceSummary(legs))
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("fallback took %s", took)
	}

	// Every leg down: an error naming all legs.
	slow.Close()
	if _, legs, err := e.race(w); err == nil || !strings.Contains(err.Error(), "all 2 transport legs failed") {
		t.Fatalf("all down: %v (%s)", err, RaceSummary(legs))
	}
}

func TestRaceStickyHeadStartAndReEvaluation(t *testing.T) {
	e := newRaceEnv(t)
	slow := e.serve(300 * time.Millisecond)
	fast := e.serve(0)
	w := Worker{Name: "w1", CertName: "w1", Transport: "tcp", Address: fast.Addr().String(),
		Alt: []Endpoint{{Transport: "tcp", Address: slow.Addr().String()}}}
	// An earlier process found the slow leg (handshake ~1 s): it gets a 2 s
	// head start and keeps winning while it works.
	e.d.cache.put("w1", CachedLeg{Leg: "tcp://" + slow.Addr().String(), TookMs: 1000, At: time.Now()})
	won, legs, err := e.race(w)
	if err != nil || won.Address != slow.Addr().String() {
		t.Fatalf("sticky: won %v, %v (%s)", won, err, RaceSummary(legs))
	}
	for _, l := range legs {
		if l.Endpoint.Address == fast.Addr().String() && l.Err != "not tried" {
			t.Fatalf("fast leg was dialed during the head start: %+v", l)
		}
	}
	// Once the choice is older than reevaluate, all legs race again.
	e.d.reEvaluate = time.Nanosecond
	time.Sleep(time.Millisecond)
	won, legs, err = e.race(w)
	if err != nil || won.Address != fast.Addr().String() {
		t.Fatalf("re-evaluated: won %v, %v (%s)", won, err, RaceSummary(legs))
	}
}

func TestRacePreferPolicy(t *testing.T) {
	e := newRaceEnv(t)
	fast := e.serve(0)
	// longnet is preferred but this leg can't work (nothing listens on the
	// relay address); tcp starts once it failed, well before preferGrace.
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close()
	w := Worker{Name: "w1", CertName: "w1", Transport: "tcp", Address: fast.Addr().String(),
		Alt: []Endpoint{{Transport: "tailscale", Address: "192.0.2.1:7701"}}}
	if err := e.d.SetPolicy("tailscale"); err != nil {
		t.Fatal(err)
	}
	won, legs, err := e.race(w)
	if err != nil || won.Transport != "tcp" {
		t.Fatalf("prefer tailscale (unusable): won %v, %v (%s)", won, err, RaceSummary(legs))
	}
	// A preferred tcp leg that works wins even when listed second.
	w2 := Worker{Name: "w2", CertName: "w1", Transport: "tcp", Address: deadAddr,
		Alt: []Endpoint{{Transport: "tcp", Address: fast.Addr().String()}}}
	e.d.SetPolicy("tcp")
	if won, legs, err := e.race(w2); err != nil || won.Address != fast.Addr().String() {
		t.Fatalf("prefer tcp: won %v, %v (%s)", won, err, RaceSummary(legs))
	}
	if err := e.d.SetPolicy("carrier-pigeon"); err == nil {
		t.Fatal("bad policy accepted")
	}
}

func TestRaceWrongWorkerLoses(t *testing.T) {
	// A leg that reaches some other TLS server (wrong certificate name) must
	// not win, however fast it is.
	e := newRaceEnv(t)
	right := e.serve(200 * time.Millisecond)
	cert, key, _ := e.ca.Issue(pki.IssueOptions{Role: pki.RoleWorker, Name: "intruder", Validity: time.Hour})
	dir := filepath.Join(t.TempDir(), "intruder")
	pki.WriteBundle(dir, e.ca.CertPEM, cert, key)
	ib, _ := pki.LoadBundle(dir)
	saved := e.worker
	e.worker = ib
	wrong := e.serve(0)
	e.worker = saved
	w := Worker{Name: "w1", CertName: "w1", Transport: "tcp", Address: wrong.Addr().String(),
		Alt: []Endpoint{{Transport: "tcp", Address: right.Addr().String()}}}
	won, legs, err := e.race(w)
	if err != nil || won.Address != right.Addr().String() {
		t.Fatalf("won %v, %v (%s)", won, err, RaceSummary(legs))
	}
}

func TestParseEndpointAndLegs(t *testing.T) {
	for in, want := range map[string]Endpoint{
		"tcp://1.2.3.4:7701":           {"tcp", "1.2.3.4:7701"},
		"tailscale://100.64.0.5:7701":  {"tailscale", "100.64.0.5:7701"},
		"tsnet://longarm-w1":           {"tailscale", "longarm-w1"},
		"longnet://relay:7443?name=w1": {"longnet", "relay:7443"},
	} {
		got, err := ParseEndpoint(in)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v", in, got, err)
		}
	}
	if _, err := ParseEndpoint("ftp://x:1"); err == nil {
		t.Error("ftp accepted")
	}
	w := Worker{Transport: "tcp", Address: "a:1", Alt: []Endpoint{{"tcp", "a:1"}, {"longnet", "r:2"}}}
	if l := w.Legs(); len(l) != 2 || l[1].Transport != "longnet" {
		t.Fatalf("legs %+v", l)
	}
	if !tailnetIP("100.101.102.103:7701") || tailnetIP("10.0.0.1:7701") || !tailnetIP("[fd7a:115c:a1e0::1]:7701") {
		t.Fatal("tailnetIP")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, WorkersFile), []byte(`
[dial]
reevaluate = "1m"

[[worker]]
name = "w1"
address = "10.0.0.1:7701"
endpoints = ["longnet://relay.example.com:7443", "tailscale://100.64.0.9:7701"]
`), 0o644)
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Dial.ReEvaluate != "1m" || len(c.Workers[0].Legs()) != 3 {
		t.Fatalf("config %+v", c)
	}
	// The policy moved to settings.json (longarm transport): a leftover is an error.
	os.WriteFile(filepath.Join(dir, WorkersFile), []byte("[dial]\npolicy = \"longnet\"\n"), 0o644)
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "longarm transport longnet") {
		t.Fatalf("[dial] policy: %v", err)
	}
}

func TestRaceGraceBoundedByDeadline(t *testing.T) {
	e := newRaceEnv(t)
	fast := e.serve(0)
	// The sticky leg hangs (accepts TCP, never answers the TLS handshake)
	// and its head start would be long; with a 3 s deadline the other leg
	// must start by 1.5 s.
	hang, _ := net.Listen("tcp", "127.0.0.1:0")
	defer hang.Close()
	saved := maxHeadStart
	maxHeadStart = time.Minute
	defer func() { maxHeadStart = saved }()
	w := Worker{Name: "w1", CertName: "w1", Transport: "tcp", Address: hang.Addr().String(),
		Alt: []Endpoint{{Transport: "tcp", Address: fast.Addr().String()}}}
	e.d.cache.put("w1", CachedLeg{Leg: "tcp://" + hang.Addr().String(), TookMs: 60_000, At: time.Now()})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	tc, _, _, legs, err := e.d.race(ctx, w, false)
	if err != nil {
		t.Fatalf("race: %v (%s)", err, RaceSummary(legs))
	}
	tc.Close()
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Fatalf("fallback only after %s", took)
	}
}

// TestRaceStickyDroppedWhenDegraded: longarm daemon's live view of the legs
// takes the head start away from a sticky winner whose path got worse or
// that another leg clearly beats, and gives it to the best measured leg.
func TestRaceStickyDroppedWhenDegraded(t *testing.T) {
	e := newRaceEnv(t)
	sticky := e.serve(100 * time.Millisecond)
	other := e.serve(0)
	sk, ok := "tcp://"+sticky.Addr().String(), "tcp://"+other.Addr().String()
	w := Worker{Name: "w1", CertName: "w1", Transport: "tcp", Address: other.Addr().String(),
		Alt: []Endpoint{{Transport: "tcp", Address: sticky.Addr().String()}}}
	var live []LiveLeg
	e.d.LiveLegs = func(context.Context, string) []LiveLeg { return live }
	// race with the sticky leg cached as won over a direct path (2 s head start).
	race := func(what string) (Endpoint, []LegResult) {
		t.Helper()
		e.d.cache.put("w1", CachedLeg{Leg: sk, Path: "longnet/direct 77ms (1.2.3.4:5000)", TookMs: 1000, At: time.Now()})
		won, legs, err := e.race(w)
		if err != nil {
			t.Fatalf("%s: %v (%s)", what, err, RaceSummary(legs))
		}
		return won, legs
	}
	notTried := func(legs []LegResult, key string) bool {
		for _, l := range legs {
			if l.Endpoint.Key() == key {
				return l.Err == "not tried"
			}
		}
		return false
	}
	cases := []struct {
		name   string
		live   []LiveLeg
		winner string
	}{
		{"no live view: sticky keeps its head start", nil, sk},
		{"sticky still direct and about as fast", []LiveLeg{{Leg: sk, Up: true, Kind: "direct", RTTMs: 80}, {Leg: ok, Up: true, Kind: "direct", RTTMs: 77}}, sk},
		{"sticky fell back to the relay (the fleet bug)", []LiveLeg{{Leg: sk, Up: true, Kind: "relay", RTTMs: 137}, {Leg: ok, Up: true, Kind: "direct", RTTMs: 77}}, ok},
		{"sticky relayed but just as fast: still a downgrade", []LiveLeg{{Leg: sk, Up: true, Kind: "relay", RTTMs: 70}, {Leg: ok, Up: true, Kind: "direct", RTTMs: 77}}, ok},
		{"sticky >20% slower than another live leg", []LiveLeg{{Leg: sk, Up: true, Kind: "direct", RTTMs: 137}, {Leg: ok, Up: true, Kind: "direct", RTTMs: 77}}, ok},
		{"sticky down", []LiveLeg{{Leg: sk}, {Leg: ok, Up: true, Kind: "direct", RTTMs: 77}}, ok},
		{"a faster leg on a worse path doesn't beat a direct sticky", []LiveLeg{{Leg: sk, Up: true, Kind: "direct", RTTMs: 30}, {Leg: ok, Up: true, Kind: "derp", RTTMs: 28}}, sk},
	}
	for _, c := range cases {
		live = c.live
		won, legs := race(c.name)
		if won.Key() != c.winner {
			t.Fatalf("%s: won %s (%s)", c.name, won.Key(), RaceSummary(legs))
		}
		// The best measured leg got the head start: the dropped sticky leg
		// (100 ms handshake) was never dialed.
		if c.winner == ok && c.live != nil && !notTried(legs, sk) {
			t.Fatalf("%s: the dropped sticky leg was dialed: %s", c.name, RaceSummary(legs))
		}
	}

	// Without a cached winner the best measured leg gets the head start:
	// the sticky (100 ms handshake) server wins although the other one is
	// faster to handshake, because the daemon measures it better.
	e.d.cache.mu.Lock()
	delete(e.d.cache.Workers, "w1")
	e.d.cache.mu.Unlock()
	os.Remove(e.d.cache.path)
	live = []LiveLeg{{Leg: sk, Up: true, Kind: "direct", RTTMs: 10}, {Leg: ok, Up: true, Kind: "relay", RTTMs: 10}}
	won, legs, err := e.race(w)
	if err != nil || won.Key() != sk || !notTried(legs, ok) {
		t.Fatalf("best live leg: won %v, %v (%s)", won, err, RaceSummary(legs))
	}
}

// TestStickyDowngradeNotTrusted: a sticky leg that wins with its head
// start over a worse path than it was chosen on is no longer trusted.
func TestStickyDowngradeNotTrusted(t *testing.T) {
	c := CachedLeg{Leg: "longnet://relay:7443", Path: "longnet/direct 77ms (1.2.3.4:5000, nat easy/easy)", At: time.Now().Add(-time.Minute)}
	if pathKindOf(c.Path) != "direct" || pathKindOf("tcp/relay 1ms") != "relay" || pathKindOf("garbage") != "" {
		t.Fatal("pathKindOf")
	}
	for _, tc := range []struct {
		now  string
		want bool
	}{{"relay", true}, {"derp", true}, {"peer-relay", true}, {"direct", false}, {"unknown", false}, {"", false}} {
		if got := !stickyAt(&c, c.Leg, transport.PathInfo{Kind: transport.PathKind(tc.now)}).Equal(c.At); got != tc.want {
			t.Errorf("direct -> %q: distrusted %v", tc.now, got)
		}
	}
	if at := stickyAt(&c, "tcp://other:1", transport.PathInfo{Kind: transport.PathDirect}); at.Equal(c.At) || at.IsZero() {
		t.Error("a new winner gets a fresh time")
	}
	if !stickyAt(&c, c.Leg, transport.PathInfo{Kind: transport.PathRelay}).IsZero() {
		t.Error("a distrusted sticky must expire at once")
	}
}

// TestRaceSkippedLegLastResort: a leg SkipLeg rules out (the hub looked
// down, so its relay probably is) is still dialed when every other leg
// failed - the hub may only have been slow.
func TestRaceSkippedLegLastResort(t *testing.T) {
	e := newRaceEnv(t)
	ok := e.serve(0)
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close()
	e.d.SkipLeg = func(ep Endpoint) string {
		if ep.Address == ok.Addr().String() {
			return "the hub's relay is down"
		}
		return ""
	}
	only := Worker{Name: "w1", CertName: "w1", Transport: "tcp", Address: ok.Addr().String()}
	if won, legs, err := e.race(only); err != nil || won.Address != ok.Addr().String() {
		t.Fatalf("only leg skipped: won %v, %v (%s)", won, err, RaceSummary(legs))
	}
	two := Worker{Name: "w2", CertName: "w1", Transport: "tcp", Address: deadAddr,
		Alt: []Endpoint{{Transport: "tcp", Address: ok.Addr().String()}}}
	if won, legs, err := e.race(two); err != nil || won.Address != ok.Addr().String() {
		t.Fatalf("other leg failed: won %v, %v (%s)", won, err, RaceSummary(legs))
	}
	// A skip that isn't a guess (no tailscale here) stays a skip.
	ts := Worker{Name: "w3", CertName: "w1", Transport: "tailscale", Address: "192.0.2.1:7701"}
	if _, legs, err := e.race(ts); err == nil || !strings.Contains(err.Error(), "skipped") {
		t.Fatalf("tailscale without a node: %v (%s)", err, RaceSummary(legs))
	}
}
