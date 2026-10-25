package e2e

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// delayProxy forwards TCP connections to target after holding each new
// connection for delay (a slow leg); it can be stopped and restarted on the
// same port (a leg going down).
type delayProxy struct {
	t      *testing.T
	addr   string
	target string
	delay  atomic.Int64
	mu     sync.Mutex
	ln     net.Listener
	conns  []net.Conn
}

func newDelayProxy(t *testing.T, target string, delay time.Duration) *delayProxy {
	p := &delayProxy{t: t, target: target}
	p.delay.Store(int64(delay))
	// The first listen takes a port from the kernel and keeps it (a port
	// checked free and bound later was taken in between on a busy CI host).
	p.addr = "127.0.0.1:0"
	p.start()
	t.Cleanup(p.stop)
	return p
}

func (p *delayProxy) start() {
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		p.t.Fatal(err)
	}
	p.mu.Lock()
	p.ln = ln
	p.addr = ln.Addr().String() // restarts reuse the port
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns = append(p.conns, c)
			p.mu.Unlock()
			go func() {
				defer c.Close()
				time.Sleep(time.Duration(p.delay.Load()))
				u, err := net.Dial("tcp", p.target)
				if err != nil {
					return
				}
				defer u.Close()
				go io.Copy(u, c)
				io.Copy(c, u)
			}()
		}
	}()
}

// stop closes the listener and every proxied connection.
func (p *delayProxy) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

// TestTransportRace: a worker advertises two legs through the hub - tcp
// behind a slow proxy and longnet through the hub's relay. The race picks the
// faster leg (longnet), longarm ls / info show the winner and the race, the choice
// is sticky across longarm processes, and losing either leg (relay = hub down,
// or the tcp leg down) is survived by the other.
func TestTransportRace(t *testing.T) {
	if testing.Short() {
		t.Skip("slow; skipped with -short")
	}
	t.Parallel()
	h := newHubEnv(t, "")
	os.Remove(filepath.Join(h.client, "workers.toml")) // workers come from the hub
	wa := h.newWorkerDir(t, "wa", "")
	proxy := newDelayProxy(t, fmt.Sprintf("127.0.0.1:%d", wa.port), 1500*time.Millisecond)
	os.WriteFile(filepath.Join(wa.dataDir, "longarmd.toml"), []byte(fmt.Sprintf(`listen = ["tcp://127.0.0.1:%d", "longnet://%s?name=wa"]
[hub]
advertise = ["tcp://%s", "longnet://%s?name=wa"]
`, wa.port, h.addr(), proxy.addr, h.addr())), 0o600)
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	n := h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	if eps := fmt.Sprint(n["endpoints"]); !strings.Contains(eps, "longnet://") || !strings.Contains(eps, "tcp://"+proxy.addr) {
		t.Fatalf("advertised endpoints %s", eps)
	}

	// 1. Race: longnet (fast) beats tcp (1.5 s proxy delay), although tcp
	// ranks first in the endpoint list.
	info := h.mustLongarm(h.client, "info", "wa")
	if !strings.Contains(info, "path: longnet/") || !strings.Contains(info, "policy: race") || !strings.Contains(info, "longnet") ||
		!strings.Contains(info, "won") {
		t.Fatalf("longarm info:\n%s", info)
	}
	cache, _ := os.ReadFile(filepath.Join(h.client, "transport-cache.json"))
	if !strings.Contains(string(cache), `"leg": "longnet://`) {
		t.Fatalf("transport-cache.json: %s", cache)
	}
	// longarm ls via the hub shows the last winning leg; --direct races again.
	if ls := h.mustLongarm(h.client, "ls"); !strings.Contains(ls, "last longnet/") {
		t.Fatalf("longarm ls:\n%s", ls)
	}
	if ls := h.mustLongarm(h.client, "ls", "--direct"); !strings.Contains(ls, "longnet/") || !strings.Contains(ls, "won") {
		t.Fatalf("longarm ls --direct:\n%s", ls)
	}
	// The hub's relay answers STUN on a second port (relay_stun2 = auto,
	// UDP port + 1), so both ends learn their NAT type: the longnet path shows
	// "nat x/y".
	hubLog, _ := os.ReadFile(filepath.Join(h.root, "hub.log"))
	m := regexp.MustCompile(`relay_udp=(\S+) stun2=(\S+)`).FindStringSubmatch(string(hubLog))
	if m == nil || !stunAnswers(m[1]) || !stunAnswers(m[2]) {
		t.Fatalf("hub without two STUN ports (%q):\n%s", m, tail(string(hubLog), 2000))
	}
	waitFor(t, "NAT types on the longnet path", 30*time.Second, func() bool {
		return strings.Contains(h.longarm(h.client, "info", "wa").stdout, ", nat ")
	})
	// --transport tcp prefers the slow leg.
	if info := h.mustLongarm(h.client, "--transport", "tcp", "info", "wa"); !strings.Contains(info, "path: tcp/") {
		t.Fatalf("--transport tcp:\n%s", info)
	}

	// 2. longnet leg down (the relay lives in the hub): longarm uses the cached
	// endpoints and the tcp leg.
	h.killHub()
	start := time.Now()
	r := h.longarm(h.client, "exec", "wa", "--", bins.helper, "print", "1", "0", "5")
	if r.code != 5 {
		t.Fatalf("exec with the longnet leg down: %+v", r)
	}
	t.Logf("exec with longnet down took %s", time.Since(start))
	if info := h.mustLongarm(h.client, "info", "wa"); !strings.Contains(info, "path: tcp/") {
		t.Fatalf("longarm info with longnet down:\n%s", info)
	}

	// 3. Hub back, tcp leg down: longnet carries the next connection even
	// though tcp is now the sticky choice.
	h.startHub(t)
	h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	proxy.stop()
	waitFor(t, "longnet leg back", 30*time.Second, func() bool {
		r := h.longarm(h.client, "exec", "wa", "--", bins.helper, "print", "1", "0", "6")
		return r.code == 6
	})
	if info := h.mustLongarm(h.client, "info", "wa"); !strings.Contains(info, "path: longnet/") {
		t.Fatalf("longarm info with tcp down:\n%s", info)
	}
}

// TestStickyLegDegraded reproduces the fleet bug: longnet had won the race
// over a direct path and is the sticky choice; after a restart the worker's
// longnet only works over the relay (LONGARM_NET_DISABLE_DIRECT here). The sticky
// leg must not keep its head start: without longarm daemon, the dial that still
// wins over the worse path makes the next one race every leg; with longarm
// daemon (which measures every leg), tcp - direct and as fast - gets the
// head start right away.
func TestStickyLegDegraded(t *testing.T) {
	if testing.Short() {
		t.Skip("slow; skipped with -short")
	}
	t.Parallel()
	h := newHubEnv(t, "")
	os.Remove(filepath.Join(h.client, "workers.toml")) // workers come from the hub
	wa := h.newWorkerDir(t, "wa", "")
	wa.env = []string{"LONGARM_NET_DISABLE_DIRECT=1"}
	proxy := newDelayProxy(t, fmt.Sprintf("127.0.0.1:%d", wa.port), 0)
	os.WriteFile(filepath.Join(wa.dataDir, "longarmd.toml"), []byte(fmt.Sprintf(`listen = ["tcp://127.0.0.1:%d", "longnet://%s?name=wa"]
[hub]
advertise = ["tcp://%s", "longnet://%s?name=wa"]
`, wa.port, h.addr(), proxy.addr, h.addr())), 0o600)
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })

	cacheFile := filepath.Join(h.client, "transport-cache.json")
	longnetLeg := "longnet://" + h.addr()
	// An earlier longarm found longnet over a direct path (2 s head start).
	stick := func() {
		b := fmt.Sprintf(`{"workers": {"wa": {"leg": %q, "path": "longnet/direct 1ms (simulated)", "took_ms": 1000, "at": %q}}}`,
			longnetLeg, time.Now().UTC().Format(time.RFC3339Nano))
		if err := os.WriteFile(cacheFile, []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	line := func(out, key string) string {
		m := regexp.MustCompile(`(?m)^` + key + `: (.*)$`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no %s in:\n%s", key, out)
		}
		return m[1]
	}

	// 1. No daemon.
	stick()
	out := h.mustLongarm(h.client, "info", "wa")
	cache, _ := os.ReadFile(cacheFile)
	t.Logf("no daemon, sticky longnet: path %s, legs %s", line(out, "path"), line(out, "legs"))
	if strings.HasPrefix(line(out, "path"), "longnet/relay") && !strings.Contains(string(cache), `"at": "0001-01-01T00:00:00Z"`) {
		t.Fatalf("the sticky leg won over the relay but stays trusted: %s", cache)
	}
	out = h.mustLongarm(h.client, "info", "wa")
	if legs := line(out, "legs"); strings.Contains(legs, "not tried") {
		t.Fatalf("the next dial gave a head start again: %s", legs)
	}

	// 2. longarm daemon measures both legs: tcp (direct) beats the relayed
	// sticky longnet leg and gets the head start.
	env := []string{"LONGARM_POOL_INTERVAL=200ms"}
	if r := h.longarmWith(env, h.client, "daemon", "start"); r.code != 0 {
		t.Fatalf("longarm daemon start: %+v", r)
	}
	t.Cleanup(func() { h.longarmWith(nil, h.client, "daemon", "stop") })
	waitFor(t, "the daemon measures both legs", 60*time.Second, func() bool {
		p := h.longarm(h.client, "pool").stdout
		return strings.Contains(p, "longnet relay-") && regexp.MustCompile(`tcp [0-9.]+[mµu]?s`).MatchString(p)
	})
	stick()
	var last string
	waitFor(t, "tcp chosen over the degraded sticky longnet leg", 30*time.Second, func() bool {
		out := h.mustLongarm(h.client, "info", "wa")
		last = line(out, "path") + " | " + line(out, "legs")
		return strings.HasPrefix(line(out, "path"), "tcp/")
	})
	t.Logf("daemon: %s", last)
	cache, _ = os.ReadFile(cacheFile)
	if !strings.Contains(string(cache), `"leg": "tcp://`) {
		t.Fatalf("transport-cache.json: %s", cache)
	}
}
