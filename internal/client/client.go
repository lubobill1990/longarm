// Package client is the longarm side: configuration (workers.toml, TLS bundle)
// and gRPC connections to workers over a Transport.
package client

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/agentauth"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/telemetry/tgrpc"
	"github.com/lubobill1990/longarm/internal/transport"
	"github.com/lubobill1990/longarm/internal/transport/longnet"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Config-dir layout.
const (
	WorkersFile = "workers.toml"
	TLSDir      = "tls"
	CADir       = "ca"
	TSNetDir    = "tsnet"
)

// DefaultConfigDir is $LONGARM_CONFIG_DIR or ~/.config/longarm (same path on all
// platforms, to keep docs and scripts simple).
func DefaultConfigDir() string {
	if d := os.Getenv("LONGARM_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".longarm-client"
	}
	return filepath.Join(home, ".config", "longarm")
}

// Worker is one [[worker]] entry of workers.toml.
type Worker struct {
	Name string `toml:"name"`
	// Address: "host:port" (tcp), "tailnet-name[:port]" (tailscale), or the
	// relay "host:port" (longnet; default: [longnet] relay).
	Address string `toml:"address"`
	// Transport: "tcp" (default), "tailscale" or "longnet".
	Transport string `toml:"transport"`
	// CertName is the name in the worker's certificate (default: Name).
	CertName string `toml:"cert_name"`
	// Endpoints are more legs to race against the primary one, as URLs:
	// "tcp://100.64.0.11:7701", "longnet://relay:7443", "tailscale://longarm-w1".
	Endpoints []string `toml:"endpoints"`

	// Alt is Endpoints parsed (for hub-known workers: the advertised
	// endpoints beyond the primary one).
	Alt []Endpoint `toml:"-"`

	// Note, if set, leads the error when no leg connects (longarm: "worker
	// X is offline (the hub last saw it 2h ago)").
	Note string `toml:"-"`
}

// TailscaleConfig is the [tailscale] section for the embedded client node.
type TailscaleConfig struct {
	Hostname   string `toml:"hostname"`    // default longarm-client-<hostname>
	ControlURL string `toml:"control_url"` // default Tailscale SaaS
}

// LongNetConfig is the [longnet] section: settings of the longnet client node.
type LongNetConfig struct {
	Relay         string `toml:"relay"`          // default relay "host:port" (UDP and TCP)
	RelayTCP      string `toml:"relay_tcp"`      // relay TLS/TCP address if different
	Mode          string `toml:"mode"`           // auto (default) | udp | tcp
	DisableDirect bool   `toml:"disable_direct"` // never hole-punch; always relay
}

// Config is workers.toml.
type Config struct {
	Tailscale TailscaleConfig  `toml:"tailscale"`
	LongNet   LongNetConfig    `toml:"longnet"`
	Hub       hubclient.Config `toml:"hub"` // default: hub.toml (written by longarm join)
	Dial      DialConfig       `toml:"dial"`
	Workers   []Worker         `toml:"worker"`
	// AgentAuth is [agent_auth]: forwarding of coding-agent credentials
	// (longarm run --agent-auth).
	AgentAuth agentauth.Config `toml:"agent_auth"`

	Dir string `toml:"-"`
}

// LoadConfig reads <dir>/workers.toml (missing file = no workers).
func LoadConfig(dir string) (*Config, error) {
	c := &Config{Dir: dir}
	if _, err := toml.DecodeFile(filepath.Join(dir, WorkersFile), c); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, WorkersFile), err)
	}
	if c.Hub.Address == "" {
		hf, err := hubclient.LoadFile(dir)
		if err != nil {
			return nil, err
		}
		c.Hub = hf
	}
	// The tailnet the hub enrolled this client in (longarm join + Headscale).
	if c.Tailscale.ControlURL == "" {
		c.Tailscale.ControlURL = c.Hub.Tailnet.ControlURL
	}
	if c.Tailscale.Hostname == "" {
		c.Tailscale.Hostname = c.Hub.Tailnet.Hostname
	}
	seen := map[string]bool{}
	for i := range c.Workers {
		w := &c.Workers[i]
		if w.Transport == "longnet" && w.Address == "" {
			w.Address = c.LongNet.Relay
		}
		if w.Name == "" || w.Address == "" {
			return nil, fmt.Errorf("%s: worker #%d needs name and address", WorkersFile, i+1)
		}
		if seen[w.Name] {
			return nil, fmt.Errorf("%s: duplicate worker %q", WorkersFile, w.Name)
		}
		seen[w.Name] = true
		if w.Transport == "" {
			w.Transport = "tcp"
		}
		if w.Transport != "tcp" && w.Transport != "tailscale" && w.Transport != "longnet" {
			return nil, fmt.Errorf("%s: worker %q: unknown transport %q", WorkersFile, w.Name, w.Transport)
		}
		if w.CertName == "" {
			w.CertName = w.Name
		}
		for _, s := range w.Endpoints {
			e, err := ParseEndpoint(s)
			if err != nil {
				return nil, fmt.Errorf("%s: worker %q: %w", WorkersFile, w.Name, err)
			}
			w.Alt = append(w.Alt, e)
		}
	}
	if c.Dial.Policy != "" {
		return nil, fmt.Errorf("%s: [dial] policy is gone: set the transport policy with `longarm transport %s` (or LONGARM_TRANSPORT / --transport)", WorkersFile, c.Dial.Policy)
	}
	if c.Dial.ReEvaluate != "" {
		if _, err := time.ParseDuration(c.Dial.ReEvaluate); err != nil {
			return nil, fmt.Errorf("%s: [dial] reevaluate: %w", WorkersFile, err)
		}
	}
	return c, nil
}

// Worker finds a worker by name. An address ("host:port") not in the config
// is accepted too, as an ad-hoc tcp worker whose certificate name is given
// after '@' ("w1@127.0.0.1:7701") or defaults to the host part.
func (c *Config) Worker(name string) (Worker, error) {
	for _, w := range c.Workers {
		if w.Name == name {
			return w, nil
		}
	}
	certName, addr := "", name
	for i := 0; i < len(name); i++ {
		if name[i] == '@' {
			certName, addr = name[:i], name[i+1:]
			break
		}
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		if certName == "" {
			certName = host
		}
		return Worker{Name: name, Address: addr, Transport: "tcp", CertName: certName}, nil
	}
	return Worker{}, UnknownWorkerError(name, c.Dir)
}

// UnknownWorkerError is the error for a worker name nobody knows; it
// mentions workers.toml only when there is one.
func UnknownWorkerError(name, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, WorkersFile)); err == nil {
		return fmt.Errorf("no worker named %s (not in %s either): `longarm ls` lists them", name, filepath.Join(dir, WorkersFile))
	}
	return fmt.Errorf("no worker named %s: `longarm ls` lists them", name)
}

// NotSignedInHint is what to do on a machine without a client certificate
// (goos: runtime.GOOS; Windows has the Start menu's Longarm app).
func NotSignedInHint(goos string) string {
	if goos == "windows" {
		return "This machine isn't signed in: run `longarm login` (or open Longarm from the Start menu → Account → Sign in)"
	}
	return "This machine isn't signed in: run `longarm login`"
}

// ErrNotSignedIn: the config dir has no client certificate (tls/).
var ErrNotSignedIn = errors.New(NotSignedInHint(runtime.GOOS))

// Dialer creates connections to workers; it owns the (lazily started)
// tailscale client node.
type Dialer struct {
	cfg    *Config
	bundle *pki.Bundle

	mu      sync.Mutex
	ts      *transport.TSNet
	longnet map[string]*longnet.Transport // by relay address

	policy     string
	reEvaluate time.Duration
	cache      *legCache

	// SkipLeg, if set, names a reason not to dial a leg at all (longarm: an
	// longnet leg through the relay of a hub known to be down). Set it before
	// the first Dial.
	SkipLeg func(e Endpoint) string

	// Proxy, if set, dials the tailscale and longnet legs when it runs (a
	// one-shot longarm through longarm daemon's long-lived nodes); this process
	// starts its own nodes only when it doesn't. Set it before the first
	// Dial.
	Proxy LegProxy
	// LiveLegs, if set, returns the current per-leg state of a worker as a
	// long-lived monitor sees it (longarm daemon's pool view); a race uses it to
	// distrust a sticky winner whose path degraded. nil/empty = unknown.
	LiveLegs func(ctx context.Context, worker string) []LiveLeg
	// LegTransport, if set, may supply the transport of a leg (longarmd: its
	// own longnet / tailscale nodes for port links); nil = the default.
	LegTransport func(e Endpoint) transport.Transport
	// OneShot: this process exits soon. Close doesn't wait for the
	// tailscale node to shut down, and a cold own tailscale node gets no
	// head start (the other legs race it at once).
	OneShot bool
}

// NewDialer loads the client TLS bundle from <dir>/tls.
func NewDialer(cfg *Config) (*Dialer, error) {
	b, err := pki.LoadBundle(filepath.Join(cfg.Dir, TLSDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotSignedIn
	}
	if err != nil {
		return nil, fmt.Errorf("%w: sign in again with `longarm login`", err)
	}
	if b.Identity.Role == pki.RoleWorker {
		return nil, WorkerOnlyError(filepath.Join(cfg.Dir, TLSDir))
	}
	// A broken trust.toml fails here rather than silently trusting every
	// worker (fail closed).
	if b.Trust, err = TrustStore(cfg.Dir).Source(true); err != nil {
		return nil, fmt.Errorf("end-to-end trust: %w", err)
	}
	d := &Dialer{cfg: cfg, bundle: b.Live(), reEvaluate: DefaultReEvaluate, cache: loadLegCache(cfg.Dir)}
	if v, err := time.ParseDuration(cfg.Dial.ReEvaluate); err == nil {
		d.reEvaluate = v
	}
	p := LoadSettings(cfg.Dir).Transport
	if env := os.Getenv("LONGARM_TRANSPORT"); env != "" {
		p = env
	}
	if err := d.SetPolicy(p); err != nil {
		return nil, err
	}
	return d, nil
}

// WorkerOnlyError: tlsDir holds a worker certificate, not one of a user.
func WorkerOnlyError(tlsDir string) error {
	return fmt.Errorf("this machine is signed in as a worker only (%s holds a worker certificate): run `longarm login` to sign in as you", tlsDir)
}

// NewDialerFor is a dialer with an already loaded bundle of any role (the
// "from" worker of a port link dials the target worker with its worker
// certificate); transport-cache.json goes to cfg.Dir. Policy: race.
func NewDialerFor(cfg *Config, b *pki.Bundle) *Dialer {
	return &Dialer{cfg: cfg, bundle: b, reEvaluate: DefaultReEvaluate, cache: loadLegCache(cfg.Dir), policy: PolicyRace}
}

// Reload picks up a renewed client certificate from <dir>/tls; new
// connections (and reconnects) present it. The trust policy stays the
// bundle's (WatchTrust).
func (d *Dialer) Reload() error {
	b, err := pki.LoadBundle(filepath.Join(d.cfg.Dir, TLSDir))
	if err != nil {
		return err
	}
	return d.bundle.Update(b)
}

// Identity of this client.
func (d *Dialer) Identity() pki.Identity { return d.bundle.Identity }

// Bundle is the client TLS bundle.
func (d *Dialer) Bundle() *pki.Bundle { return d.bundle }

// longnetTransport returns the longnet node for a relay (one per relay).
func (d *Dialer) longnetTransport(relayAddr string) (transport.Transport, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t := d.longnet[relayAddr]; t != nil {
		return t, nil
	}
	r := d.cfg.LongNet
	relayTCP := ""
	if relayAddr == r.Relay { // relay_tcp belongs to the default relay only
		relayTCP = r.RelayTCP
	}
	if relayTCP == "" {
		relayTCP = hubRelayTCP(relayAddr, d.cfg.Hub.Address)
	}
	o, err := longnet.ClientOptions(relayAddr, relayTCP, r.Mode, r.DisableDirect, d.bundle, nil)
	if err != nil {
		return nil, err
	}
	if d.longnet == nil {
		d.longnet = map[string]*longnet.Transport{}
	}
	t := longnet.New(o)
	d.longnet[relayAddr] = t
	return t, nil
}

// hubRelayTCP is the TLS/TCP fallback of the hub's own relay: the relay
// built into the hub shares the hub's TCP port, while its UDP port may be
// another (Azure: UDP 7445, TCP 7443). "" for other relays.
func hubRelayTCP(relayAddr, hubAddr string) string {
	rh, rp, err1 := net.SplitHostPort(relayAddr)
	hh, hp, err2 := net.SplitHostPort(hubAddr)
	if err1 != nil || err2 != nil || rh != hh || rp == hp {
		return ""
	}
	return hubAddr
}

// TSNetDirFor is the base of the embedded tailscale node's state dirs of a
// config dir ("tsnet"; per control server "tsnet-<host>").
func TSNetDirFor(dir string) string { return filepath.Join(dir, TSNetDir) }

// TSNetStateDir is the state dir of the embedded tailscale node.
func (c *Config) TSNetStateDir() string {
	return transport.StateDirFor(TSNetDirFor(c.Dir), c.Tailscale.ControlURL)
}

// hubTailnet reports whether the embedded node's tailnet is the hub's
// (the hub then hands out keys).
func (c *Config) hubTailnet() bool {
	return c.Hub.Enabled() && c.Hub.Tailnet.ControlURL != "" && transport.SameControl(c.Tailscale.ControlURL, c.Hub.Tailnet.ControlURL)
}

// tsnetConfigured reports whether this client has an embedded tailscale
// node it can start without a login prompt: an auth key, state from an
// earlier start, or the hub's tailnet (the hub hands out keys).
func (d *Dialer) tsnetConfigured() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if os.Getenv("TS_AUTHKEY") != "" || d.cfg.tsAuthKey() != "" || d.cfg.hubTailnet() {
		return true
	}
	return transport.Joined(d.cfg.TSNetStateDir())
}

// SetTailnet points the embedded tailscale node at another control server
// (the hub switched providers); call ResetTailnet first when a node may
// be running.
func (d *Dialer) SetTailnet(t hubclient.TailnetConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg.Hub.Tailnet = t
	d.cfg.Tailscale.ControlURL, d.cfg.Tailscale.Hostname = t.ControlURL, t.Hostname
}

// ResetTailnet stops the embedded tailscale node, if any (logging it out
// first with logout): the next tailscale leg starts a new one from the
// current settings. A long-lived longarm daemon re-keys / switches this way.
func (d *Dialer) ResetTailnet(ctx context.Context, logout bool) {
	d.mu.Lock()
	ts := d.ts
	d.ts = nil
	d.mu.Unlock()
	if ts == nil {
		return
	}
	if logout {
		ts.Logout(ctx)
	}
	ts.Close()
}

// hubTailnetKey fetches a key for the embedded node (on control) from the
// hub.
func (d *Dialer) hubTailnetKey(ctx context.Context, hubAddr, control, reason string) (string, error) {
	t, err := hubclient.FetchTailnetKey(ctx, hubAddr, d.bundle, reason)
	if err != nil {
		return "", fmt.Errorf("no tailnet key from the hub: %w", err)
	}
	if !transport.SameControl(t.ControlUrl, control) {
		// The hub switched tailnets: keep the key for that one (the next
		// longarm run uses it).
		hf, err := hubclient.LoadFile(d.cfg.Dir)
		if err == nil && hubclient.SaveTailnet(d.cfg.Dir, &hf, t) == nil {
			hubclient.SaveFile(d.cfg.Dir, hf)
		}
		return "", fmt.Errorf("the hub's tailnet is now %s", t.ControlUrl)
	}
	return t.AuthKey, nil
}

// tsnetTransport returns the (lazily started) embedded tailscale node.
func (d *Dialer) tsnetTransport() transport.Transport {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ts == nil {
		host := d.cfg.Tailscale.Hostname
		if host == "" {
			h, _ := os.Hostname()
			host = "longarm-client-" + h
		}
		dir := d.cfg.TSNetStateDir()
		if d.cfg.Tailscale.ControlURL != "" {
			// State from before per-control state dirs.
			transport.MigrateStateDir(TSNetDirFor(d.cfg.Dir), d.cfg.Tailscale.ControlURL)
		}
		tc := transport.TSNetConfig{
			Hostname: host, Dir: dir, AuthKey: os.Getenv("TS_AUTHKEY"),
			AuthKeyFile: dir + ".authkey",
			ControlURL:  d.cfg.Tailscale.ControlURL,
		}
		if tc.AuthKey == "" && d.cfg.hubTailnet() {
			hubAddr, control := d.cfg.Hub.Address, tc.ControlURL
			tc.KeyFunc = func(ctx context.Context, reason string) (string, error) {
				return d.hubTailnetKey(ctx, hubAddr, control, reason)
			}
		}
		d.ts = transport.NewTSNet(tc)
	}
	return d.ts
}

// WarmTailnet starts the embedded tailscale node in the background when
// this client can start it without a login prompt (longarm daemon: the first
// tailscale leg, its own or proxied, then finds it up).
func (d *Dialer) WarmTailnet() {
	if !d.tsnetConfigured() {
		return
	}
	if ts, ok := d.tsnetTransport().(*transport.TSNet); ok {
		ts.Warm()
	}
}

// oneShotCloseWait bounds how long Close of a OneShot dialer waits for
// the nodes to shut down.
var oneShotCloseWait = 200 * time.Millisecond

// Close releases the tailscale and longnet nodes, if started (a start in
// progress is cancelled). A OneShot dialer doesn't wait for more than
// oneShotCloseWait: the process exits anyway.
func (d *Dialer) Close() error {
	d.mu.Lock()
	lns, ts := d.longnet, d.ts
	d.longnet, d.ts = nil, nil
	d.mu.Unlock()
	var err error
	closeAll := func() {
		for _, t := range lns {
			t.Close()
		}
		if ts != nil {
			err = ts.Close()
		}
	}
	if !d.OneShot {
		closeAll()
		return err
	}
	done := make(chan struct{})
	go func() { closeAll(); close(done) }()
	select {
	case <-done:
		return err
	case <-time.After(oneShotCloseWait):
		return nil
	}
}

// Conn is a connection to one worker.
type Conn struct {
	*grpc.ClientConn
	Worker Worker

	mu   sync.Mutex
	path transport.PathInfo
	raw  net.Conn
	legs []LegResult
	// peer is PeerKey of the worker on the current transport; pin, once
	// set (PinPeer), is the only peer any transport may have.
	peer, pin string
}

// livePath is implemented by transport conns whose path can change (longnet).
type livePath interface{ PathInfo() transport.PathInfo }

// Path is the transport path of the most recent dial (live for longnet, which
// may have moved from the relay to a direct path since).
func (c *Conn) Path() transport.PathInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if lp, ok := c.raw.(livePath); ok {
		return lp.PathInfo()
	}
	return c.path
}

// Legs is the outcome of every transport leg in the most recent dial.
func (c *Conn) Legs() []LegResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]LegResult(nil), c.legs...)
}

// PeerChanged marks the error of a transport refused by PinPeer (gRPC
// flattens dial errors into status messages; Retryable finds it there).
const PeerChanged = "worker identity changed"

// PeerKey identifies the node behind a worker certificate: its end-to-end
// trust node key when the certificate carries one (stable across
// certificate renewals), else the certificate itself.
func PeerKey(c *x509.Certificate) string {
	if t, err := pki.TrustOf(c); err == nil && t != nil && len(t.NodeKey) > 0 {
		return "node:" + base64.StdEncoding.EncodeToString(t.NodeKey)
	}
	h := sha256.Sum256(c.Raw)
	return "cert:" + hex.EncodeToString(h[:])
}

// PinPeer binds the connection to the worker whose certificate cert was
// verified on it (what an operation was authorized for, e.g. forwarding
// agent credentials): every later transport - gRPC reconnects, re-raced
// legs - must present the same node, or the dial fails (PeerChanged) and
// no call goes out on it. It fails when the current transport is already
// another node's (a reconnect since cert was seen).
func (c *Conn) PinPeer(cert *x509.Certificate) error {
	if cert == nil {
		return fmt.Errorf("%s: no worker certificate to pin", PeerChanged)
	}
	key := PeerKey(cert)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pin != "" && c.pin != key {
		return fmt.Errorf("%s: worker %s is pinned to another node", PeerChanged, c.Worker.Name)
	}
	if c.peer != "" && c.peer != key {
		return fmt.Errorf("%s: worker %s reconnected to another node since it was checked", PeerChanged, c.Worker.Name)
	}
	c.pin = key
	return nil
}

func (c *Conn) Node() pb.NodeClient           { return pb.NewNodeClient(c.ClientConn) }
func (c *Conn) Exec() pb.ExecClient           { return pb.NewExecClient(c.ClientConn) }
func (c *Conn) Jobs() pb.JobsClient           { return pb.NewJobsClient(c.ClientConn) }
func (c *Conn) Metrics() pb.MetricsClient     { return pb.NewMetricsClient(c.ClientConn) }
func (c *Conn) Admin() pb.AdminClient         { return pb.NewAdminClient(c.ClientConn) }
func (c *Conn) Sessions() pb.SessionsClient   { return pb.NewSessionsClient(c.ClientConn) }
func (c *Conn) Forward() pb.ForwardClient     { return pb.NewForwardClient(c.ClientConn) }
func (c *Conn) Files() pb.FilesClient         { return pb.NewFilesClient(c.ClientConn) }
func (c *Conn) Workspace() pb.WorkspaceClient { return pb.NewWorkspaceClient(c.ClientConn) }

// MachineUpdate: the owner's update of the worker's machine.
func (c *Conn) MachineUpdate() pb.MachineUpdateClient { return pb.NewMachineUpdateClient(c.ClientConn) }

// Dial returns a lazily-connecting gRPC client for the worker. gRPC
// reconnects by itself (backoff capped at 5 s); every new connection races
// the worker's transport legs (see race.go) and completes app-level mTLS on
// the winner.
func (d *Dialer) Dial(w Worker) (*Conn, error) {
	conn := &Conn{Worker: w}
	explicit := w.Transport == "tailscale" && len(w.Alt) == 0 && d.isConfigured(w.Name)
	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		start := time.Now()
		tc, raw, pi, legs, err := d.race(ctx, w, explicit)
		recordDial(ctx, time.Since(start), pi, legs, err)
		conn.mu.Lock()
		conn.legs = legs
		if err == nil {
			// the transport carries nothing yet: a peer other than the
			// pinned one is refused before a single byte of a call
			key := ""
			if cs := tc.ConnectionState(); len(cs.PeerCertificates) > 0 {
				key = PeerKey(cs.PeerCertificates[0])
			}
			if conn.pin != "" && key != conn.pin {
				conn.mu.Unlock()
				tc.Close()
				return nil, fmt.Errorf("%s: worker %s presented another node than this operation was authorized for", PeerChanged, w.Name)
			}
			conn.path, conn.raw, conn.peer = pi, raw, key
		}
		conn.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return &raceConn{tc}, nil
	}
	bo := backoff.DefaultConfig
	bo.MaxDelay = 5 * time.Second
	cc, err := grpc.NewClient("passthrough:///"+w.Name,
		grpc.WithContextDialer(dial),
		grpc.WithTransportCredentials(preHandshaked{credentials.NewTLS(d.tlsConfig(w))}),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: bo, MinConnectTimeout: connectTimeout()}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 20 * time.Second, PermitWithoutStream: false}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256<<20)),
		grpc.WithChainUnaryInterceptor(tgrpc.UnaryClient("worker")),
		grpc.WithChainStreamInterceptor(tgrpc.StreamClient("worker")),
	)
	if err != nil {
		return nil, err
	}
	conn.ClientConn = cc
	return conn, nil
}

// isConfigured reports whether name is a workers.toml entry.
func (d *Dialer) isConfigured(name string) bool {
	for _, w := range d.cfg.Workers {
		if w.Name == name {
			return true
		}
	}
	return false
}

// Retryable reports whether err is a transient connectivity failure worth
// retrying (worker restarting, network blip).
func Retryable(err error) bool {
	if _, ok := TrustRefusal(err); ok {
		return false // the worker stays refused until someone endorses it
	}
	if err != nil && strings.Contains(err.Error(), PeerChanged) {
		return false // the pinned worker is gone: never send there
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.Aborted, codes.Internal, codes.Unknown:
		return true
	}
	return false
}

// Retry calls fn until it succeeds, fails permanently, or the worker has been
// unreachable for longer than giveUp. fn must call progress() whenever it
// made progress (received data), which resets the unreachable clock. notify
// is called before each retry.
func Retry(ctx context.Context, giveUp time.Duration, notify func(err error, wait time.Duration),
	fn func(ctx context.Context, progress func()) error) error {
	wait := 250 * time.Millisecond
	since := time.Now()
	progress := func() { since = time.Now(); wait = 250 * time.Millisecond }
	for {
		err := fn(ctx, progress)
		if err == nil || !Retryable(err) || ctx.Err() != nil {
			return err
		}
		if time.Since(since) > giveUp {
			return fmt.Errorf("giving up after %s unreachable: %w", giveUp, err)
		}
		if notify != nil {
			notify(err, wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if wait *= 2; wait > 5*time.Second {
			wait = 5 * time.Second
		}
	}
}

// tsAuthKey is a key for the embedded tailscale node waiting next to its
// state dir (minted by the hub at `longarm join` or a provider switch); it is
// removed once the node has state.
func (c *Config) tsAuthKey() string {
	b, err := os.ReadFile(c.TSNetStateDir() + ".authkey")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// connectTimeout bounds one connection attempt (all legs); LONGARM_CONNECT_TIMEOUT
// raises it, e.g. for a cold embedded tailscale node (default 10s; 25s
// when the client has a tailnet, whose fresh node needs a netmap round).
func connectTimeout() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("LONGARM_CONNECT_TIMEOUT")); err == nil && d > 0 {
		return d
	}
	return 10 * time.Second
}
