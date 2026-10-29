package longarmd

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/server"
	"github.com/lubobill1990/longarm/internal/transport"
	"github.com/lubobill1990/longarm/internal/transport/longnet"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The hub relay legs: a worker whose listeners are all loopback (the
// default of every sign-in install: longarmd login, the tray, the MSI's
// session and admin workers) cannot be reached from another machine. It
// listens on the hub's longnet relays as well - the relay set comes with
// every hub session (HubWelcome.relays; an older hub: HubWelcome.relay, its
// embedded relay only) and changes during one (HubCommand.relays), so
// existing installs pick it up on their next start or hub session without
// signing in again. The set is ordered by preference: external relays
// (longarm-relay --hub, which keep running while the hub restarts) first,
// the hub's embedded relay last; the worker listens on all of them and
// clients race the resulting endpoints. longnet punches a direct UDP path
// when it can (STUN through a relay) and relays otherwise.
//
// The last set is kept in relay-set.json: a worker that starts while the
// hub is down listens on the external relays at once.
//
// Off with [hub] relay = "off", with a longnet or a non-loopback listener
// of its own, or with [hub] advertise (the endpoints are configured).

// RelaySetFile caches the last relay set (in the data dir).
const RelaySetFile = "relay-set.json"

// capRelaySet is this longarmd's NodeHello capability for the relay set
// (hub.CapRelaySet).
const capRelaySet = "relay-set"

// envLegacyRelay (tests only) makes longarmd behave like a version without
// the relay set: no capability in the hello, HubWelcome.relays and
// HubCommand.relays ignored (the embedded relay only).
const envLegacyRelay = "LONGARM_TEST_LEGACY_RELAY"

func legacyRelay() bool { return os.Getenv(envLegacyRelay) == "1" }

// autoRelayLeg tells whether longarmd adds the hub relay legs.
func autoRelayLeg(cfg Config) bool {
	if cfg.Hub.Relay == "off" || len(cfg.Hub.Advertise) > 0 {
		return false
	}
	for _, u := range cfg.Listen {
		if strings.HasPrefix(u, "longnet://") {
			return false
		}
		pu, err := url.Parse(u)
		if err != nil {
			return false
		}
		if h := pu.Hostname(); !strings.HasPrefix(h, "127.") && h != "::1" && h != "localhost" {
			return false // reachable by itself (0.0.0.0, a LAN or tailnet address, tailscale://)
		}
	}
	return true
}

// relayListenURL is the listen URL on the hub's relay: relay is the
// welcome's "host:port" (empty host: the hub's), the TLS fallback the hub's
// own port.
func relayListenURL(hubAddr, relay, node string) string {
	host, port, err := net.SplitHostPort(hubAddr)
	if err != nil || relay == "" {
		return ""
	}
	rh, rp, err := net.SplitHostPort(relay)
	if err != nil || rp == "" || rp == "0" {
		return ""
	}
	q := ""
	if rh == "" || rh == host {
		rh = host
		q = "tcp=" + net.JoinHostPort(host, port) + "&"
	}
	return "longnet://" + net.JoinHostPort(rh, rp) + "?" + q + "name=" + url.QueryEscape(node)
}

// relaySetURL is the listen URL for one relay of the set: the embedded
// relay as relayListenURL; an external one at its own address, with its TLS
// fallback when that is elsewhere.
func relaySetURL(hubAddr string, r *pb.RelayEndpoint, node string) string {
	if r.GetEmbedded() || strings.HasPrefix(r.GetUdp(), ":") {
		return relayListenURL(hubAddr, r.GetUdp(), node)
	}
	h, p, err := net.SplitHostPort(r.GetUdp())
	if err != nil || h == "" || p == "" || p == "0" {
		return ""
	}
	q := ""
	if t := r.GetTcp(); t != "" && t != r.GetUdp() {
		if th, tp, err := net.SplitHostPort(t); err != nil || th == "" || tp == "" {
			return ""
		}
		q = "tcp=" + t + "&"
	}
	return "longnet://" + r.GetUdp() + "?" + q + "name=" + url.QueryEscape(node)
}

// relayLegs runs the listeners on the relay set.
type relayLeg struct {
	ctx     context.Context // longarmd's
	log     *slog.Logger
	node    string // the worker's certificate name
	env     transport.Env
	srv     *server.Server
	serve   func(net.Listener, string)
	track   func(relay string, lt *longnet.Transport) // port links dial with it; nil lt: gone
	changed func()                                    // endpoints changed
	cache   string                                    // RelaySetFile path ("" = none)

	mu   sync.Mutex
	runs map[string]*relayRun // by listen URL
	wg   sync.WaitGroup
}

type relayRun struct {
	stop context.CancelFunc
	done chan struct{}
}

// relaySetCache is RelaySetFile.
type relaySetCache struct {
	Hub    string          `json:"hub"`
	Relays []relayCacheEnt `json:"relays"`
	Saved  time.Time       `json:"saved"`
}

type relayCacheEnt struct {
	UDP      string `json:"udp"`
	TCP      string `json:"tcp,omitempty"`
	Name     string `json:"name,omitempty"`
	Embedded bool   `json:"embedded,omitempty"`
}

// apply (re)starts the legs for the relay a hub session announced the old
// way (HubWelcome.relay: the embedded relay; "" = none).
func (r *relayLeg) apply(hubAddr, relay string) {
	var set []*pb.RelayEndpoint
	if relay != "" {
		set = []*pb.RelayEndpoint{{Udp: relay, Embedded: strings.HasPrefix(relay, ":")}}
	}
	r.applySet(hubAddr, set)
}

// applySet makes the legs match the relay set (empty = none) and caches it.
func (r *relayLeg) applySet(hubAddr string, set []*pb.RelayEndpoint) {
	if r == nil {
		return
	}
	r.start(hubAddr, set)
	r.saveCache(hubAddr, set)
}

func (r *relayLeg) start(hubAddr string, set []*pb.RelayEndpoint) {
	want := map[string]string{} // URL -> TLS fallback hint for clients
	var order []string
	for _, e := range set {
		u := relaySetURL(hubAddr, e, r.node)
		if _, dup := want[u]; u == "" || dup {
			continue
		}
		hint := ""
		if !e.GetEmbedded() && !strings.HasPrefix(e.GetUdp(), ":") && e.GetTcp() != "" && e.GetTcp() != e.GetUdp() {
			hint = e.GetTcp() // clients can't guess an external relay's TLS fallback
		}
		want[u] = hint
		order = append(order, u)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return
	}
	if r.runs == nil {
		r.runs = map[string]*relayRun{}
	}
	for u, run := range r.runs {
		if _, ok := want[u]; !ok {
			run.stop()
			<-run.done
			delete(r.runs, u)
			r.log.Info("hub relay leg removed", "url", u)
		}
	}
	for _, u := range order {
		if r.runs[u] != nil {
			continue
		}
		ctx, cancel := context.WithCancel(r.ctx)
		run := &relayRun{stop: cancel, done: make(chan struct{})}
		r.runs[u] = run
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer close(run.done)
			r.run(ctx, u, want[u])
		}()
	}
}

// loadCache starts the legs of the cached set (longarmd's start: the hub
// may be down; its session replaces the set when it comes).
func (r *relayLeg) loadCache(hubAddr string) {
	if r == nil || r.cache == "" {
		return
	}
	b, err := os.ReadFile(r.cache)
	if err != nil {
		return
	}
	var c relaySetCache
	if json.Unmarshal(b, &c) != nil || c.Hub != hubAddr || len(c.Relays) == 0 {
		return
	}
	var set []*pb.RelayEndpoint
	for _, e := range c.Relays {
		set = append(set, &pb.RelayEndpoint{Udp: e.UDP, Tcp: e.TCP, Name: e.Name, Embedded: e.Embedded})
	}
	r.log.Info("hub relay legs from the cached relay set (until the hub session)", "relays", len(set), "saved", c.Saved)
	r.start(hubAddr, set)
}

func (r *relayLeg) saveCache(hubAddr string, set []*pb.RelayEndpoint) {
	if r.cache == "" {
		return
	}
	c := relaySetCache{Hub: hubAddr, Saved: time.Now().UTC()}
	for _, e := range set {
		c.Relays = append(c.Relays, relayCacheEnt{UDP: e.GetUdp(), TCP: e.GetTcp(), Name: e.GetName(), Embedded: e.GetEmbedded()})
	}
	if old, err := os.ReadFile(r.cache); err == nil {
		var oc relaySetCache
		if json.Unmarshal(old, &oc) == nil && oc.Hub == c.Hub && slices.Equal(oc.Relays, c.Relays) {
			return
		}
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := r.cache + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil || platform.ReplaceFile(tmp, r.cache) != nil {
		os.Remove(tmp)
		r.log.Warn("save the relay set", "path", r.cache, "err", err)
	}
}

func (r *relayLeg) wait() {
	if r != nil {
		r.wg.Wait()
	}
}

func (r *relayLeg) run(ctx context.Context, u, tcpHint string) {
	wait := 5 * time.Second
	for {
		if r.listen(ctx, u, tcpHint) || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, 5*time.Minute)
	}
}

// listen serves on the relay until ctx ends (true) or fails to start.
// tcpHint (an external relay's TLS fallback) goes into the advertised
// endpoint ("longnet://relay:7445?name=w1&tcp=relay:7443").
func (r *relayLeg) listen(ctx context.Context, u, tcpHint string) bool {
	tr, err := transport.ParseListenEnv(u, r.env)
	if err != nil {
		r.log.Warn("hub relay leg", "url", u, "err", err)
		return false
	}
	lt, ok := tr.(*longnet.Transport)
	if !ok {
		return false
	}
	defer lt.Close()
	ln, err := lt.Listen(ctx)
	if err != nil {
		r.log.Warn("hub relay leg not up; retrying", "url", u, "err", err)
		return false
	}
	pu, _ := url.Parse(u)
	r.track(pu.Host, lt)
	defer r.track(pu.Host, nil)
	name := "longnet://" + ln.Addr().String()
	if tcpHint != "" {
		name += "&tcp=" + tcpHint
	}
	r.srv.AddListener(name)
	r.changed()
	r.log.Info("listening on the hub's relay", "url", name)
	go r.serve(ln, name)
	<-ctx.Done()
	ln.Close()
	r.srv.RemoveListener(name)
	if r.ctx.Err() == nil { // another relay or none, not longarmd stopping
		r.changed()
	}
	return true
}
