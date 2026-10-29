package pool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/transport"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type fakeProber struct {
	mu      sync.Mutex
	rtt     time.Duration
	path    transport.PathInfo
	err     error
	metrics int // streams started
	closed  bool
	snap    *pb.MetricsSnapshot
}

func (f *fakeProber) Probe(ctx context.Context) (time.Duration, transport.PathInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rtt, f.path, f.err
}

func (f *fakeProber) Metrics(ctx context.Context, interval time.Duration, fn func(*pb.MetricsSnapshot)) error {
	f.mu.Lock()
	f.metrics++
	f.mu.Unlock()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		f.mu.Lock()
		s := f.snap
		f.mu.Unlock()
		if s != nil {
			fn(s)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (f *fakeProber) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

func (f *fakeProber) set(rtt time.Duration, kind transport.PathKind, detail string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rtt, f.err = rtt, err
	f.path = transport.PathInfo{Transport: f.path.Transport, Kind: kind, Detail: detail}
}

func waitView(t *testing.T, m *Monitor, what string, cond func(v *pb.PoolView) bool) *pb.PoolView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		v := m.View(true)
		if cond(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s; last view %v", what, v)
		}
		select {
		case <-m.Changed():
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestMonitorLegsPathsAndMetricsSource(t *testing.T) {
	tcp := &fakeProber{path: transport.PathInfo{Transport: "tcp"}}
	longarm := &fakeProber{path: transport.PathInfo{Transport: "longnet"}}
	tcp.set(3*time.Millisecond, transport.PathDirect, "10.0.0.2:7701", nil)
	longarm.set(40*time.Millisecond, transport.PathRelay, "udp relay.example:7443, nat easy/hard", nil)
	longarm.snap = &pb.MetricsSnapshot{CpuPercent: 12, MemTotalBytes: 100, MemUsedBytes: 95, Time: timestamppb.Now()}
	tcp.snap = longarm.snap
	var policy = client.PolicyRace
	var sticky string
	var bmu sync.Mutex
	m := New(Config{
		Interval: 20 * time.Millisecond, HubStale: 200 * time.Millisecond,
		Targets: func(context.Context) ([]Target, bool) {
			return []Target{{Name: "w1", Legs: client.NodeLegs("w1", []string{"longnet://relay:7443", "tcp://10.0.0.2:7701"})}}, false
		},
		Dial: func(l client.Worker) (Prober, error) {
			if l.Transport == "tcp" {
				return tcp, nil
			}
			return longarm, nil
		},
		Policy: func() string { bmu.Lock(); defer bmu.Unlock(); return policy },
		Sticky: func(string) string { bmu.Lock(); defer bmu.Unlock(); return sticky },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	v := waitView(t, m, "online with both legs", func(v *pb.PoolView) bool {
		return len(v.Workers) == 1 && v.Workers[0].State == "online" && len(v.Workers[0].Link.Legs) == 2 &&
			v.Workers[0].Link.Legs[0].Up && v.Workers[0].Link.Legs[1].Up
	})
	l := v.Workers[0].Link
	if l.Legs[0].Transport != "tcp" || !l.Legs[0].Active || l.Active != "tcp" || l.RttMs != 3 {
		t.Fatalf("race: the faster tcp leg first and active: %v", l)
	}
	if rl := l.Legs[1]; rl.Label != "longnet relay-udp" || rl.Nat != "easy/hard" || rl.RelaySince == nil || rl.Kind != "relay" {
		t.Fatalf("longnet leg: %v", rl)
	}
	// No hub: metrics stream directly from the active leg.
	v = waitView(t, m, "direct metrics", func(v *pb.PoolView) bool { return v.Source == "direct" && v.Workers[0].Metrics != nil })
	if len(v.Workers[0].History) == 0 || v.Workers[0].History[0].Mem != 95 {
		t.Fatalf("metrics history: %v", v.Workers[0].History)
	}

	// Direct first: the hub's events don't replace current direct metrics
	// (they fill in only for workers without them, TestHubFillsIn).
	m.SetHubUp(true)
	m.HubNode(&pb.NodeInfo{Name: "w1", Role: "worker", State: "approved", Online: true,
		Metrics: &pb.MetricsSnapshot{CpuPercent: 50, MemTotalBytes: 100, MemUsedBytes: 10, Time: timestamppb.Now()}})
	time.Sleep(60 * time.Millisecond)
	v = waitView(t, m, "still direct", func(v *pb.PoolView) bool { return v.Source == "direct" })
	if v.Workers[0].Metrics.CpuPercent != 12 || v.Workers[0].Node == nil {
		t.Fatalf("direct metrics with the hub up: %v", v.Workers[0])
	}

	// Race: the clearly fastest leg that is up is active (>20 % and >5 ms
	// faster, or a better path kind); the longnet leg moving to a direct path
	// clears relay_since.
	tcp.set(10*time.Millisecond, transport.PathDirect, "10.0.0.2:7701", nil)
	longarm.set(1*time.Millisecond, transport.PathDirect, "1.2.3.4:5000, nat easy/easy", nil)
	v = waitView(t, m, "race picks longnet", func(v *pb.PoolView) bool { return v.Workers[0].Link.Active == "longnet direct" })
	if v.Workers[0].Link.Legs[1].RelaySince != nil || v.Transport != "race" {
		t.Fatalf("after direct: %v", v.Workers[0].Link.Legs[1])
	}
	// The dialer's sticky winner stays active while it is up, even if a
	// bit slower.
	tcp.set(3*time.Millisecond, transport.PathDirect, "10.0.0.2:7701", nil)
	bmu.Lock()
	sticky = "tcp://10.0.0.2:7701"
	bmu.Unlock()
	waitView(t, m, "sticky tcp", func(v *pb.PoolView) bool { return v.Workers[0].Link.Active == "tcp" })
	// ... unless another leg clearly beats it (the dialer drops it too).
	tcp.set(100*time.Millisecond, transport.PathDirect, "10.0.0.2:7701", nil)
	waitView(t, m, "degraded sticky tcp loses", func(v *pb.PoolView) bool { return v.Workers[0].Link.Active == "longnet direct" })
	live := m.LiveLegs("w1", time.Minute)
	if len(live) != 2 || live[0] != (client.LiveLeg{Leg: "tcp://10.0.0.2:7701", Up: true, Kind: "direct", RTTMs: 100}) ||
		live[1] != (client.LiveLeg{Leg: "longnet://relay:7443", Up: true, Kind: "direct", RTTMs: 1}) {
		t.Fatalf("LiveLegs: %+v", live)
	}
	if m.LiveLegs("nope", time.Minute) != nil || len(m.LiveLegs("w1", -time.Second)) != 0 {
		t.Fatal("LiveLegs of an unknown worker / stale samples")
	}
	tcp.set(3*time.Millisecond, transport.PathDirect, "10.0.0.2:7701", nil)
	waitView(t, m, "sticky tcp again", func(v *pb.PoolView) bool { return v.Workers[0].Link.Active == "tcp" })
	// A preferred transport wins over speed.
	bmu.Lock()
	sticky, policy = "", "tcp"
	bmu.Unlock()
	v = waitView(t, m, "prefer tcp", func(v *pb.PoolView) bool { return v.Workers[0].Link.Active == "tcp" })
	if v.Transport != "tcp" {
		t.Fatalf("view transport %q", v.Transport)
	}

	// All legs fail: offline, with the error; samples recorded as -1.
	tcp.set(0, "", "", errors.New("connection refused"))
	longarm.set(0, "", "", errors.New("relay down"))
	v = waitView(t, m, "offline", func(v *pb.PoolView) bool { return v.Workers[0].State == "offline" })
	if v.Workers[0].Error == "" {
		t.Fatalf("offline without error: %v", v.Workers[0])
	}
	h := v.Workers[0].Link.Legs[0].History
	if h[len(h)-1].RttMs != -1 {
		t.Fatalf("failed probe sample: %v", h[len(h)-1])
	}
	rep := m.LinkReport()
	if len(rep.Links) != 1 || len(rep.Links[0].Legs[0].History) != 0 {
		t.Fatalf("link report: %v", rep)
	}

	cancel()
	<-done
	if !tcp.closed || !longarm.closed {
		t.Fatal("probers not closed on stop")
	}
}

func TestMonitorHistoryBounded(t *testing.T) {
	m := New(Config{})
	l := &leg{}
	for i := 0; i < HistoryLen+50; i++ {
		m.observeLocked(l, time.Now(), time.Millisecond, transport.PathInfo{Transport: "tcp", Kind: transport.PathDirect}, nil)
	}
	if len(l.hist) != HistoryLen {
		t.Fatalf("history %d", len(l.hist))
	}
	w := &worker{}
	for i := 0; i < HistoryLen+5; i++ {
		m.setMetricsLocked(w, &pb.MetricsSnapshot{CpuPercent: float64(i)}, "hub")
	}
	if len(w.hist) != HistoryLen || w.hist[len(w.hist)-1].Cpu != float64(HistoryLen+4) {
		t.Fatalf("metrics history %d", len(w.hist))
	}
}

func TestRevokedWorkerDropped(t *testing.T) {
	p := &fakeProber{}
	m := New(Config{Interval: time.Hour, Dial: func(client.Worker) (Prober, error) { return p, nil },
		Targets: func(context.Context) ([]Target, bool) {
			return []Target{{Name: "w1", Legs: client.NodeLegs("w1", []string{"tcp://10.0.0.1:1"})}}, false
		}})
	m.refresh(context.Background())
	m.HubNode(&pb.NodeInfo{Name: "w1", Role: "worker", State: "revoked"})
	if len(m.View(false).Workers) != 0 || !p.closed {
		t.Fatal("revoked worker kept")
	}
}

// TestAuthoritativeTargets: a worker the hub reported once but that is no
// longer in an authoritative list (e.g. revoked while the stream was
// down) is dropped; a failed dial is retried on refresh.
func TestAuthoritativeTargets(t *testing.T) {
	p := &fakeProber{}
	fail := true
	complete := false
	targets := []Target{{Name: "w1", Legs: client.NodeLegs("w1", []string{"tcp://10.0.0.1:1"})}}
	m := New(Config{Interval: time.Hour,
		Dial: func(client.Worker) (Prober, error) {
			if fail {
				return nil, errors.New("no route")
			}
			return p, nil
		},
		Targets: func(context.Context) ([]Target, bool) { return targets, complete }})
	m.refresh(context.Background())
	if l := m.View(false).Workers[0].Link.Legs[0]; l.Error != "no route" {
		t.Fatalf("dial error not shown: %v", l)
	}
	fail = false
	m.refresh(context.Background())
	m.mu.Lock()
	got := m.workers["w1"].legs[0].prober
	m.mu.Unlock()
	if got != p {
		t.Fatal("failed dial not retried")
	}
	m.HubNode(&pb.NodeInfo{Name: "w2", Role: "worker", State: "approved", Endpoints: []string{"tcp://10.0.0.2:1"}})
	if len(m.View(false).Workers) != 2 {
		t.Fatal("hub-reported worker not added")
	}
	m.refresh(context.Background()) // not authoritative: w2 stays
	if len(m.View(false).Workers) != 2 {
		t.Fatal("w2 dropped by a non-authoritative list")
	}
	complete = true
	m.refresh(context.Background())
	if v := m.View(false); len(v.Workers) != 1 || v.Workers[0].Name != "w1" {
		t.Fatalf("authoritative list: %v", v.Workers)
	}
}

// TestHubFillsIn: workers beyond the direct cap (or with no reachable leg)
// show the hub's metrics; an idle monitor (no viewer) stops its direct
// streams and the hub fills in, a viewer brings them back.
func TestHubFillsIn(t *testing.T) {
	p := &fakeProber{path: transport.PathInfo{Transport: "tcp"}}
	p.set(2*time.Millisecond, transport.PathDirect, "10.0.0.2:7701", nil)
	p.snap = &pb.MetricsSnapshot{CpuPercent: 7, Time: timestamppb.Now()}
	var now struct {
		sync.Mutex
		t time.Time
	}
	now.t = time.Now()
	m := New(Config{
		Interval: 20 * time.Millisecond, HubStale: time.Hour, MaxDirect: 1, IdleAfter: time.Minute,
		Targets: func(context.Context) ([]Target, bool) {
			return []Target{{Name: "a", Legs: client.NodeLegs("a", []string{"tcp://10.0.0.2:7701"})},
				{Name: "b", Legs: client.NodeLegs("b", []string{"tcp://10.0.0.3:7701"})}}, true
		},
		Dial: func(l client.Worker) (Prober, error) {
			if l.Address == "10.0.0.3:7701" {
				q := &fakeProber{path: transport.PathInfo{Transport: "tcp"}}
				q.set(50*time.Millisecond, transport.PathDirect, "10.0.0.3:7701", nil)
				q.snap = &pb.MetricsSnapshot{CpuPercent: 99, Time: timestamppb.Now()}
				return q, nil
			}
			return p, nil
		},
		Now: func() time.Time { now.Lock(); defer now.Unlock(); return now.t },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	m.SetHubUp(true)
	hub := func(name string, cpu float64) {
		m.HubNode(&pb.NodeInfo{Name: name, Role: "worker", State: "approved", Online: true,
			Metrics: &pb.MetricsSnapshot{CpuPercent: cpu, Time: timestamppb.New(time.Now().Add(time.Duration(cpu) * time.Millisecond))}})
	}
	hub("a", 1)
	hub("b", 2)
	byName := func(v *pb.PoolView, n string) *pb.PoolWorker {
		for _, w := range v.Workers {
			if w.Name == n {
				return w
			}
		}
		return nil
	}
	// a (faster) streams directly, b (beyond MaxDirect 1) shows the hub's
	waitView(t, m, "a direct, b from the hub", func(v *pb.PoolView) bool {
		a, b := byName(v, "a"), byName(v, "b")
		return a != nil && b != nil && a.Source == "direct" && a.Metrics.GetCpuPercent() == 7 &&
			b.Source == "hub" && b.Metrics.GetCpuPercent() == 2 && v.Source == "mixed"
	})
	// no viewer for IdleAfter: the direct stream stops, the hub fills in
	now.Lock()
	now.t = now.t.Add(2 * time.Minute)
	now.Unlock()
	hub("a", 3)
	waitView(t, m, "idle: hub for a", func(v *pb.PoolView) bool {
		a := byName(v, "a")
		return a != nil && a.Source == "hub" && a.Metrics.GetCpuPercent() == 3
	})
	m.Touch()
	waitView(t, m, "a viewer: direct again", func(v *pb.PoolView) bool {
		a := byName(v, "a")
		return a != nil && a.Source == "direct" && a.Metrics.GetCpuPercent() == 7
	})
}
