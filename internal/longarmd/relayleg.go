package longarmd

import (
	"context"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/server"
	"github.com/lubobill1990/longarm/internal/transport"
	"github.com/lubobill1990/longarm/internal/transport/longnet"
)

// The hub relay leg: a worker whose listeners are all loopback (the default
// of every sign-in install: longarmd login, the tray, the MSI's session and
// admin workers) cannot be reached from another machine. It listens on the
// hub's longnet relay as well - the address comes with every hub session
// (HubWelcome.relay), so existing installs pick it up on their next start
// without signing in again. longnet punches a direct UDP path when it can
// (STUN through the relay) and relays otherwise. Off with [hub] relay = "off",
// with a longnet or a non-loopback listener of its own, or with [hub]
// advertise (the endpoints are configured).

// autoRelayLeg tells whether longarmd adds the hub relay leg.
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

// relayLeg runs the listener on the hub's relay.
type relayLeg struct {
	ctx     context.Context // longarmd's
	log     *slog.Logger
	node    string // the worker's certificate name
	env     transport.Env
	srv     *server.Server
	serve   func(net.Listener, string)
	track   func(relay string, lt *longnet.Transport) // port links dial with it; nil lt: gone
	changed func()                                    // endpoints changed

	mu   sync.Mutex
	url  string
	stop context.CancelFunc
	done chan struct{}
	wg   sync.WaitGroup
}

// apply (re)starts the leg for the relay a hub session announced ("" =
// none: stop it).
func (r *relayLeg) apply(hubAddr, relay string) {
	if r == nil {
		return
	}
	u := relayListenURL(hubAddr, relay, r.node)
	r.mu.Lock()
	defer r.mu.Unlock()
	if u == r.url || r.ctx.Err() != nil {
		return
	}
	if r.stop != nil {
		r.stop()
		<-r.done
		r.stop = nil
	}
	r.url = u
	if u == "" {
		return
	}
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	r.stop, r.done = cancel, done
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(done)
		r.run(ctx, u)
	}()
}

func (r *relayLeg) wait() {
	if r != nil {
		r.wg.Wait()
	}
}

func (r *relayLeg) run(ctx context.Context, u string) {
	wait := 5 * time.Second
	for {
		if r.listen(ctx, u) || ctx.Err() != nil {
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
func (r *relayLeg) listen(ctx context.Context, u string) bool {
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
