package hub

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/lubobill1990/longarm/internal/azauth"
	"github.com/lubobill1990/longarm/internal/pii"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
	"github.com/lubobill1990/longarm/internal/telemetry"
	"github.com/lubobill1990/longarm/internal/telemetry/appinsights"
	"github.com/lubobill1990/longarm/internal/telemetry/tgrpc"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TelemetryConfig is [telemetry] in hub.toml: where the fleet's telemetry
// goes (docs/telemetry.md). No connection string = telemetry off fleet-wide:
// nodes stop collecting when the hub tells them.
type TelemetryConfig struct {
	// ConnectionString of an Application Insights resource
	// ("InstrumentationKey=...;IngestionEndpoint=https://...").
	ConnectionString string `toml:"connection_string"`
	// Enabled = false keeps telemetry off with a connection string set.
	Enabled *bool `toml:"enabled"`
}

// On reports whether the hub collects telemetry.
func (t TelemetryConfig) On() bool {
	return strings.TrimSpace(t.ConnectionString) != "" && (t.Enabled == nil || *t.Enabled)
}

// EnvAppInsights overrides [telemetry] connection_string.
const EnvAppInsights = "LONGARM_HUB_APPINSIGHTS_CONNECTION_STRING"

// Telemetry limits of the hub.
const (
	telMaxBatch      = 500
	telRetryOff      = 6 * time.Hour // nodes ask again after this when the hub doesn't collect
	telItemsPerMin   = 1200          // per node, sustained
	telItemsBurst    = 5000
	telScrubberEvery = 5 * time.Minute
)

// telForwardInterval: the longest a partial batch waits (tests shorten it).
var telForwardInterval = 5 * time.Second

// hubTelemetry is the hub's telemetry: the forwarder to Application
// Insights, the hub's own client, and the scrubber that knows every node
// and user name in the registry.
type hubTelemetry struct {
	fwd   *appinsights.Forwarder
	tel   *telemetry.Client
	fleet string
	salt  []byte

	mu      sync.Mutex
	scrub   *pii.Scrubber
	scrubAt time.Time
	scrubN  int
	limits  map[string]*rate.Limiter
}

// startTelemetry sets up telemetry when [telemetry] has a destination;
// the forwarder runs until ctx ends.
func (h *Hub) startTelemetry(ctx context.Context, run func(string, func() error)) {
	if !h.cfg.Telemetry.On() {
		return
	}
	cs, err := appinsights.ParseConnectionString(h.cfg.Telemetry.ConnectionString)
	if err != nil {
		h.log.Warn("telemetry off: bad [telemetry] connection_string", "err", err)
		return
	}
	t := &hubTelemetry{fleet: telemetry.FleetID(h.ca.Cert.Raw), salt: pii.FleetSalt(h.ca.Cert.Raw), limits: map[string]*rate.Limiter{}}
	t.fwd = &appinsights.Forwarder{Conn: cs, Log: h.log.With("component", "telemetry"), Interval: telForwardInterval}
	auth := "instrumentation key"
	if azauth.Available() {
		t.fwd.Tokens = &azauth.ManagedIdentity{}
		auth = "managed identity"
	}
	t.scrub = h.registryScrubber()
	t.scrubAt = time.Now()
	t.tel = telemetry.New(telemetry.Config{
		Component: "longarm-hub", Version: version.Version, Scrubber: t.scrub,
		Dir:      filepath.Join(h.cfg.DataDir, "telemetry"), // crash reports
		Identity: telemetry.Identity{CA: h.ca.Cert.Raw, Node: h.cfg.Name, Role: string(pki.RoleHub)},
		Sink:     t.fwd.Enqueue,
	})
	h.telem = t
	telemetry.SetDefault(t.tel) // the log handler's traces
	t.tel.ReportCrashes()
	stopCrashes := t.tel.WatchCrashes()
	run("telemetry", func() error { t.fwd.Run(ctx); stopCrashes(); return nil })
	run("telemetry ticks", func() error { h.telemetryLoop(ctx); return nil })
	h.log.Info("telemetry: forwarding to Application Insights", "endpoint", cs.Endpoint, "auth", auth)
}

// tel is the hub's own telemetry client (nil = off).
func (h *Hub) tel() *telemetry.Client {
	if h.telem == nil {
		return nil
	}
	return h.telem.tel
}

// event records an event of the hub.
func (h *Hub) event(ctx context.Context, name string, props map[string]string) {
	h.tel().Event(ctx, name, props, nil)
}

// registryScrubber knows the names of every node, host and user the hub
// has seen, keyed by the fleet salt (the nodes hash the same way).
func (h *Hub) registryScrubber() *pii.Scrubber {
	names := map[string]string{h.cfg.Name: pii.KindNode}
	h.mu.Lock()
	for _, n := range h.nodes {
		names[n.rec.Name] = pii.KindNode
		names[n.rec.DialName()] = pii.KindNode
		if n.rec.Hostname != "" {
			names[n.rec.Hostname] = pii.KindHost
			if i := strings.IndexByte(n.rec.Hostname, '.'); i > 0 {
				names[n.rec.Hostname[:i]] = pii.KindHost
			}
		}
		if n.rec.User != "" {
			names[n.rec.User] = pii.KindUser
		}
	}
	for u := range h.tenants {
		names[u] = pii.KindUser
	}
	h.mu.Unlock()
	if users, err := h.store.Users(); err == nil {
		for _, u := range users {
			names[u] = pii.KindUser
		}
	}
	for _, host := range h.cfg.Hosts {
		names[host] = pii.KindHost
	}
	return pii.New(pii.Options{Salt: pii.FleetSalt(h.ca.Cert.Raw), Names: names})
}

// scrubber returns the registry scrubber, rebuilt when nodes were added
// or every few minutes.
func (t *hubTelemetry) scrubber(h *Hub) *pii.Scrubber {
	h.mu.Lock()
	n := len(h.nodes) + len(h.tenants)
	h.mu.Unlock()
	t.mu.Lock()
	stale := n != t.scrubN || time.Since(t.scrubAt) > telScrubberEvery
	s := t.scrub
	t.mu.Unlock()
	if !stale {
		return s
	}
	s = h.registryScrubber()
	t.mu.Lock()
	t.scrub, t.scrubAt, t.scrubN = s, time.Now(), n
	t.mu.Unlock()
	t.tel.SetScrubber(s)
	return s
}

// allow takes n items from node's budget.
func (t *hubTelemetry) allow(node string, n int) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.limits[node]
	if l == nil {
		if len(t.limits) > 10000 {
			t.limits = map[string]*rate.Limiter{}
		}
		l = rate.NewLimiter(rate.Limit(float64(telItemsPerMin)/60), telItemsBurst)
		t.limits[node] = l
	}
	now := time.Now()
	ok := int(l.TokensAt(now))
	if ok > n {
		ok = n
	}
	if ok > 0 {
		l.AllowN(now, ok)
	}
	return ok
}

// telemetryLoop sends the hub's per-minute aggregates and relay counters.
func (h *Hub) telemetryLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	var last relay.Stats
	h.event(ctx, "process.start", map[string]string{"relay": boolStr(h.relay != nil), "https": boolStr(h.cfg.HTTPS != ""),
		"tailnet": boolStr(h.tailnet != nil)})
	for {
		select {
		case <-ctx.Done():
			h.tel().Event(context.Background(), "process.stop", nil, map[string]float64{"uptime_s": time.Since(h.started).Seconds()})
			h.tel().Tick()
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			h.telem.fwd.Flush(fctx)
			cancel()
			return
		case <-t.C:
		}
		tel := h.tel()
		if h.relay != nil {
			st := h.relay.Stats()
			tel.Metric("relay.sessions", float64(len(st.Sessions)), nil)
			tel.Metric("relay.sessions_new", float64(st.SessionsTotal-last.SessionsTotal), nil)
			tel.Metric("relay.bytes", float64(st.BytesRelayed-last.BytesRelayed), nil)
			drops := (st.DropNoPeer + st.DropRateLimit + st.DropQueueFull + st.DropPolicy) -
				(last.DropNoPeer + last.DropRateLimit + last.DropQueueFull + last.DropPolicy)
			if drops > 0 {
				tel.Metric("relay.drops", float64(drops), nil)
			}
			if d := st.HandshakeFails - last.HandshakeFails; d > 0 {
				tel.Metric("relay.handshake_fails", float64(d), nil)
			}
			last = st
		}
		h.mu.Lock()
		online, queued, links := 0, len(h.queue), len(h.portLinks)
		for _, n := range h.nodes {
			if n.sess != nil {
				online++
			}
		}
		h.mu.Unlock()
		tel.Metric("nodes.online", float64(online), nil)
		tel.Metric("scheduler.queue", float64(queued), nil)
		tel.Metric("links.active", float64(links), nil)
		fs := h.telem.fwd.Stats()
		if fs.Dropped > 0 || fs.Failed > 0 {
			tel.Metric("telemetry.forward_failed", float64(fs.Failed+fs.Dropped), nil)
		}
		tel.Tick()
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// telemetryServer is the Telemetry service: nodes push their queues.
type telemetryServer struct {
	pb.UnimplementedTelemetryServer
	h *Hub
}

func (s *telemetryServer) Push(ctx context.Context, b *pb.TelemetryBatch) (*pb.TelemetryAck, error) {
	h := s.h
	t := h.telem
	if t == nil {
		return &pb.TelemetryAck{Collecting: false, RetryAfterSeconds: int64(telRetryOff / time.Second)}, nil
	}
	ack := &pb.TelemetryAck{Collecting: true, MaxBatch: telMaxBatch}
	items := b.Items
	if len(items) > telMaxBatch {
		ack.Dropped += int32(len(items) - telMaxBatch)
		items = items[:telMaxBatch]
	}
	if len(items) == 0 {
		return ack, nil
	}
	c := callerFrom(ctx)
	n := t.allow(c.nodeID, len(items))
	ack.Dropped += int32(len(items) - n)
	items = items[:n]
	scr := t.scrubber(h)
	node := scr.Hash(pii.KindNode, c.id.Name)
	user := ""
	if c.id.Role != pki.RoleWorker {
		user = scr.Hash(pii.KindUser, c.user)
	}
	out := make([]telemetry.Item, 0, len(items))
	for _, p := range items {
		it := tgrpc.FromPB(p)
		switch it.Kind {
		case telemetry.KindEvent, telemetry.KindException, telemetry.KindRequest, telemetry.KindDependency, telemetry.KindTrace, telemetry.KindMetric:
		default:
			ack.Dropped++
			continue
		}
		switch it.Context.Component {
		case "longarm", "longarmd", "longarm-tray":
		default:
			it.Context.Component = "unknown"
		}
		// the caller's certificate says who sent it, not the item
		it.Context.Fleet, it.Context.Node, it.Context.User, it.Context.Role = t.fleet, node, user, string(c.id.Role)
		if it.Time.IsZero() || it.Time.After(time.Now().Add(time.Hour)) {
			it.Time = time.Now()
		}
		telemetry.Prepare(&it, scr)
		out = append(out, it)
	}
	t.fwd.Enqueue(out)
	if ack.Dropped > 0 {
		t.tel.Count("telemetry.hub_dropped", map[string]string{"role": string(c.id.Role)}, float64(ack.Dropped))
	}
	return ack, nil
}
