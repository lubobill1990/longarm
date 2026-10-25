package transport

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// TSNetConfig configures the embedded Tailscale node (DESIGN §4.2).
type TSNetConfig struct {
	Hostname string // tailnet machine name, e.g. "longarm-<host>" / "longarm-client-<host>"
	Dir      string // state directory (node key etc.)
	AuthKey  string // pre-authorized (tagged) auth key; only needed on first start
	// AuthKeyFile holds an auth key (minted by the hub's Headscale) when
	// AuthKey is empty; it is removed once the node is up (keys are one-time).
	AuthKeyFile string
	ControlURL  string // empty = Tailscale SaaS; or a Headscale URL (DESIGN §4.4)
	Port        int    // longarmd port inside the tailnet (Listen), default port for Dial
	Ephemeral   bool
	// Logf receives tsnet's verbose backend logs; nil discards them.
	Logf func(format string, args ...any)
	// KeyFunc fetches a fresh one-time key (from the hub) when the node has
	// neither state nor a key, or its state no longer logs in (an ephemeral
	// node the control server removed, a node an admin deleted, a re-key):
	// the state dir is then wiped and the node enrols again. reason is
	// logged by the hub. nil = no automatic keys.
	KeyFunc func(ctx context.Context, reason string) (string, error)
}

// ErrNeedsLogin: the control server wants an interactive login (no usable
// key, and the node's state - if any - is no longer valid).
var ErrNeedsLogin = errors.New("tailscale node needs a login (no valid key or state)")

// needsLoginAfter is how long a node started from existing state may sit
// in NeedsLogin before it counts as logged out (a fresh node with a key
// passes through NeedsLogin while it registers; that never counts).
var needsLoginAfter = 8 * time.Second

// JoinedMarker is written into the state dir once the node is up (logged
// in); tsnet creates its state file before that, so the file alone says
// nothing.
const JoinedMarker = "longarm.joined"

// Joined reports whether the embedded node in dir has been up before.
func Joined(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, JoinedMarker))
	return err == nil
}

// TSNet is the tailscale backend: one tsnet.Server per process.
type TSNet struct {
	cfg TSNetConfig

	// The node starts in the background (start): a dial that gives up (the
	// race picked another leg) doesn't abort it, so the next dial finds it
	// up. Close cancels a start in progress.
	bgCtx    context.Context
	bgCancel context.CancelFunc

	mu       sync.Mutex // srv, closed, starting, startErr: never held across network calls
	srv      *tsnet.Server
	closed   bool
	starting chan struct{} // closed when the start in progress finished
	startErr error         // outcome of the last finished start
}

// NewTSNet returns an unstarted tailscale transport; the node starts on the
// first Listen/Dial (or Warm).
func NewTSNet(cfg TSNetConfig) *TSNet {
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &TSNet{cfg: cfg, bgCtx: ctx, bgCancel: cancel}
}

func (t *TSNet) Name() string { return "tailscale" }

// Config returns the effective configuration.
func (t *TSNet) Config() TSNetConfig { return t.cfg }

// Warm starts the node in the background if it isn't up or starting (a
// long-lived process, so that its first dial finds it up).
func (t *TSNet) Warm() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.kickLocked()
}

// kickLocked starts the node unless it runs, is starting or was closed;
// it returns the channel that is closed when the start finished (nil when
// nothing is to be waited for).
func (t *TSNet) kickLocked() chan struct{} {
	if t.srv != nil || t.closed {
		return nil
	}
	if t.starting == nil {
		ch := make(chan struct{})
		t.starting = ch
		go t.start(ch)
	}
	return t.starting
}

// server returns the running node, starting it if needed; ctx only bounds
// the wait (the start goes on without it).
func (t *TSNet) server(ctx context.Context) (*tsnet.Server, error) {
	t.mu.Lock()
	s, closed := t.srv, t.closed
	ch := t.kickLocked()
	t.mu.Unlock()
	if s != nil {
		return s, nil
	}
	if closed {
		return nil, net.ErrClosed
	}
	select {
	case <-ch:
	case <-ctx.Done():
		return nil, fmt.Errorf("tailscale (%s): still starting: %w", t.cfg.Hostname, context.Cause(ctx))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.srv != nil {
		return t.srv, nil
	}
	if t.closed {
		return nil, net.ErrClosed
	}
	return nil, t.startErr
}

// start brings the node up (one at a time: ch) and records the outcome.
func (t *TSNet) start(ch chan struct{}) {
	s, err := t.bringUp(t.bgCtx)
	t.mu.Lock()
	closed := t.closed
	if err == nil && !closed {
		t.srv = s
	}
	t.mu.Unlock()
	if err == nil && closed { // closed while it came up: down before Close returns
		s.Close()
		err = net.ErrClosed
	}
	t.mu.Lock()
	t.startErr = err
	t.starting = nil
	t.mu.Unlock()
	close(ch)
}

func (t *TSNet) bringUp(ctx context.Context) (*tsnet.Server, error) {
	if t.cfg.Hostname == "" {
		return nil, errors.New("tailscale transport: hostname required")
	}
	if t.cfg.Dir == "" {
		return nil, errors.New("tailscale transport: state dir required")
	}
	authKey := t.cfg.AuthKey
	if authKey == "" && t.cfg.AuthKeyFile != "" {
		if b, err := os.ReadFile(t.cfg.AuthKeyFile); err == nil {
			authKey = strings.TrimSpace(string(b))
			if t.cfg.Logf != nil && authKey != "" {
				t.cfg.Logf("tailscale (%s): using the waiting key in %s", t.cfg.Hostname, t.cfg.AuthKeyFile)
			}
		}
	}
	fresh := false // authKey came from KeyFunc in this call
	joined := Joined(t.cfg.Dir)
	if authKey == "" && !joined && t.cfg.KeyFunc != nil {
		k, err := t.cfg.KeyFunc(ctx, "no state")
		if err != nil {
			return nil, fmt.Errorf("tailscale (%s): no key: %w", t.cfg.Hostname, err)
		}
		authKey, fresh = k, true
		t.saveKey(k)
	}
	s, err := t.up(ctx, authKey)
	if err != nil && !fresh && t.cfg.KeyFunc != nil && (errors.Is(err, ErrNeedsLogin) || (authKey != "" && !joined && errors.Is(err, errBackend))) {
		// The state is no longer valid or the stored key was rejected:
		// start over with a fresh key (fetched first: no key, no wipe).
		reason := "login needed"
		if !errors.Is(err, ErrNeedsLogin) {
			reason = "key rejected"
		}
		k, kerr := t.cfg.KeyFunc(ctx, reason)
		if kerr != nil {
			return nil, fmt.Errorf("%w; no new key: %w", err, kerr)
		}
		if werr := WipeState(t.cfg.Dir, t.cfg.AuthKeyFile); werr != nil {
			return nil, werr
		}
		t.saveKey(k)
		s, err = t.up(ctx, k)
	}
	if err != nil {
		return nil, err
	}
	if t.cfg.AuthKeyFile != "" {
		retryRemove(func() error { return os.Remove(t.cfg.AuthKeyFile) }) // used (or not needed: the node has state)
	}
	// The marker also records the control server (logout after the hub
	// settings are gone).
	os.WriteFile(filepath.Join(t.cfg.Dir, JoinedMarker), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"+t.cfg.ControlURL+"\n"), 0o600)
	return s, nil
}

// saveKey keeps a fetched key in AuthKeyFile until the node used it, so a
// restart before the control server answered doesn't waste it.
func (t *TSNet) saveKey(k string) {
	if t.cfg.AuthKeyFile == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(t.cfg.AuthKeyFile), 0o700); err == nil {
		os.WriteFile(t.cfg.AuthKeyFile, []byte(k), 0o600)
	}
}

// errBackend marks an error the tailscale backend reported (e.g. a
// rejected key), as opposed to a timeout.
var errBackend = errors.New("tailscale backend error")

// up starts a tsnet node and waits until it runs. A node from existing
// state that the control server sends back to login fails with
// ErrNeedsLogin instead of waiting for an interactive login.
func (t *TSNet) up(ctx context.Context, authKey string) (*tsnet.Server, error) {
	logf := t.cfg.Logf
	if logf == nil && os.Getenv("LONGARM_TSNET_LOG") == "1" { // debugging the tailscale leg
		logf = log.Printf
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &tsnet.Server{
		Hostname:   t.cfg.Hostname,
		Dir:        t.cfg.Dir,
		AuthKey:    authKey,
		ControlURL: t.cfg.ControlURL,
		Ephemeral:  t.cfg.Ephemeral,
		Logf:       logf,
		UserLogf:   log.Printf, // login URLs etc. are worth showing
	}
	upCtx, cancel := context.WithTimeoutCause(ctx, 2*time.Minute, errors.New("timed out"))
	defer cancel()
	lc, err := s.LocalClient() // starts the node
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("tailscale up (%s): %w", t.cfg.Hostname, err)
	}
	wctx, stop := context.WithCancelCause(upCtx)
	defer stop(nil)
	// Without KeyFunc an interactive login (URL in the log) stays possible.
	if t.cfg.KeyFunc != nil {
		go watchLogin(wctx, lc, authKey == "" || Joined(t.cfg.Dir), stop)
	}
	if _, err := s.Up(wctx); err != nil {
		s.Close()
		if c := context.Cause(wctx); errors.Is(c, ErrNeedsLogin) {
			return nil, fmt.Errorf("tailscale up (%s): %w", t.cfg.Hostname, ErrNeedsLogin)
		}
		if strings.Contains(err.Error(), "backend:") {
			return nil, fmt.Errorf("tailscale up (%s): %w: %w", t.cfg.Hostname, errBackend, err)
		}
		return nil, fmt.Errorf("tailscale up (%s): %w", t.cfg.Hostname, err)
	}
	return s, nil
}

// watchLogin cancels ctx with ErrNeedsLogin when the node asks for an
// interactive login (an auth URL), or - for a node started from state
// (where tsnet ignores a key) - stays in NeedsLogin for needsLoginAfter.
func watchLogin(ctx context.Context, lc interface {
	StatusWithoutPeers(context.Context) (*ipnstate.Status, error)
}, fromState bool, stop context.CancelCauseFunc) {
	var since time.Time
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		st, err := lc.StatusWithoutPeers(sctx)
		cancel()
		if err != nil {
			continue
		}
		if st.AuthURL != "" {
			stop(ErrNeedsLogin)
			return
		}
		if st.BackendState != "NeedsLogin" || !fromState {
			since = time.Time{}
			continue
		}
		if since.IsZero() {
			since = time.Now()
		} else if time.Since(since) >= needsLoginAfter {
			stop(ErrNeedsLogin)
			return
		}
	}
}

// WipeState removes a node's tailscale state (and its waiting key): it
// enrols as a new node next time.
func WipeState(dir, keyFile string) error {
	if keyFile != "" {
		// A key left behind would be used again instead of a fresh one
		// (a re-key that silently re-uses the old key).
		if err := retryRemove(func() error { return os.Remove(keyFile) }); err != nil {
			return fmt.Errorf("wipe tailscale key %s: %w", keyFile, err)
		}
	}
	if dir == "" {
		return nil
	}
	if err := retryRemove(func() error { return os.RemoveAll(dir) }); err != nil {
		return fmt.Errorf("wipe tailscale state %s: %w", dir, err)
	}
	return os.MkdirAll(dir, 0o700)
}

// retryRemove retries a removal for a few seconds: on Windows a file that
// another process has open (a reader, an anti-virus scan, a node still
// closing its state) can't be deleted for a moment. Missing = done.
func retryRemove(rm func() error) error {
	var err error
	for i := 0; i < 60; i++ {
		if err = rm(); err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

// Logout logs the running node out of the tailnet (its node key is
// invalidated; an ephemeral node is removed at once). No-op when the node
// isn't running.
func (t *TSNet) Logout(ctx context.Context) error {
	t.mu.Lock()
	s := t.srv
	t.mu.Unlock()
	if s == nil {
		return nil
	}
	lc, err := s.LocalClient()
	if err != nil {
		return err
	}
	return lc.Logout(ctx)
}

// Self is the node's name and addresses as the control server set them
// (the hostname may carry a suffix such as -1 when the name was taken).
func (t *TSNet) Self(ctx context.Context) (dnsName string, ips []string) {
	t.mu.Lock()
	s := t.srv
	t.mu.Unlock()
	if s == nil {
		return "", nil
	}
	lc, err := s.LocalClient()
	if err != nil {
		return "", nil
	}
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil || st.Self == nil {
		return "", nil
	}
	for _, ip := range st.TailscaleIPs {
		ips = append(ips, ip.String())
	}
	return strings.TrimSuffix(st.Self.DNSName, "."), ips
}

// Listen starts the node (if needed) and listens on the tailnet port.
func (t *TSNet) Listen(ctx context.Context) (net.Listener, error) {
	s, err := t.server(ctx)
	if err != nil {
		return nil, err
	}
	return s.Listen("tcp", ":"+strconv.Itoa(t.cfg.Port))
}

// Dial connects to node ("hostname[:port]" or "100.x.y.z[:port]") through
// the embedded node and reports the tailscale path (direct / peer relay /
// DERP) of the peer.
func (t *TSNet) Dial(ctx context.Context, node NodeID) (net.Conn, PathInfo, error) {
	s, err := t.server(ctx)
	if err != nil {
		return nil, PathInfo{}, err
	}
	addr := splitHostPortDefault(string(node), t.cfg.Port)
	start := time.Now()
	c, err := s.Dial(ctx, "tcp", addr)
	if err != nil {
		return nil, PathInfo{}, err
	}
	pi := PathInfo{Transport: t.Name(), Kind: PathUnknown, RTT: time.Since(start)}
	ap, perr := netip.ParseAddrPort(c.RemoteAddr().String())
	if perr == nil {
		pi.Kind, pi.Detail = t.path(ctx, s, ap.Addr())
	} else {
		return c, pi, nil
	}
	return &tsConn{Conn: c, t: t, s: s, ip: ap.Addr(), pi: pi, at: time.Now()}, pi, nil
}

// tsConn reports the peer's current tailscale path (it moves between DERP,
// peer relay and direct while the connection lives); re-read at most every
// 2 s.
type tsConn struct {
	net.Conn
	t  *TSNet
	s  *tsnet.Server
	ip netip.Addr

	mu sync.Mutex
	pi PathInfo
	at time.Time
}

// PathInfo implements the live path of client.Conn.
func (c *tsConn) PathInfo() PathInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) >= 2*time.Second {
		c.at = time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if k, d := c.t.path(ctx, c.s, c.ip); k != PathUnknown {
			c.pi.Kind, c.pi.Detail = k, d
		}
		cancel()
	}
	return c.pi
}

func (t *TSNet) path(ctx context.Context, s *tsnet.Server, ip netip.Addr) (PathKind, string) {
	lc, err := s.LocalClient()
	if err != nil {
		return PathUnknown, ""
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	st, err := lc.Status(sctx)
	if err != nil {
		return PathUnknown, ""
	}
	for _, p := range st.Peer {
		for _, a := range p.TailscaleIPs {
			if a != ip {
				continue
			}
			return ClassifyPath(p.CurAddr, p.PeerRelay, p.Relay)
		}
	}
	return PathUnknown, ""
}

// ClassifyPath maps tailscale peer status fields to a PathKind.
func ClassifyPath(curAddr, peerRelay, derpRegion string) (PathKind, string) {
	switch {
	case curAddr != "":
		return PathDirect, curAddr
	case peerRelay != "":
		return PathPeerRelay, peerRelay
	case derpRegion != "":
		return PathDERP, derpRegion
	}
	return PathUnknown, ""
}

// Close shuts the embedded node down, cancelling (and waiting for) a start
// in progress; the transport can't be started again.
func (t *TSNet) Close() error {
	t.bgCancel()
	t.mu.Lock()
	s, ch := t.srv, t.starting
	t.srv, t.closed = nil, true
	t.mu.Unlock()
	if ch != nil {
		// A start in progress ends quickly once cancelled; wait for it, so
		// the node is down (its state dir free) when Close returns.
		<-ch
	}
	if s == nil {
		return nil
	}
	return s.Close()
}

// Starting reports whether the node is coming up (a start is in progress).
func (t *TSNet) Starting() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.starting != nil
}

// AdvertiseURL is "tailscale://<tailnet IPv4>:<port>" once the node is up
// (the IP works with the embedded node and with a system tailscaled on the
// client alike), else "tailscale://<hostname>:<port>".
func (t *TSNet) AdvertiseURL(ln net.Listener) string {
	host := t.cfg.Hostname
	t.mu.Lock()
	s := t.srv
	t.mu.Unlock()
	if s != nil {
		if ip4, _ := s.TailscaleIPs(); ip4.IsValid() {
			host = ip4.String()
		}
	}
	return "tailscale://" + net.JoinHostPort(host, strconv.Itoa(t.cfg.Port))
}

// Started reports whether the node is up.
func (t *TSNet) Started() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.srv != nil
}
