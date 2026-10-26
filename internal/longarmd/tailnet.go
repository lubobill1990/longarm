package longarmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/server"
	"github.com/lubobill1990/longarm/internal/transport"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The tailscale legs (DESIGN §4.2/§4.4) run in the background: a slow or
// unreachable control server must never keep longarmd from serving its other
// legs. When one comes up, its endpoint is added and the hub gets a fresh
// hello; when it stops, the endpoint goes away again.
//
// The hub-managed leg follows the hub's tailnet (hub.toml [tailnet], and
// the TailnetInfo of every hub session): when the hub switches providers
// the leg restarts on the new control server, with that server's own state
// dir (tsnet-<host>), so switching back needs no key while the old state is
// valid. Keys come from the hub whenever the node has no state for the
// control server or its state no longer logs in (TSNetConfig.KeyFunc). An
// admin re-key (longarm admin tailnet rekey) logs the node out, wipes its state
// and enrols again.

func hasTailscaleListen(urls []string) bool {
	for _, u := range urls {
		if strings.HasPrefix(u, "tailscale://") || strings.HasPrefix(u, "tsnet://") {
			return true
		}
	}
	return false
}

// lateListenRetry is the first retry delay of a failed tailscale listener.
var lateListenRetry = 10 * time.Second

// managedListenURL is the listen URL of the hub-managed leg.
func managedListenURL(t hubclient.TailnetConfig) string {
	return fmt.Sprintf("tailscale://%s:%d?control=%s", t.Hostname, transport.DefaultPort, url.QueryEscape(t.ControlURL))
}

// tailnetLegs runs longarmd's tailscale listeners.
type tailnetLegs struct {
	ctx     context.Context // longarmd's
	log     *slog.Logger
	dataDir string
	hubAddr string // "" = no hub
	live    *pki.Bundle
	env     transport.Env
	srv     *server.Server
	serve   func(net.Listener, string)
	changed func(endpoints bool) // endpoints changed (write longarmd.endpoints) and/or state (re-hello)
	wg      sync.WaitGroup
	applyCh chan *pb.TailnetInfo

	mu      sync.Mutex
	static  []*tsLeg // listen URLs of longarmd.toml
	managed *tsLeg   // the hub's tailnet
}

// tsLeg is one tailscale listener.
type tsLeg struct {
	url    string
	cancel context.CancelFunc
	done   chan struct{}
	// reason overrides the first key request's reason ("rekey").
	reason string
	// managed: the hub's tailnet (keys from the hub); static legs of
	// longarmd.toml bring their own ($TS_AUTHKEY or an interactive login).
	managed bool

	mu     sync.Mutex
	tr     *transport.TSNet
	state  *pb.TailnetState
	logout bool // log the node out when stopping
}

func (l *tsLeg) setState(st *pb.TailnetState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != nil && l.state.State == st.State && st.Since == nil {
		st.Since = l.state.Since
	}
	if st.Since == nil {
		st.Since = timestamppb.Now()
	}
	l.state = st
}

func (l *tsLeg) snapshot() *pb.TailnetState {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == nil {
		return nil
	}
	return proto.Clone(l.state).(*pb.TailnetState)
}

// start launches the static legs and, with a hub, the managed one.
func (m *tailnetLegs) start(listen []string, hubTailnet hubclient.TailnetConfig) {
	m.applyCh = make(chan *pb.TailnetInfo, 1)
	for _, u := range listen {
		if hasTailscaleListen([]string{u}) {
			m.static = append(m.static, m.launch(u, "", false))
		}
	}
	if m.hubAddr == "" {
		return
	}
	if hubTailnet.ControlURL != "" && hubTailnet.Hostname != "" {
		if len(m.static) > 0 {
			m.log.Info("tailscale listener configured in longarmd.toml; not adding the hub's tailnet", "control", hubTailnet.ControlURL)
		} else {
			m.managed = m.launch(managedListenURL(hubTailnet), "", true)
		}
	}
	m.wg.Add(1)
	go func() { defer m.wg.Done(); m.applyLoop() }()
}

// wait blocks until every leg stopped (after longarmd's context ended).
func (m *tailnetLegs) wait() { m.wg.Wait() }

// launch starts a leg's run loop.
func (m *tailnetLegs) launch(u, reason string, managed bool) *tsLeg {
	ctx, cancel := context.WithCancel(m.ctx)
	l := &tsLeg{url: u, cancel: cancel, done: make(chan struct{}), reason: reason, managed: managed}
	// Starting at once: the hello sent right after a switch reports it.
	host, control := legTarget(u)
	l.setState(&pb.TailnetState{ControlUrl: control, Hostname: host, State: "starting"})
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(l.done)
		m.run(ctx, l)
	}()
	return l
}

// stop ends a leg and waits for it (logout: log the node out first).
func (l *tsLeg) stop(logout bool) {
	l.mu.Lock()
	l.logout = logout
	l.mu.Unlock()
	l.cancel()
	<-l.done
}

// legTarget is the hostname and control URL of a tailscale listen URL.
func legTarget(u string) (hostname, control string) {
	if pu, err := url.Parse(u); err == nil {
		return pu.Hostname(), pu.Query().Get("control")
	}
	return "", ""
}

func (m *tailnetLegs) run(ctx context.Context, l *tsLeg) {
	hostname, control := legTarget(l.url)
	wait := lateListenRetry
	for ctx.Err() == nil {
		l.setState(&pb.TailnetState{ControlUrl: control, Hostname: hostname, State: "starting"})
		env := m.env
		if !l.managed {
			env.TailnetKey = nil
		}
		if l.reason != "" && env.TailnetKey != nil {
			reason, inner := l.reason, env.TailnetKey
			env.TailnetKey = func(ctx context.Context, control, r string) (string, error) {
				return inner(ctx, control, reason)
			}
		}
		tr, err := transport.ParseListenEnv(l.url, env)
		if err != nil {
			l.setState(&pb.TailnetState{ControlUrl: control, Hostname: hostname, State: "error", Error: err.Error()})
			m.log.Warn("tailscale leg", "url", l.url, "err", err)
			return
		}
		ts := tr.(*transport.TSNet)
		l.mu.Lock()
		l.tr = ts
		l.mu.Unlock()
		ln, err := ts.Listen(ctx)
		if err == nil {
			l.reason = "" // the next key request is a plain one again
			name := ts.AdvertiseURL(ln)
			sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			dns, ips := ts.Self(sctx)
			cancel()
			l.setState(&pb.TailnetState{ControlUrl: control, Hostname: hostname, State: "running", DnsName: dns, Ips: ips})
			m.srv.AddListener(name)
			m.changed(true)
			m.log.Info("listening", "url", name, "tailnet_name", dns)
			go m.serve(ln, name)
			<-ctx.Done()
			ln.Close()
			l.mu.Lock()
			logout := l.logout
			l.mu.Unlock()
			if logout {
				lctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := ts.Logout(lctx); err != nil {
					m.log.Warn("tailscale logout", "url", l.url, "err", err)
				} else {
					m.log.Info("tailscale node logged out", "control", control, "name", dns)
				}
				cancel()
			}
			ts.Close()
			m.srv.RemoveListener(name)
			l.setState(&pb.TailnetState{ControlUrl: control, Hostname: hostname, State: "off"})
			if m.ctx.Err() == nil { // stopped or switched, not longarmd shutting down
				m.changed(true)
			}
			return
		}
		ts.Close()
		if ctx.Err() != nil {
			return
		}
		state := "error"
		if errors.Is(err, transport.ErrNeedsLogin) || strings.Contains(err.Error(), "no key") {
			state = "needs-key"
		}
		l.setState(&pb.TailnetState{ControlUrl: control, Hostname: hostname, State: state, Error: err.Error()})
		m.changed(false)
		m.log.Warn("tailscale leg not up; retrying", "url", l.url, "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, 5*time.Minute)
	}
}

// state is the tailnet state reported to the hub: the managed leg's, else
// the first static one's.
func (m *tailnetLegs) state() *pb.TailnetState {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.managed != nil {
		return m.managed.snapshot()
	}
	if len(m.static) > 0 {
		return m.static[0].snapshot()
	}
	return nil
}

// apply hands the hub's tailnet info to the apply loop (latest wins).
func (m *tailnetLegs) apply(ti *pb.TailnetInfo) {
	if m == nil || ti == nil || m.applyCh == nil {
		return
	}
	for {
		select {
		case m.applyCh <- ti:
			return
		default:
		}
		select {
		case <-m.applyCh: // drop the stale one
		default:
		}
	}
}

func (m *tailnetLegs) applyLoop() {
	for {
		select {
		case <-m.ctx.Done():
			return
		case ti := <-m.applyCh:
			m.applyNow(ti)
		}
	}
}

// applyNow brings the managed leg in line with the hub's tailnet.
func (m *tailnetLegs) applyNow(ti *pb.TailnetInfo) {
	m.mu.Lock()
	static := len(m.static) > 0
	cur := m.managed
	m.mu.Unlock()
	if static {
		return
	}
	hf, err := hubclient.LoadFile(m.dataDir)
	if err != nil {
		m.log.Warn("tailnet: hub.toml", "err", err)
		return
	}
	if ti.Provider == "" || ti.ControlUrl == "" {
		if cur == nil && hf.Tailnet.ControlURL == "" {
			return
		}
		m.log.Info("the hub has no tailnet any more; stopping the tailscale leg", "control", hf.Tailnet.ControlURL)
		if cur != nil {
			cur.stop(false)
		}
		m.setManaged(nil)
		hf.Tailnet = hubclient.TailnetConfig{}
		if err := hubclient.SaveFile(m.dataDir, hf); err != nil {
			m.log.Warn("tailnet: save hub.toml", "err", err)
		}
		m.changed(false)
		return
	}
	switched := !transport.SameControl(hf.Tailnet.ControlURL, ti.ControlUrl)
	if !switched && !ti.Rekey && cur != nil {
		if hf.Tailnet.Provider != ti.Provider { // record the provider of P2-era files
			hf.Tailnet.Provider = ti.Provider
			hubclient.SaveFile(m.dataDir, hf)
		}
		return
	}
	next := hubclient.TailnetConfig{Provider: ti.Provider, ControlURL: ti.ControlUrl, Hostname: hf.Tailnet.Hostname}
	if next.Hostname == "" {
		next.Hostname = "longarm-" + m.live.Identity.Name
	}
	reason := ""
	if ti.Rekey && os.Getenv("TS_AUTHKEY") != "" {
		m.log.Warn("tailnet re-key requested, but $TS_AUTHKEY provides this node's key: ignored (unset it to let the hub re-key)")
		if !switched {
			return
		}
		ti.Rekey = false
	}
	switch {
	case ti.Rekey && !switched:
		m.log.Info("tailnet re-key requested by the hub: logging out and enrolling again", "control", ti.ControlUrl)
		if cur != nil {
			cur.stop(true)
		}
		dir := hubclient.TailnetStateDir(m.dataDir, ti.ControlUrl)
		if err := transport.WipeState(dir, dir+".authkey"); err != nil {
			m.log.Warn("tailnet re-key", "err", err)
		}
		reason = "rekey"
	case switched:
		m.log.Info("the hub's tailnet changed; switching", "from", hf.Tailnet.ControlURL, "to", ti.ControlUrl, "provider", ti.Provider)
		if cur != nil {
			cur.stop(false) // its state stays for switching back
		}
		reason = "provider switch"
		if ti.Rekey {
			dir := hubclient.TailnetStateDir(m.dataDir, ti.ControlUrl)
			transport.WipeState(dir, dir+".authkey")
			reason = "rekey"
		}
	default: // no managed leg yet (enrolled before the hub had a tailnet)
		m.log.Info("joining the hub's tailnet", "control", ti.ControlUrl, "provider", ti.Provider)
		reason = "enrol"
	}
	hf.Tailnet = next
	if err := hubclient.SaveFile(m.dataDir, hf); err != nil {
		m.log.Warn("tailnet: save hub.toml", "err", err)
	}
	m.setManaged(m.launch(managedListenURL(next), reason, true))
	m.changed(false)
}

func (m *tailnetLegs) setManaged(l *tsLeg) {
	m.mu.Lock()
	m.managed = l
	m.mu.Unlock()
}

// logoutAll logs every running tailscale node out (longarmd leave) and
// returns their state dirs.
func (m *tailnetLegs) logoutAll(ctx context.Context) (dirs []string) {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	legs := append([]*tsLeg(nil), m.static...)
	if m.managed != nil {
		legs = append(legs, m.managed)
	}
	m.mu.Unlock()
	for _, l := range legs {
		l.mu.Lock()
		tr := l.tr
		l.mu.Unlock()
		if tr == nil || !tr.Started() {
			continue
		}
		if err := tr.Logout(ctx); err != nil {
			m.log.Warn("tailscale logout", "url", l.url, "err", err)
		}
		dirs = append(dirs, tr.Config().Dir)
	}
	return dirs
}

// hubKey fetches a key from the hub for the managed leg's control server.
// When the hub's tailnet is another one by now, the key is kept for that
// one and the leg switches.
func (m *tailnetLegs) hubKey(ctx context.Context, control, reason string) (string, error) {
	hf, _ := hubclient.LoadFile(m.dataDir)
	if !transport.SameControl(hf.Tailnet.ControlURL, control) {
		return "", fmt.Errorf("%s is not the hub's tailnet (%s)", control, hf.Tailnet.ControlURL)
	}
	if os.Getenv("TS_AUTHKEY") != "" {
		return "", errors.New("TS_AUTHKEY is set")
	}
	t, err := hubclient.FetchTailnetKey(ctx, m.hubAddr, m.live, reason)
	if err != nil {
		return "", fmt.Errorf("no tailnet key from the hub: %w", err)
	}
	if !transport.SameControl(t.ControlUrl, control) {
		kf := hubclient.TailnetKeyFile(m.dataDir, t.ControlUrl)
		if err := os.MkdirAll(filepath.Dir(kf), 0o700); err == nil {
			os.WriteFile(kf, []byte(t.AuthKey), 0o600)
		}
		m.apply(&pb.TailnetInfo{Provider: t.Provider, ControlUrl: t.ControlUrl})
		return "", fmt.Errorf("the hub's tailnet is now %s", t.ControlUrl)
	}
	m.log.Info("tailnet key from the hub", "provider", t.Provider, "control", t.ControlUrl, "tags", t.Tags, "reason", reason)
	return t.AuthKey, nil
}

// rehello sends a fresh hello (endpoints changed) on the current session.
func (a *hubAgent) rehello() {
	a.send(&pb.NodeReport{Msg: &pb.NodeReport_Hello{Hello: a.hello()}}, false)
}

// migrateTailnetState moves the state of before per-control state dirs
// ("tsnet", used by every tailscale listener without dir=) to the dir its
// listener uses now: the first longarmd.toml tailscale listener's when there is
// one (the hub's tailnet was not added then), else the hub's tailnet's.
func migrateTailnetState(cfg Config, hf hubclient.TailnetConfig) {
	base := filepath.Join(cfg.DataDir, TSNetDir)
	for _, u := range cfg.Listen {
		if !hasTailscaleListen([]string{u}) {
			continue
		}
		if pu, err := url.Parse(u); err == nil && pu.Query().Get("dir") == "" {
			transport.MigrateStateDir(base, pu.Query().Get("control"))
		}
		return
	}
	if hf.ControlURL != "" {
		transport.MigrateStateDir(base, hf.ControlURL)
	}
}

// dialer is a running tailscale node of this worker to dial other nodes
// with (port links), nil if none: the managed leg's, else a static one's.
func (m *tailnetLegs) dialer() transport.Transport {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	legs := append([]*tsLeg{m.managed}, m.static...)
	m.mu.Unlock()
	for _, l := range legs {
		if l == nil {
			continue
		}
		l.mu.Lock()
		tr := l.tr
		l.mu.Unlock()
		if tr != nil && tr.Started() {
			return tr
		}
	}
	return nil
}
