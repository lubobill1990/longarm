package hub

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/conntrack"
	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
	"github.com/lubobill1990/longarm/internal/sched"
	"github.com/lubobill1990/longarm/internal/telemetry"
	"github.com/lubobill1990/longarm/internal/telemetry/tgrpc"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Hub is a running longarm-hub.
type Hub struct {
	cfg    *Config
	log    *slog.Logger
	store  *store.Store
	ca     *pki.CA
	params sched.Params
	bus    *bus

	hubCert, relayCert *pki.Bundle
	revoked            *pki.RevocationList
	joinLimit          *ipLimiter
	loginLimit         *ipLimiter
	loginMu            sync.Mutex
	renewLimit         *ipLimiter // keyed by node name
	// certMu serializes issuing (approve, renew) against revoking, so a
	// certificate issued during a revocation cannot escape it.
	certMu sync.Mutex
	conns  *conntrack.Tracker

	mu           sync.Mutex
	nodes        map[string]*node
	jobs         map[string]map[string]*jobEntry // worker -> id -> job
	reservations map[string]*reservation         // job id
	queue        []*queued
	profiles     map[string]*pb.Profile // latest revision
	stable       map[string]*pb.Profile // last revision past its canary
	knownSerials map[string]bool
	revVersion   int64
	sessionSeq   uint64
	queueSeq     uint64
	kick         chan struct{}   // re-run the queue
	rollout      *pb.Rollout     // the current / last release rollout
	tailnet      tailnetProvider // nil = no tailnet enrollment
	// TailnetKey calls per node: workers 5, then one per hour; clients
	// 10, then one per 10 minutes.
	tailnetWorkerLimit, tailnetClientLimit *ipLimiter
	tailnetMu                              sync.Mutex
	tailnetIssued                          map[string]issuedKey   // enrollment -> minted key (JoinStatus polls)
	tailnetMintErr                         map[string]mintFailure // last failed mint per role (tailnetMu)
	pending                                map[string]*pendingReq // requests relayed to workers, by request id
	links                                  map[string]*linkEntry  // latest link report per client
	// multi-tenancy: users, the policy version workers have to have, users
	// whose data workers purge, and whether separation was on at the last push
	tenants        map[string]*tenant
	tenancyVersion int64
	purged         []string
	lastTenancyOn  bool
	entra          *entraState // nil = no [auth.entra]
	pair           *pairing    // pending pairings (link.go)
	// port links (portlinks.go): by id; the version counts pushes
	portLinks        map[string]*portLinkEntry
	portLinksVersion int64

	started time.Time
	relay   *relay.Server
	telem   *hubTelemetry // nil = telemetry off (telemetry.go)

	addrMu sync.Mutex
	addrs  map[string]string // "grpc", "http", "udp" -> bound address
}

// node is the in-memory state of a registered node.
type node struct {
	rec         *store.Node
	sess        *nodeSession // nil = offline
	lastReport  time.Time
	metrics     *pb.MetricsSnapshot
	localDrain  bool
	localReason string
	toolchain   *pb.ToolchainReport
	tailnet     *pb.TailnetState   // as last reported (workers)
	savedSeen   time.Time          // last time LastSeen was persisted
	history     []*pb.MetricsPoint // recent metrics (sparklines)
	maxJobs     int32              // local limits (NodeState)
	reservedGB  float64
}

type nodeSession struct {
	id     uint64
	out    chan *pb.HubCommand
	cancel context.CancelFunc
}

type jobEntry struct {
	job     *pb.Job
	updated time.Time
}

type reservation struct {
	jobID  string
	worker string
	memGB  float64
	at     time.Time
	owner  string // the user who placed it (quotas)
}

// Open loads the hub's state; the CA must exist.
func Open(cfg *Config, log *slog.Logger) (*Hub, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	ca, err := pki.LoadCA(filepath.Join(cfg.DataDir, CADir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoCA
	}
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath()), 0o700); err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.DatabasePath())
	if err != nil {
		return nil, err
	}
	h := &Hub{
		cfg: cfg, log: log, store: st, ca: ca, params: cfg.SchedParams(), bus: newBus(),
		joinLimit: newIPLimiter(0.2, 10), loginLimit: newIPLimiter(0.1, 5), renewLimit: newIPLimiter(0.5, 10), conns: conntrack.New(),
		tailnetWorkerLimit: newIPLimiter(1.0/3600, 5), tailnetClientLimit: newIPLimiter(1.0/600, 10), nodes: map[string]*node{},
		jobs: map[string]map[string]*jobEntry{}, reservations: map[string]*reservation{},
		profiles: map[string]*pb.Profile{}, stable: map[string]*pb.Profile{}, knownSerials: map[string]bool{}, kick: make(chan struct{}, 1),
		pending: map[string]*pendingReq{}, links: map[string]*linkEntry{}, tenants: map[string]*tenant{}, portLinks: map[string]*portLinkEntry{},
		started: time.Now(), addrs: map[string]string{}, entra: newEntraState(&cfg.Auth.Entra), pair: newPairing(),
	}
	if h.tailnet, err = newTailnetProvider(cfg.Tailnet); err != nil {
		st.Close()
		return nil, err
	}
	if err := h.load(); err != nil {
		st.Close()
		return nil, err
	}
	hosts := cfg.serverHosts()
	if h.hubCert, err = ensureServerCert(ca, filepath.Join(cfg.DataDir, HubTLSDir), pki.RoleHub, cfg.Name, hosts); err != nil {
		st.Close()
		return nil, fmt.Errorf("hub certificate: %w", err)
	}
	if h.relayCert, err = ensureServerCert(ca, filepath.Join(cfg.DataDir, RelayTLSDir), pki.RoleRelay, cfg.Name, hosts); err != nil {
		st.Close()
		return nil, fmt.Errorf("relay certificate: %w", err)
	}
	h.hubCert, h.relayCert = h.hubCert.Live(), h.relayCert.Live()
	return h, nil
}

// Store gives CLI subcommands (user add, release add) access to the db.
func (h *Hub) Store() *store.Store { return h.store }

// CA returns the hub's CA.
func (h *Hub) CA() *pki.CA { return h.ca }

// Close releases the database.
func (h *Hub) Close() error { return h.store.Close() }

func (h *Hub) load() error {
	recs, err := h.store.Nodes()
	if err != nil {
		return err
	}
	for _, r := range recs {
		n := &node{rec: r}
		if len(r.Toolchain) > 0 {
			tc := &pb.ToolchainReport{}
			if protojson.Unmarshal(r.Toolchain, tc) == nil {
				n.toolchain = tc
			}
		}
		if len(r.Tailnet) > 0 {
			ts := &pb.TailnetState{}
			if protojson.Unmarshal(r.Tailnet, ts) == nil {
				n.tailnet = ts
			}
		}
		h.nodes[r.Name] = n
	}
	rows, err := h.store.Jobs()
	if err != nil {
		return err
	}
	for _, r := range rows {
		j := &pb.Job{}
		if proto.Unmarshal(r.Data, j) != nil {
			continue
		}
		h.jobMap(r.Worker)[r.ID] = &jobEntry{job: j, updated: r.UpdatedAt}
	}
	profs, err := h.store.Profiles()
	if err != nil {
		return err
	}
	for name, d := range profs {
		latest, stable, err := h.decodeProfile(d)
		if err != nil {
			h.log.Warn("skipping unreadable profile", "profile", name, "err", err)
			continue
		}
		h.profiles[name] = latest
		if stable != nil {
			h.stable[name] = stable
		}
	}
	h.loadRollout()
	if err := h.loadTenants(); err != nil {
		return err
	}
	if err := h.loadPortLinks(); err != nil {
		return err
	}
	h.lastTenancyOn = h.tenancyOnLocked()
	if on, _ := h.store.Meta("tenancy_on"); on != strconv.FormatBool(h.lastTenancyOn) {
		// separation turned on or off (hub.toml) since the last policy
		h.tenancyChangedLocked()
	}
	h.recheckPortLinksLocked() // isolation or hub.toml may have changed since
	v, _ := h.store.Meta("revocation_version")
	h.revVersion, _ = strconv.ParseInt(v, 10, 64)
	h.revoked, _ = pki.LoadRevocationList("")
	return h.rebuildRevocations()
}

func (h *Hub) jobMap(worker string) map[string]*jobEntry {
	m := h.jobs[worker]
	if m == nil {
		m = map[string]*jobEntry{}
		h.jobs[worker] = m
	}
	return m
}

// rebuildRevocations reloads the revocation list from the store.
func (h *Hub) rebuildRevocations() error {
	certs, err := h.store.RevokedCerts(time.Now())
	if err != nil {
		return err
	}
	m := make(map[string]time.Time, len(certs))
	for _, c := range certs {
		m[c.Serial] = c.NotAfter
	}
	return h.revoked.Replace(h.revVersion, m)
}

func (h *Hub) revocationsMsg() *pb.HubCommand {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.revocationsMsgLocked()
}

// revocationsMsgLocked is the revocation list and its version as one
// snapshot (Revoke changes both under h.mu).
func (h *Hub) revocationsMsgLocked() *pb.HubCommand {
	certs, err := h.store.RevokedCerts(time.Now())
	if err != nil {
		h.log.Warn("revocations", "err", err)
	}
	r := &pb.Revocations{Version: h.revVersion}
	for _, c := range certs {
		r.Certs = append(r.Certs, &pb.RevokedCert{Serial: c.Serial, NotAfter: tspb(c.NotAfter), Node: c.Node})
	}
	return &pb.HubCommand{Msg: &pb.HubCommand_Revocations{Revocations: r}}
}

// Addr returns a bound listener address ("grpc", "http", "udp").
func (h *Hub) Addr(name string) string {
	h.addrMu.Lock()
	defer h.addrMu.Unlock()
	return h.addrs[name]
}

func (h *Hub) setAddr(name, a string) {
	h.addrMu.Lock()
	h.addrs[name] = a
	h.addrMu.Unlock()
}

// EndpointsFile lists the bound addresses while the hub runs (tests,
// scripts): "grpc=...", "http=...", "udp=...".
const EndpointsFile = "hub.endpoints"

// Serve runs the listeners until ctx is done.
func (h *Hub) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	g := grpc.NewServer(
		grpc.Creds(h.conns.Wrap(credentials.NewTLS(h.apiTLS()))),
		grpc.ChainUnaryInterceptor(tgrpc.UnaryServer(h.tel), h.unaryAuth),
		grpc.ChainStreamInterceptor(tgrpc.StreamServer(h.tel), h.streamAuth),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 20 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	pb.RegisterTelemetryServer(g, &telemetryServer{h: h})
	pb.RegisterEnrollServer(g, &enrollServer{h: h})
	pb.RegisterHubNodeServer(g, &hubNodeServer{h: h})
	pb.RegisterFleetServer(g, &fleetServer{h: h})
	pb.RegisterHubAdminServer(g, &hubAdminServer{h: h})
	ds, err := newDiagServer(h)
	if err != nil {
		return err
	}
	pb.RegisterDiagnosticsServer(g, ds)

	tcp, err := net.Listen("tcp", h.cfg.Listen)
	if err != nil {
		return err
	}
	h.setAddr("grpc", tcp.Addr().String())
	mux := newALPNMux(tcp)
	grpcLn := mux.Default()

	var wg sync.WaitGroup
	errc := make(chan error, 4)
	run := func(name string, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil && ctx.Err() == nil {
				errc <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}

	if h.cfg.RelayUDP != "off" {
		rate, err := parseRate(h.cfg.RelayRate)
		if err != nil {
			mux.Close()
			return err
		}
		rtls, err := relay.ServerTLS(h.relayCert)
		if err != nil {
			mux.Close()
			return err
		}
		rtls.VerifyPeerCertificate = h.revoked.VerifyNotRevoked
		h.relay = &relay.Server{TLS: rtls, Logger: h.log.With("component", "relay"), Rate: rate, Burst: int(rate)}
		if rate > 0 && rate < 64<<10 {
			h.relay.Burst = 64 << 10
		}
		relayTCP := mux.Route(relay.ALPN)
		run("relay tcp", func() error { return h.relay.ServeTCP(relayTCP) })
		pc, err := net.ListenPacket("udp", h.cfg.RelayUDP)
		if err != nil {
			mux.Close()
			h.relay.Close()
			return fmt.Errorf("relay udp: %w", err)
		}
		h.setAddr("udp", pc.LocalAddr().String())
		// The second STUN observation point (NAT type detection), like
		// longarm-relay --stun2.
		var stunPC net.PacketConn
		if a := h.cfg.RelaySTUN2; a != "off" {
			if a == "auto" {
				ua := pc.LocalAddr().(*net.UDPAddr)
				host, _, _ := net.SplitHostPort(h.cfg.RelayUDP)
				a = net.JoinHostPort(host, strconv.Itoa(ua.Port+1))
			}
			if spc, err := net.ListenPacket("udp", a); err != nil {
				h.log.Warn("relay: second STUN port unavailable; NAT type detection off", "addr", a, "err", err)
			} else {
				stunPC = spc
				h.setAddr("stun2", spc.LocalAddr().String())
			}
		}
		// The advertised STUN endpoints are fixed before any node connects
		// (ServeSTUN registers its port only once its goroutine runs).
		// relay_stun_advertise may mix "self" (these ports) with external
		// servers. Should our own answers turn out to be SNATed (cloud
		// containers), the relay drops its ":port" entries by itself.
		own := []string{fmt.Sprintf(":%d", pc.LocalAddr().(*net.UDPAddr).Port)}
		if stunPC != nil {
			own = append(own, fmt.Sprintf(":%d", stunPC.LocalAddr().(*net.UDPAddr).Port))
		}
		h.relay.STUN = relay.ExpandSTUN(h.cfg.RelaySTUNAdvertise, own)
		h.log.Info("relay: advertised STUN servers", "stun", h.relay.STUN)
		if stunPC != nil {
			run("relay stun2", func() error { return h.relay.ServeSTUN(stunPC) })
		}
		run("relay udp", func() error { return h.relay.ServeQUIC(pc) })
		h.mu.Lock()
		h.updateRelayPairsLocked() // linked workers may talk through the relay
		h.mu.Unlock()
	}
	h.startTelemetry(ctx, run)
	run("mux", mux.Serve)
	run("grpc", func() error { return g.Serve(grpcLn) })

	// the panel/API: plain http (behind a proxy), and/or https with a Let's
	// Encrypt certificate (acme.go) plus its http->https redirect
	var httpSrvs []*http.Server
	stopHTTP := func() {
		for _, s := range httpSrvs {
			sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
			s.Shutdown(sctx)
			scancel()
			s.Close()
		}
	}
	serveHTTP := func(key, addr string, srv *http.Server) error {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			mux.Close()
			if h.relay != nil {
				h.relay.Close()
			}
			stopHTTP()
			return err
		}
		h.setAddr(key, l.Addr().String())
		httpSrvs = append(httpSrvs, srv)
		run(key, func() error {
			var err error
			if srv.TLSConfig != nil {
				err = srv.ServeTLS(l, "", "")
			} else {
				err = srv.Serve(l)
			}
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
		return nil
	}
	var handler http.Handler
	if h.cfg.HTTP != "off" || h.cfg.HTTPS != "" {
		handler = h.httpHandler()
	}
	if h.cfg.HTTP != "off" {
		if err := serveHTTP("http", h.cfg.HTTP, &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}); err != nil {
			return err
		}
	}
	if h.cfg.HTTPS != "" {
		m := h.acmeManager()
		tc := m.TLSConfig()
		if get := tc.GetCertificate; get != nil {
			tc.GetCertificate = func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
				c, err := get(hi)
				if err != nil && hi.ServerName == h.cfg.ACMEHost() {
					h.tel().Count("acme.get_certificate_failed", nil, 1) // the error itself is in the log
				}
				return c, err
			}
		}
		tc.MinVersion = tls.VersionTLS12
		if err := serveHTTP("https", h.cfg.HTTPS, &http.Server{Handler: handler, TLSConfig: tc, ReadHeaderTimeout: 10 * time.Second}); err != nil {
			return err
		}
		if h.cfg.HTTPSRedirect != "" {
			if err := serveHTTP("https_redirect", h.cfg.HTTPSRedirect, &http.Server{Handler: m.HTTPHandler(nil), ReadHeaderTimeout: 10 * time.Second}); err != nil {
				return err
			}
		}
	}

	wg.Add(1)
	go func() { defer wg.Done(); h.maintain(ctx) }()

	ep := filepath.Join(h.cfg.DataDir, EndpointsFile)
	var lines []string
	for _, k := range []string{"grpc", "http", "https", "udp"} {
		if a := h.Addr(k); a != "" {
			lines = append(lines, k+"="+a)
		}
	}
	os.WriteFile(ep, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	defer os.Remove(ep)
	h.log.Info("longarm-hub started", "version", version.Version, "grpc", h.Addr("grpc"), "relay_udp", h.Addr("udp"), "stun2", h.Addr("stun2"),
		"http", h.Addr("http"), "https", h.Addr("https"), "data", h.cfg.DataDir, "ca", pki.Fingerprint(h.ca.Cert))

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
		h.log.Error("listener failed", "err", runErr)
	}
	cancel()
	h.bus.closeAll()
	mux.Close()
	if h.relay != nil {
		h.relay.Close()
	}
	stopHTTP()
	stopped := make(chan struct{})
	go func() { g.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		g.Stop()
	}
	wg.Wait()
	h.log.Info("longarm-hub stopped")
	return runErr
}

// maintain runs periodic work: offline detection, reservation expiry, the
// queue, profile rollouts.
func (h *Hub) maintain(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	certs := time.NewTicker(6 * time.Hour)
	defer certs.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.sweep()
		case <-certs.C:
			h.renewServerCerts()
		case <-h.kick:
		}
		h.runQueue()
	}
}

func (h *Hub) sweep() {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, n := range h.nodes {
		if n.sess != nil && now.Sub(n.lastReport) > h.cfg.Heartbeat.Offline.Duration {
			h.log.Warn("node silent; marking offline", "node", n.rec.Name, "last_report", n.lastReport)
			n.sess.cancel()
			h.setOfflineLocked(n, n.sess.id)
		}
	}
	for id, r := range h.reservations {
		if now.Sub(r.at) > h.cfg.Scheduler.ReservationTTL.Duration {
			h.log.Info("placement expired unused", "job", id, "worker", r.worker)
			delete(h.reservations, id)
		}
	}
	h.sweepPortLinksLocked(now)
	h.advanceRolloutsLocked()
	h.advanceReleaseLocked()
}

// kickQueue asks the maintenance loop to re-run the queue soon.
func (h *Hub) kickQueue() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// parseRate parses "0", "500K", "32M", "1G" (bytes/s).
func parseRate(s string) (float64, error) {
	s = strings.TrimSpace(strings.ToUpper(strings.TrimSuffix(strings.TrimSuffix(s, "/s"), "B")))
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("bad relay_rate %q", s)
	}
	return v * mult, nil
}

// NewLogger builds the hub's logger (stderr, plus file if configured).
func NewLogger(cfg *Config) (*slog.Logger, io.Closer, error) {
	var w io.Writer = os.Stderr
	var c io.Closer = io.NopCloser(nil)
	if cfg.LogFile != "" {
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, err
		}
		w, c = io.MultiWriter(os.Stderr, f), f
	}
	// WARN / ERROR records also become telemetry traces (when the hub
	// collects telemetry), records of an operation get trace=<id>.
	return slog.New(telemetry.NewHandler(slog.NewTextHandler(w, nil))), c, nil
}
