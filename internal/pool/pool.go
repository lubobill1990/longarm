// Package pool is longarm daemon's live view of the worker pool (design §11.3): for every known worker it
// keeps a persistent connection per advertised leg (tcp / longnet /
// tailscale endpoint), probes each leg's round trip every 2 s, tracks the
// path kind (direct, relay, DERP ...) and its changes, and keeps ~5 min of
// history. Metrics come from the workers directly first (a metrics stream
// on the active leg of each worker, up to MaxDirect workers); the hub's
// event stream only fills in for the workers the monitor can't reach or
// doesn't stream from, so the view is the same with the hub down. Without a
// viewer for IdleAfter (the tray closed) the monitor idles: no direct
// streams, probes every IdleInterval.
package pool

import (
	"context"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/transport"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// HistoryLen is ~5 min of samples at the 2 s interval.
const HistoryLen = 150

// Prober is a persistent connection to one leg of a worker.
type Prober interface {
	// Probe measures one round trip and reports the current path.
	Probe(ctx context.Context) (time.Duration, transport.PathInfo, error)
	// Metrics streams the worker's metrics until ctx ends or it fails.
	Metrics(ctx context.Context, interval time.Duration, fn func(*pb.MetricsSnapshot)) error
	Close()
}

// Target is a worker to monitor.
type Target struct {
	Name string
	Legs []client.Worker // best first (client.Legs)
}

// Config of a Monitor.
type Config struct {
	Interval     time.Duration // probe / metrics interval (default 2 s)
	ProbeTimeout time.Duration // default 5 s
	HubStale     time.Duration // metrics older than this are stale (default 3 intervals)
	// MaxDirect caps the workers with a direct metrics stream (default 32;
	// < 0 = none): those with this client's jobs first, then the fastest.
	// The others get the hub's metrics.
	MaxDirect int
	// IdleAfter: without a viewer (Touch) for this long the monitor idles -
	// no direct metrics streams, probes only every IdleInterval (defaults
	// 5 min and 30 s; IdleAfter < 0 = never idle).
	IdleAfter    time.Duration
	IdleInterval time.Duration
	Refresh      time.Duration // re-read Targets (default 30 s)
	// Targets lists the workers to monitor; complete = the list is
	// authoritative (the hub answered), so workers missing from it are
	// dropped even if the hub reported them before.
	Targets func(ctx context.Context) (targets []Target, complete bool)
	Dial    func(leg client.Worker) (Prober, error)
	Policy  func() string // transport policy (client.Policies); nil = race
	// Sticky returns the endpoint key of the leg that won the last race to
	// a worker (client.Dialer.StickyLeg), "" = none.
	Sticky func(worker string) string
	Now    func() time.Time
}

// Monitor keeps the pool view.
type Monitor struct {
	cfg Config

	mu       sync.Mutex
	workers  map[string]*worker
	hubUp    bool
	lastView time.Time // last Touch
	myJobs   map[string]int32
	changed  chan struct{} // closed and replaced on every update
}

type worker struct {
	name     string
	legs     []*leg
	state    string
	since    time.Time
	node     *pb.NodeInfo
	metrics  *pb.MetricsSnapshot
	source   string              // of metrics: hub | direct
	hubSnap  *pb.MetricsSnapshot // the hub's latest (fills in when direct isn't fresh)
	hubAt    time.Time
	directAt time.Time
	hist     []*pb.MetricsPoint
	direct   context.CancelFunc // running direct metrics stream
	err      string
}

type leg struct {
	w        client.Worker
	label    string
	prober   Prober
	dialErr  string
	inflight bool
	st       pb.LegStatus
	hist     []*pb.LinkSample
	probed   bool
	lastAt   time.Time // last probe start
}

// New returns a monitor; Run drives it.
func New(cfg Config) *Monitor {
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 5 * time.Second
	}
	if cfg.HubStale <= 0 {
		cfg.HubStale = 3 * cfg.Interval
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = 30 * time.Second
	}
	if cfg.MaxDirect == 0 {
		cfg.MaxDirect = 32
	}
	if cfg.IdleAfter == 0 {
		cfg.IdleAfter = 5 * time.Minute
	}
	if cfg.IdleInterval <= 0 {
		cfg.IdleInterval = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Monitor{cfg: cfg, workers: map[string]*worker{}, changed: make(chan struct{}), lastView: cfg.Now()}
}

func (m *Monitor) now() time.Time { return m.cfg.Now() }

func (m *Monitor) policy() string {
	if m.cfg.Policy == nil {
		return client.PolicyRace
	}
	return m.cfg.Policy()
}

// notifyLocked wakes WatchPool streams.
func (m *Monitor) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// Changed returns a channel closed at the next update.
func (m *Monitor) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

// Run probes until ctx is done, then closes every connection.
func (m *Monitor) Run(ctx context.Context) {
	m.refresh(ctx)
	tick := time.NewTicker(m.cfg.Interval)
	defer tick.Stop()
	lastRefresh := time.Now()
	for {
		m.tick(ctx)
		select {
		case <-ctx.Done():
			m.mu.Lock()
			for _, w := range m.workers {
				m.dropLocked(w)
			}
			m.workers = map[string]*worker{}
			m.mu.Unlock()
			return
		case <-tick.C:
		}
		if time.Since(lastRefresh) >= m.cfg.Refresh {
			lastRefresh = time.Now()
			m.refresh(ctx)
		}
	}
}

// Refresh re-reads the targets now (e.g. after the transport policy changed).
func (m *Monitor) Refresh(ctx context.Context) { m.refresh(ctx) }

func (m *Monitor) refresh(ctx context.Context) {
	if m.cfg.Targets == nil {
		return
	}
	targets, complete := m.cfg.Targets(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	for _, t := range targets {
		seen[t.Name] = true
		w := m.workers[t.Name]
		if w == nil {
			w = &worker{name: t.Name, state: "connecting", since: m.now()}
			m.workers[t.Name] = w
		}
		// Keep legs whose endpoint is unchanged; close the others.
		var legs []*leg
		for _, tl := range t.Legs {
			var keep *leg
			for _, old := range w.legs {
				if old.w.Transport == tl.Transport && old.w.Address == tl.Address && old.w.CertName == tl.CertName {
					keep = old
					break
				}
			}
			if keep == nil {
				keep = m.newLeg(tl)
			} else if keep.prober == nil { // its dial failed before: retry
				fresh := m.newLeg(tl)
				keep.prober, keep.dialErr, keep.st.Error = fresh.prober, fresh.dialErr, fresh.st.Error
			}
			legs = append(legs, keep)
		}
		for _, old := range w.legs {
			found := false
			for _, l := range legs {
				found = found || l == old
			}
			if !found && old.prober != nil {
				old.prober.Close()
			}
		}
		w.legs = legs
	}
	for name, w := range m.workers {
		if !seen[name] && (complete || w.node == nil) {
			m.dropLocked(w)
			delete(m.workers, name)
		}
	}
	m.notifyLocked()
}

// newLeg opens a leg's connection (lazily connecting: cheap).
func (m *Monitor) newLeg(tl client.Worker) *leg {
	l := &leg{w: tl, label: tl.Transport}
	l.st = pb.LegStatus{Transport: tl.Transport, Endpoint: tl.Transport + "://" + tl.Address, Label: tl.Transport}
	p, err := m.cfg.Dial(tl)
	if err != nil {
		l.dialErr = err.Error()
		l.st.Error = l.dialErr
	}
	l.prober = p
	return l
}

func (m *Monitor) dropLocked(w *worker) {
	if w.direct != nil {
		w.direct()
		w.direct = nil
	}
	for _, l := range w.legs {
		if l.prober != nil {
			l.prober.Close()
		}
	}
	w.legs = nil
}

// Touch records a viewer (the tray's pool view, longarm ls): the monitor
// leaves its idle mode.
func (m *Monitor) Touch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastView = m.now()
}

// idleLocked: no viewer for IdleAfter.
func (m *Monitor) idleLocked(now time.Time) bool {
	return m.cfg.IdleAfter > 0 && now.Sub(m.lastView) >= m.cfg.IdleAfter
}

// directRankLocked orders the workers for the MaxDirect cap: those with
// this client's jobs, then by the active leg's round trip, then by name.
func (m *Monitor) directRankLocked() []*worker {
	type rw struct {
		w   *worker
		rtt float64
	}
	var ws []rw
	for _, w := range m.workers {
		l := m.activeLegLocked(w)
		if l == nil || l.prober == nil || !l.st.Up {
			continue
		}
		ws = append(ws, rw{w, l.st.RttMs})
	}
	sort.Slice(ws, func(i, j int) bool {
		a, b := m.myJobs[ws[i].w.name] > 0, m.myJobs[ws[j].w.name] > 0
		if a != b {
			return a
		}
		if ws[i].rtt != ws[j].rtt {
			return ws[i].rtt < ws[j].rtt
		}
		return ws[i].w.name < ws[j].w.name
	})
	out := make([]*worker, len(ws))
	for i, x := range ws {
		out[i] = x.w
	}
	return out
}

// tick starts the probes that are due and the direct metrics streams that
// are needed (direct first; the hub fills in).
func (m *Monitor) tick(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	idle := m.idleLocked(now)
	want := map[*worker]bool{}
	if !idle && m.cfg.MaxDirect > 0 {
		for i, w := range m.directRankLocked() {
			if i >= m.cfg.MaxDirect {
				break
			}
			want[w] = true
		}
	}
	for _, w := range m.workers {
		for _, l := range w.legs {
			if l.prober == nil || l.inflight || (idle && now.Sub(l.lastAt) < m.cfg.IdleInterval) {
				continue
			}
			l.inflight, l.lastAt = true, now
			go m.probe(ctx, w.name, l)
		}
		switch {
		case !want[w] && w.direct != nil:
			w.direct()
			w.direct, w.directAt = nil, time.Time{} // no longer current: the hub fills in
		case want[w] && w.direct == nil:
			if l := m.activeLegLocked(w); l != nil && l.prober != nil && l.st.Up {
				dctx, cancel := context.WithCancel(ctx)
				w.direct = cancel
				go m.streamDirect(dctx, w, l.prober)
			}
		}
		m.hubFillLocked(w) // a stalled or missing direct stream: the hub's, if any
		m.updateStateLocked(w, now)
	}
}

// directFreshLocked: the worker's direct metrics are current.
func (m *Monitor) directFreshLocked(w *worker, now time.Time) bool {
	return w.source == "direct" && !w.directAt.IsZero() && now.Sub(w.directAt) < m.cfg.HubStale
}

// hubFillLocked shows the hub's latest metrics when the direct ones aren't
// current (no direct stream, or it stalled).
func (m *Monitor) hubFillLocked(w *worker) {
	if w.hubSnap == nil || m.directFreshLocked(w, m.now()) {
		return
	}
	if w.metrics == nil || w.source != "hub" || !proto.Equal(w.hubSnap.Time, w.metrics.Time) {
		m.setMetricsLocked(w, w.hubSnap, "hub")
	}
}

func (m *Monitor) probe(ctx context.Context, name string, l *leg) {
	pctx, cancel := context.WithTimeout(ctx, m.cfg.ProbeTimeout)
	rtt, pi, err := l.prober.Probe(pctx)
	cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	l.inflight = false
	if ctx.Err() != nil {
		return
	}
	m.observeLocked(l, m.now(), rtt, pi, err)
	if w := m.workers[name]; w != nil {
		m.updateStateLocked(w, m.now())
	}
	m.notifyLocked()
}

// observeLocked records a probe result on a leg.
func (m *Monitor) observeLocked(l *leg, now time.Time, rtt time.Duration, pi transport.PathInfo, err error) {
	st := &l.st
	wasUp, oldKind := st.Up, st.Kind
	sample := &pb.LinkSample{Time: timestamppb.New(now), RttMs: -1}
	if err != nil {
		st.Up, st.Error = false, oneLine(err.Error())
	} else {
		st.Up, st.Error = true, ""
		st.RttMs = float64(rtt.Microseconds()) / 1000
		sample.RttMs = st.RttMs
		if pi.Kind != "" {
			st.Kind, st.Detail = string(pi.Kind), pi.Detail
			st.Nat = client.NATOf(pi.Detail)
			if lb := client.LegLabel(pi); lb != "" {
				st.Label = lb
			}
		}
	}
	if !l.probed || wasUp != st.Up || oldKind != st.Kind {
		st.Since = timestamppb.New(now)
	}
	relayed := st.Up && st.Kind != "" && st.Kind != string(transport.PathDirect) && st.Kind != string(transport.PathUnknown)
	switch {
	case relayed && st.RelaySince == nil:
		st.RelaySince = timestamppb.New(now)
	case !relayed && st.Up:
		st.RelaySince = nil
	}
	l.probed = true
	l.hist = append(l.hist, sample)
	if len(l.hist) > HistoryLen {
		l.hist = append(l.hist[:0:0], l.hist[len(l.hist)-HistoryLen:]...)
	}
}

func (m *Monitor) streamDirect(ctx context.Context, w *worker, p Prober) {
	err := p.Metrics(ctx, m.cfg.Interval, func(s *pb.MetricsSnapshot) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		m.setMetricsLocked(w, s, "direct")
		w.directAt = m.now()
		m.notifyLocked()
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() == nil && err != nil {
		w.err = "metrics: " + oneLine(err.Error())
	}
	if ctx.Err() == nil && w.direct != nil { // ended by itself: allow a restart
		w.direct()
		w.direct = nil
	}
}

func (m *Monitor) setMetricsLocked(w *worker, s *pb.MetricsSnapshot, source string) {
	w.metrics, w.source = s, source
	p := &pb.MetricsPoint{Time: s.Time, Cpu: s.CpuPercent}
	if p.Time == nil {
		p.Time = timestamppb.New(m.now())
	}
	if s.MemTotalBytes > 0 {
		p.Mem = 100 * float64(s.MemUsedBytes) / float64(s.MemTotalBytes)
	}
	w.hist = append(w.hist, p)
	if len(w.hist) > HistoryLen {
		w.hist = append(w.hist[:0:0], w.hist[len(w.hist)-HistoryLen:]...)
	}
}

// updateStateLocked derives online/connecting/offline from the legs (this
// client's view; the hub's view only when we have no leg at all).
func (m *Monitor) updateStateLocked(w *worker, now time.Time) {
	state, probed, errMsg := "connecting", false, ""
	for _, l := range w.legs {
		if l.st.Up {
			state = "online"
			break
		}
		if l.probed || l.dialErr != "" {
			probed = true
			if errMsg == "" {
				errMsg = l.st.Error
			}
		}
	}
	if state != "online" && probed {
		state = "offline"
	}
	if len(w.legs) == 0 && w.node != nil {
		state = map[bool]string{true: "online", false: "offline"}[w.node.Online]
	}
	if state != w.state {
		w.state, w.since = state, now
	}
	if state == "online" {
		errMsg = ""
	}
	if errMsg != "" || state == "online" {
		w.err = errMsg
	}
}

// activeLegLocked is the leg longarm would use under the transport policy (the
// dialer in internal/client): with race, the sticky winner of the last
// race while it is up and no other leg clearly beats it (client.BetterLeg),
// else the best leg that is up; with a preferred
// transport, its first leg that is up, else the fastest one.
func (m *Monitor) activeLegLocked(w *worker) *leg {
	if len(w.legs) == 0 {
		return nil
	}
	fastest := func(ok func(*leg) bool) *leg {
		var best *leg
		for _, l := range w.legs {
			if l.st.Up && ok(l) && (best == nil || l.st.RttMs < best.st.RttMs) {
				best = l
			}
		}
		return best
	}
	anyLeg := func(*leg) bool { return true }
	live := func(l *leg) client.LiveLeg {
		return client.LiveLeg{Leg: legKey(l.w), Up: l.st.Up, Kind: l.st.Kind, RTTMs: l.st.RttMs}
	}
	// best by the dialer's rule (client.BetterLeg): a better path kind
	// unless clearly slower, else clearly faster.
	best := func(ok func(*leg) bool) *leg {
		var b *leg
		for _, l := range w.legs {
			if l.st.Up && ok(l) && (b == nil || client.BetterLeg(live(l), live(b))) {
				b = l
			}
		}
		return b
	}
	if p := m.policy(); p != client.PolicyRace {
		for _, l := range w.legs {
			if l.st.Up && l.w.Transport == p {
				return l
			}
		}
		if f := fastest(anyLeg); f != nil {
			return f
		}
		return w.legs[0]
	}
	if m.cfg.Sticky != nil {
		if key := m.cfg.Sticky(w.name); key != "" {
			if l := fastest(func(l *leg) bool { return legKey(l.w) == key }); l != nil &&
				// the dialer drops a sticky leg another live leg clearly beats
				best(func(o *leg) bool { return o != l && client.BetterLeg(live(o), live(l)) }) == nil {
				return l
			}
		}
	}
	if b := best(anyLeg); b != nil {
		return b
	}
	return w.legs[0]
}

// LiveLegs is the current state of a worker's legs for the dialer's race
// (client.Dialer.LiveLegs); legs not probed yet, or whose last probe is
// older than maxAge, are left out.
func (m *Monitor) LiveLegs(worker string, maxAge time.Duration) []client.LiveLeg {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.workers[worker]
	if w == nil {
		return nil
	}
	now := m.now()
	var out []client.LiveLeg
	for _, l := range w.legs {
		if !l.probed || len(l.hist) == 0 || now.Sub(l.hist[len(l.hist)-1].Time.AsTime()) > maxAge {
			continue
		}
		ll := client.LiveLeg{Leg: legKey(l.w), Up: l.st.Up}
		if l.st.Up {
			ll.Kind, ll.RTTMs = l.st.Kind, l.st.RttMs
		}
		out = append(out, ll)
	}
	return out
}

func legKey(w client.Worker) string {
	return client.Endpoint{Transport: w.Transport, Address: w.Address}.Key()
}

// HubNode feeds a node / metrics event (or a ListNodes entry) from the hub.
func (m *Monitor) HubNode(n *pb.NodeInfo) {
	if n == nil || n.Role != "" && n.Role != "worker" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.workers[n.Name]
	if w == nil {
		if n.State != "approved" {
			return
		}
		w = &worker{name: n.Name, state: "connecting", since: m.now()}
		m.workers[n.Name] = w
	}
	if n.State == "revoked" {
		m.dropLocked(w)
		delete(m.workers, n.Name)
		m.notifyLocked()
		return
	}
	w.node = proto.Clone(n).(*pb.NodeInfo)
	w.node.Metrics = nil
	if len(w.legs) == 0 && len(n.Endpoints) > 0 && m.cfg.Dial != nil && n.State == "approved" {
		// a worker that appeared since the last refresh: probe it now
		for _, tl := range client.NodeLegs(n.Name, client.ReachableEndpoints(n.Endpoints, n.Hostname)) {
			w.legs = append(w.legs, m.newLeg(tl))
		}
	}
	if n.Online && n.Metrics != nil && (w.hubSnap == nil || !proto.Equal(n.Metrics.Time, w.hubSnap.Time)) {
		w.hubSnap, w.hubAt = n.Metrics, m.now()
		m.hubFillLocked(w) // direct metrics win while they are current
	}
	m.updateStateLocked(w, m.now())
	m.notifyLocked()
}

// SetHubUp records whether the hub's event stream is live.
func (m *Monitor) SetHubUp(up bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hubUp != up {
		m.hubUp = up
		m.notifyLocked()
	}
}

// SetMyJobs records this client's active jobs per worker.
func (m *Monitor) SetMyJobs(counts map[string]int32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.myJobs = counts
	m.notifyLocked()
}

// Workers lists the monitored workers.
func (m *Monitor) Workers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.workers))
	for n := range m.workers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// View is the pool view (PoolView without the hub link / identity, which
// the daemon fills in).
func (m *Monitor) View(history bool) *pb.PoolView {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	v := &pb.PoolView{Time: timestamppb.New(now), Transport: m.policy()}
	hub, direct := 0, 0
	for _, w := range m.workers {
		pw := &pb.PoolWorker{Name: w.name, State: w.state, Since: timestamppb.New(w.since), Node: w.node,
			Error: w.err, MyJobs: m.myJobs[w.name], Link: m.linkLocked(w, history)}
		fresh := w.metrics != nil && ((w.source == "hub" && m.hubUp && now.Sub(w.hubAt) < m.cfg.HubStale) ||
			(w.source == "direct" && now.Sub(w.directAt) < m.cfg.HubStale))
		switch {
		case fresh:
			pw.Source, pw.Metrics = w.source, w.metrics
		case w.metrics != nil:
			pw.Source, pw.Metrics = "none", w.metrics // stale: last known
		default:
			pw.Source = "none"
		}
		if pw.Source == "hub" {
			hub++
		} else if pw.Source == "direct" {
			direct++
		}
		if history {
			pw.History = append([]*pb.MetricsPoint(nil), w.hist...)
		}
		v.Workers = append(v.Workers, pw)
	}
	sort.Slice(v.Workers, func(i, j int) bool { return v.Workers[i].Name < v.Workers[j].Name })
	switch {
	case hub > 0 && direct == 0:
		v.Source = "hub"
	case direct > 0 && hub == 0:
		v.Source = "direct"
	case hub > 0:
		v.Source = "mixed"
	default:
		v.Source = "none"
	}
	return v
}

func (m *Monitor) linkLocked(w *worker, history bool) *pb.WorkerLink {
	wl := &pb.WorkerLink{Worker: w.name, State: w.state, Since: timestamppb.New(w.since)}
	active := m.activeLegLocked(w)
	for _, l := range w.legs {
		st := proto.Clone(&l.st).(*pb.LegStatus)
		st.Active = l == active
		if history {
			st.History = append([]*pb.LinkSample(nil), l.hist...)
		}
		if st.Active {
			wl.Active, wl.RttMs = st.Label, st.RttMs
			if !st.Up {
				wl.RttMs = 0
			}
		}
		wl.Legs = append(wl.Legs, st)
	}
	return wl
}

// LinkReport is what the daemon reports to the hub (no history).
func (m *Monitor) LinkReport() *pb.LinkReport {
	v := m.View(false)
	r := &pb.LinkReport{Time: v.Time}
	for _, w := range v.Workers {
		if w.Link != nil && len(w.Link.Legs) > 0 {
			r.Links = append(r.Links, w.Link)
		}
	}
	return r
}

func oneLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			s = s[:i]
			break
		}
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
