package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/nodeid"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
)

// Port links through an external relay (longarm-relay --hub, docs/usage.md
// "External relays"), beyond TestPortLinksExternalRelay (a link carries
// traffic, its pair comes and goes on the relay): the relay enforces the
// pairs on its own, by node ID, keeps carrying linked workers while the hub
// restarts, and refuses revoked workers.

var pingsRE = regexp.MustCompile(`pings (\d+)`)

// lastPings is the last progress line of helper hold ("pings N"; -1: none).
func lastPings(out string) int {
	ms := pingsRE.FindAllStringSubmatch(out, -1)
	if len(ms) == 0 {
		return -1
	}
	n, _ := strconv.Atoi(ms[len(ms)-1][1])
	return n
}

// relayRecorder counts the packets a relay delivers to a test session, by
// source.
type relayRecorder struct {
	mu   sync.Mutex
	from map[relay.ID]int
}

func (r *relayRecorder) RelayData(src relay.ID, _ []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.from == nil {
		r.from = map[relay.ID]int{}
	}
	r.from[src]++
}
func (r *relayRecorder) RelayCallMeMaybe(relay.ID, relay.CallMeMaybe) {}
func (r *relayRecorder) RelayPeerGone(relay.ID)                       {}

func (r *relayRecorder) got(src relay.ID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.from[src]
}

// connect opens a session on the relay (TLS/TCP) with bundle b, as a node
// would: a test's own session next to the workers' (another instance, so
// another relay ID).
func (x *extRelay) connect(t *testing.T, b *pki.Bundle, h relay.Handler) (*relay.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := relay.Connect(ctx, relay.ClientConfig{TCPAddr: x.tcp, TLS: relay.ClientTLS(b, ""), Instance: relay.NewInstance(),
		Mode: relay.ModeTCP}, h)
	if err == nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, err
}

// stats is the relay's /sessions (relay.Stats).
func (x *extRelay) stats(t *testing.T) relay.Stats {
	t.Helper()
	var st relay.Stats
	resp, err := http.Get("http://" + x.metrics + "/sessions")
	if err != nil {
		t.Fatalf("relay /sessions: %v", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("relay /sessions: %v", err)
	}
	return st
}

// hubStatus is the hub part of the relay's /healthz.
func (x *extRelay) hubStatus() map[string]any {
	hs, _ := x.health()["hub"].(map[string]any)
	return hs
}

// noEmbeddedRelay restarts h without its embedded longnet relay
// (relay_udp = "off"): the external relays are the only ones.
func noEmbeddedRelay(t *testing.T, h *hubEnv) {
	t.Helper()
	h.killHub()
	b, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := regexp.MustCompile(`(?m)^relay_udp = .*$`).ReplaceAllString(string(b), `relay_udp = "off"`)
	if cfg == string(b) {
		t.Fatal("no relay_udp in the hub's config")
	}
	if err := os.WriteFile(h.cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	h.startHub(t)
}

// listenOnly makes w listen on that longnet relay only (no tcp listener, so
// no direct path either; LONGARM_NET_DISABLE_DIRECT stops hole punching).
func listenOnly(t *testing.T, w *hubWorker, url string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.dataDir, "longarmd.toml"), []byte(fmt.Sprintf("listen = [%q]\n", url)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPortLinksExternalRelayPolicy: two workers that reach each other only
// through an external relay (no tcp listeners, hole punching off, the hub's
// embedded relay off):
//   - a link between them carries traffic over the external relay, which
//     holds the pair by the workers' node IDs: a certificate with the
//     linked name but another node ID, and an unlinked worker, are refused;
//   - the hub restarts (down for longer than the end-to-end idle timeout)
//     while a connection through the link is open: the connection goes on,
//     new connections work while the hub is down and after;
//   - removing the links drops the pair on the relay and cuts the open
//     connection;
//   - a worker revoked by ID is refused by the relay.
func TestPortLinksExternalRelayPolicy(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	alice := h.client
	os.Remove(filepath.Join(alice, "workers.toml")) // workers come from the hub
	noEmbeddedRelay(t, h)
	x := startExtRelay(t, h, "xrelay")
	waitFor(t, "the relay registered with the hub", 30*time.Second, func() bool {
		hs := x.hubStatus()
		return isTrue(hs["connected"]) && isTrue(hs["ready"])
	})
	var rel struct {
		Relays []struct {
			Endpoint struct {
				UDP      string `json:"udp"`
				Embedded bool   `json:"embedded"`
			} `json:"endpoint"`
		} `json:"relays"`
	}
	if err := json.Unmarshal([]byte(h.mustLongarm(h.admin, "admin", "relays", "--json")), &rel); err != nil {
		t.Fatal(err)
	}
	if len(rel.Relays) != 1 || rel.Relays[0].Endpoint.Embedded || rel.Relays[0].Endpoint.UDP != x.udp {
		t.Fatalf("the relay set is not the external relay alone: %+v", rel)
	}

	ws := map[string]*hubWorker{}
	ids := map[string]string{}
	for _, n := range []string{"ra", "rb"} {
		w := h.newWorkerDir(t, n, "")
		listenOnly(t, w, fmt.Sprintf("longnet://%s?tcp=%s&name=%s", x.udp, x.tcp, n))
		// an idle timeout shorter than the hub's downtime below
		w.env = []string{"LONGARM_NET_DISABLE_DIRECT=1", "LONGARM_NET_IDLE_TIMEOUT=4s"}
		h.joinWorker(t, w, true)
		w.start(t, true)
		ws[n] = w
	}
	for _, n := range []string{"ra", "rb"} {
		node := h.waitNode(t, n, "online on the external relay only", func(nd map[string]any) bool {
			eps := fmt.Sprint(nd["endpoints"])
			return isTrue(nd["online"]) && strings.Contains(eps, x.udp) && !strings.Contains(eps, "tcp://")
		})
		ids[n], _ = node["id"].(string)
		if !nodeid.Valid(ids[n]) {
			t.Fatalf("%s: no node id: %v", n, node)
		}
		if c := certOf(t, ws[n].dataDir); c.ID != ids[n] {
			t.Fatalf("%s's certificate carries node id %q, the hub says %s", n, c.ID, ids[n])
		}
	}
	cli := []string{"LONGARM_HUB_TIMEOUT=500ms"}
	la := func(args ...string) result { return h.longarmWith(cli, alice, args...) }
	get := func(lport, path string) result {
		return la("exec", "ra", "--", bins.helper, "http-get", "127.0.0.1:"+lport, path)
	}

	port, srvJob := jobPort(t, h, alice, "rb", bins.helper, "http-serve", "600")
	lport := strconv.Itoa(freePort(t))
	id := linkCreate(t, h, alice, "rb:"+port, "--on", "ra", "--port", lport)

	t.Run("unlinked_refused_pair_by_node_id", func(t *testing.T) {
		waitFor(t, "alice's server through the external relay", 60*time.Second, func() bool {
			return strings.Contains(get(lport, "/x").stdout, "hello /x")
		})
		// the relay's policy: the pair, by node ID
		waitFor(t, "the pair on the relay, by node ID", 30*time.Second, func() bool {
			hs := x.hubStatus()
			if n, _ := hs["worker_pairs"].(float64); n != 1 || !isTrue(hs["worker_pairs_active"]) {
				return false
			}
			var pf struct {
				Pairs   [][2]string `json:"pairs"`
				PairIDs [][2]string `json:"pair_ids"`
			}
			b, _ := os.ReadFile(filepath.Join(x.stateDir, "relay-policy.json"))
			return json.Unmarshal(b, &pf) == nil && len(pf.Pairs) == 1 && len(pf.PairIDs) == 1 &&
				pf.Pairs[0] == [2]string{"ra", "rb"} && strings.EqualFold(pf.PairIDs[0][0], ids["ra"]) && strings.EqualFold(pf.PairIDs[0][1], ids["rb"])
		})

		// Sessions of our own on the relay (another instance of each node):
		// the workers' real certificates, a certificate with ra's name and
		// another node ID (what a reused or renamed name would carry), and a
		// worker no link names.
		ca, err := pki.LoadCA(filepath.Join(h.client, "ca"))
		if err != nil {
			t.Fatal(err)
		}
		issued := func(name string) *pki.Bundle {
			certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{Role: pki.RoleWorker, Name: name, ID: nodeid.New()})
			if err != nil {
				t.Fatal(err)
			}
			b, err := pki.ParseBundle(ca.CertPEM, certPEM, keyPEM)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		session := func(b *pki.Bundle, rh relay.Handler) *relay.Conn {
			c, err := x.connect(t, b, rh)
			if err != nil {
				t.Fatalf("session on the relay: %v", err)
			}
			return c
		}
		recv := &relayRecorder{}
		rb := session(mustBundle(t, ws["rb"]), recv)
		ra := session(mustBundle(t, ws["ra"]), &relayRecorder{})
		fakeRA := session(issued("ra"), &relayRecorder{})
		rc := session(issued("rc"), &relayRecorder{})
		waitFor(t, "ra's certificate reaching rb's through the relay", 20*time.Second, func() bool {
			ra.SendData(rb.Self(), []byte("linked"))
			return recv.got(ra.Self()) > 0
		})
		for _, c := range []struct {
			what string
			c    *relay.Conn
		}{{"ra's name with another node ID", fakeRA}, {"an unlinked worker", rc}} {
			before := x.stats(t).DropPolicy
			waitFor(t, c.what+" refused by the relay's policy", 20*time.Second, func() bool {
				c.c.SendData(rb.Self(), []byte("not linked"))
				return x.stats(t).DropPolicy > before
			})
		}
		// the linked pair still gets through after them (the relay
		// forwards in order per source; the others were dropped, not late)
		n := recv.got(ra.Self())
		waitFor(t, "ra's certificate reaching rb's again", 20*time.Second, func() bool {
			ra.SendData(rb.Self(), []byte("linked"))
			return recv.got(ra.Self()) > n
		})
		if a, b := recv.got(fakeRA.Self()), recv.got(rc.Self()); a != 0 || b != 0 {
			t.Fatalf("the relay delivered %d packets from ra's name with another ID and %d from an unlinked worker", a, b)
		}
		for _, c := range []*relay.Conn{ra, rb, fakeRA, rc} {
			c.Close()
		}
	})

	var holdJob, echoJob, el, ep string
	t.Run("hub_restart_mid_connection", func(t *testing.T) {
		portFile := filepath.Join(h.root, "echo.port")
		echoJob = strings.TrimSpace(h.mustLongarm(alice, "run", "rb", "--", bins.helper, "echo-server", portFile))
		var echoPort string
		waitFor(t, "echo server", 30*time.Second, func() bool { b, _ := os.ReadFile(portFile); echoPort = string(b); return echoPort != "" })
		ep = strconv.Itoa(freePort(t))
		el = linkCreate(t, h, alice, "rb:"+echoPort, "--on", "ra", "--port", ep)
		waitFor(t, "a long connection through the link", 60*time.Second, func() bool {
			if holdJob != "" {
				out := h.longarm(alice, "logs", "ra", holdJob).stdout
				if strings.Contains(out, "up") {
					return true
				}
				if !strings.Contains(out, "cut") {
					return false
				}
			}
			holdJob = strings.TrimSpace(h.mustLongarm(alice, "run", "ra", "--", bins.helper, "hold", "127.0.0.1:"+ep))
			return false
		})
		pings := func(what string, atLeast int) int {
			t.Helper()
			n := -1
			waitFor(t, what, 90*time.Second, func() bool {
				out := la("logs", "ra", holdJob).stdout
				if strings.Contains(out, "cut") {
					t.Fatalf("the connection through the link was cut (%s):\n%s", what, tail(out, 2000))
				}
				n = lastPings(out)
				return n >= atLeast
			})
			return n
		}
		p0 := pings("the held connection's progress", 0)

		h.killHub()
		down := time.Now()
		// 50 more round trips through the link (at least 10 s: longer than
		// the workers' 4 s idle timeout) with the hub down
		p1 := pings("the held connection going on with the hub down", p0+50)
		if hs := x.hubStatus(); isTrue(hs["connected"]) || hs["worker_pairs"] != float64(1) || !isTrue(hs["worker_pairs_active"]) {
			t.Fatalf("the relay with the hub down: %v", hs)
		}
		if r := get(lport, "/hubdown"); !strings.Contains(r.stdout, "hello /hubdown") {
			t.Fatalf("a new connection through the link with the hub down: %+v", r)
		}
		t.Logf("hub down for %s; %d round trips through the link meanwhile", time.Since(down).Round(100*time.Millisecond), p1-p0)
		h.startHub(t)
		waitFor(t, "the relay back on the hub", 30*time.Second, func() bool { return isTrue(x.hubStatus()["connected"]) })
		h.waitNode(t, "ra", "online again", func(n map[string]any) bool { return isTrue(n["online"]) })
		pings("the held connection going on after the hub restart", p1+25)
		for _, l := range []string{id, el} {
			if linkStatus(t, h, alice, l) == "" {
				t.Fatalf("link %s lost by the hub restart", l)
			}
		}
		waitFor(t, "a new connection through the link after the hub restart", 30*time.Second, func() bool {
			return strings.Contains(get(lport, "/again").stdout, "hello /again")
		})
	})

	t.Run("removal_drops_the_pair_and_cuts", func(t *testing.T) {
		if holdJob == "" {
			t.Skip("needs hub_restart_mid_connection's connection")
		}
		recv := &relayRecorder{}
		rb, err := x.connect(t, mustBundle(t, ws["rb"]), recv)
		if err != nil {
			t.Fatal(err)
		}
		ra, err := x.connect(t, mustBundle(t, ws["ra"]), &relayRecorder{})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "ra's certificate reaching rb's while linked", 20*time.Second, func() bool {
			ra.SendData(rb.Self(), []byte("linked"))
			return recv.got(ra.Self()) > 0
		})
		h.mustLongarm(alice, "link", "rm", id, el)
		waitFor(t, "the relay dropping the pair", 30*time.Second, func() bool {
			n, ok := x.hubStatus()["worker_pairs"].(float64)
			return ok && n == 0
		})
		waitFor(t, "the open connection to be cut", 30*time.Second, func() bool {
			return strings.Contains(h.longarm(alice, "logs", "ra", holdJob).stdout, "cut")
		})
		waitFor(t, "new connections refused", 30*time.Second, func() bool {
			return la("exec", "ra", "--", bins.helper, "connect", ep).code != 0
		})
		before, n := x.stats(t).DropPolicy, recv.got(ra.Self())
		waitFor(t, "ra -> rb refused by the relay's policy", 20*time.Second, func() bool {
			ra.SendData(rb.Self(), []byte("unlinked now"))
			return x.stats(t).DropPolicy > before
		})
		if got := recv.got(ra.Self()); got != n {
			t.Fatalf("the relay delivered %d packets ra -> rb after the link went", got-n)
		}
		ra.Close()
		rb.Close()
		h.longarm(alice, "kill", echoJob)
	})

	t.Run("revoked_by_id", func(t *testing.T) {
		before, _ := x.hubStatus()["revoked"].(float64)
		b := mustBundle(t, ws["ra"])
		s, err := x.connect(t, b, &relayRecorder{})
		if err != nil {
			t.Fatalf("ra's certificate before the revocation: %v", err)
		}
		h.mustLongarm(h.admin, "admin", "revoke", ids["ra"][:8])
		waitFor(t, "the revocation on the relay", 30*time.Second, func() bool {
			n, _ := x.hubStatus()["revoked"].(float64)
			return n > before
		})
		select {
		case <-s.Done():
		case <-time.After(30 * time.Second):
			t.Fatal("the relay kept a session of the revoked worker")
		}
		if _, err := x.connect(t, b, &relayRecorder{}); err == nil {
			t.Fatal("the relay accepted a session of the revoked worker")
		}
		waitFor(t, "the revoked worker's own sessions gone from the relay", 30*time.Second, func() bool {
			for _, ss := range x.stats(t).Sessions {
				if ss.Node == "worker/ra" {
					return false
				}
			}
			return true
		})
		// rb is still welcome
		if _, err := x.connect(t, mustBundle(t, ws["rb"]), &relayRecorder{}); err != nil {
			t.Fatalf("rb's certificate after ra's revocation: %v", err)
		}
	})
	h.longarm(alice, "kill", srvJob)
}

// mustBundle is a worker's current TLS bundle (<data dir>/tls).
func mustBundle(t *testing.T, w *hubWorker) *pki.Bundle {
	t.Helper()
	b, err := pki.LoadBundle(filepath.Join(w.dataDir, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
