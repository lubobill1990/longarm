// Package metrics turns raw platform samples into the worker snapshot shown
// by `longarm ls` (and later the hub / tray). The logic:
//
//   - machine CPU% from idle/total counter deltas (GetSystemTimes on Windows)
//   - memory from GlobalMemoryStatusEx (/proc/meminfo, sysctl elsewhere)
//   - per-process CPU% from CPU-time deltas, aggregated by process name,
//     top 5 by CPU and by memory
//   - agent sessions: Claude Code / Copilot CLI launchers nest (bg daemon ->
//     wrapper -> session, all the same exe), so one session = one leaf
//     process, i.e. a "claude"/"copilot" process with no same-name child;
//     memory is summed over all of that name's processes.
package metrics

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
)

// AgentNames are the agent CLIs whose sessions are counted.
var AgentNames = []string{"claude", "copilot"}

// TopN is the length of the top-CPU / top-memory lists.
const TopN = 5

// ProcGroup aggregates processes sharing a name.
type ProcGroup struct {
	Name       string
	Count      int
	MemBytes   uint64
	CPUPercent float64
}

// AgentStat counts sessions of one agent CLI.
type AgentStat struct {
	Sessions  int
	Processes int
	MemBytes  uint64
}

// Snapshot is one computed metrics sample.
type Snapshot struct {
	Time        time.Time
	CPUPercent  float64
	Threads     int
	MemTotal    uint64
	MemUsed     uint64
	JobsRunning int
	Uptime      time.Duration
	NProc       int
	Agents      map[string]AgentStat
	TopCPU      []ProcGroup
	TopMem      []ProcGroup
	DiskFree    uint64
	DiskTotal   uint64
}

// Sample is one raw reading.
type Sample struct {
	Wall  time.Time
	Sys   platform.SysSample
	Procs []platform.ProcSample
}

type procKey struct {
	pid   int
	start uint64
}

// Compute derives a snapshot from two consecutive samples. prev may be nil
// (then CPU figures are 0).
func Compute(prev, cur *Sample) Snapshot {
	s := Snapshot{
		Time:     cur.Wall,
		Threads:  cur.Sys.NumCPU,
		MemTotal: cur.Sys.MemTotal,
		Uptime:   cur.Sys.Uptime,
		NProc:    len(cur.Procs),
	}
	if cur.Sys.MemTotal >= cur.Sys.MemAvail {
		s.MemUsed = cur.Sys.MemTotal - cur.Sys.MemAvail
	}
	var prevCPU map[procKey]time.Duration
	var wall time.Duration
	if prev != nil {
		dIdle := float64(cur.Sys.IdleTicks) - float64(prev.Sys.IdleTicks)
		dTotal := float64(cur.Sys.TotalTicks) - float64(prev.Sys.TotalTicks)
		if dTotal > 0 {
			s.CPUPercent = round1(math.Max(0, 100*(dTotal-dIdle)/dTotal))
		}
		wall = cur.Wall.Sub(prev.Wall)
		prevCPU = make(map[procKey]time.Duration, len(prev.Procs))
		for _, p := range prev.Procs {
			prevCPU[procKey{p.PID, p.StartTime}] = p.CPUTime
		}
	}
	ncpu := cur.Sys.NumCPU
	if ncpu < 1 {
		ncpu = 1
	}
	agg := map[string]*ProcGroup{}
	for _, p := range cur.Procs {
		g := agg[p.Name]
		if g == nil {
			g = &ProcGroup{Name: p.Name}
			agg[p.Name] = g
		}
		g.Count++
		g.MemBytes += p.RSS
		if wall > 0 && p.CPUTime > 0 {
			if before, ok := prevCPU[procKey{p.PID, p.StartTime}]; ok && p.CPUTime >= before {
				g.CPUPercent += 100 * float64(p.CPUTime-before) / (float64(wall) * float64(ncpu))
			}
		}
	}
	groups := make([]ProcGroup, 0, len(agg))
	for _, g := range agg {
		g.CPUPercent = round1(g.CPUPercent)
		groups = append(groups, *g)
	}
	s.TopCPU = top(groups, func(a, b ProcGroup) bool { return a.CPUPercent > b.CPUPercent })
	s.TopMem = top(groups, func(a, b ProcGroup) bool { return a.MemBytes > b.MemBytes })
	s.Agents = CountAgents(cur.Procs, AgentNames)
	return s
}

func top(groups []ProcGroup, less func(a, b ProcGroup) bool) []ProcGroup {
	g := append([]ProcGroup(nil), groups...)
	sort.SliceStable(g, func(i, j int) bool {
		if less(g[i], g[j]) {
			return true
		}
		if less(g[j], g[i]) {
			return false
		}
		return g[i].Name < g[j].Name
	})
	if len(g) > TopN {
		g = g[:TopN]
	}
	return g
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// CountAgents applies the leaf rule per name (case-insensitive, like
// PowerShell's -in): sessions = processes of that name whose pid is not the
// parent of another process of the same name.
func CountAgents(procs []platform.ProcSample, names []string) map[string]AgentStat {
	out := make(map[string]AgentStat, len(names))
	for _, name := range names {
		var ps []platform.ProcSample
		parents := map[int]bool{}
		for _, p := range procs {
			if strings.EqualFold(p.Name, name) {
				ps = append(ps, p)
				parents[p.PPID] = true
			}
		}
		st := AgentStat{Processes: len(ps)}
		for _, p := range ps {
			st.MemBytes += p.RSS
			if !parents[p.PID] {
				st.Sessions++
			}
		}
		out[name] = st
	}
	return out
}

// TakeSample reads the platform counters.
func TakeSample() (*Sample, error) {
	sys, err := platform.SampleSystem()
	if err != nil {
		return nil, err
	}
	procs, err := platform.ListProcesses()
	if err != nil {
		return nil, err
	}
	return &Sample{Wall: time.Now(), Sys: sys, Procs: procs}, nil
}

// Sampler samples periodically and keeps the latest snapshot.
type Sampler struct {
	Interval time.Duration
	JobsFn   func() int // running job count (from the supervisor)
	DiskPath string     // volume to report free space for

	mu     sync.Mutex
	prev   *Sample
	latest *Snapshot
	subs   map[chan Snapshot]struct{}
}

func (s *Sampler) step() (*Snapshot, error) {
	cur, err := TakeSample()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	prev := s.prev
	s.prev = cur
	s.mu.Unlock()
	if prev == nil {
		return nil, nil
	}
	snap := Compute(prev, cur)
	if s.JobsFn != nil {
		snap.JobsRunning = s.JobsFn()
	}
	if s.DiskPath != "" {
		snap.DiskFree, snap.DiskTotal, _ = platform.DiskUsage(s.DiskPath)
	}
	s.mu.Lock()
	s.latest = &snap
	for ch := range s.subs {
		select {
		case ch <- snap:
		default: // slow subscriber: drop, it will get the next one
		}
	}
	s.mu.Unlock()
	return &snap, nil
}

// Run samples every Interval until ctx is done.
func (s *Sampler) Run(ctx context.Context) {
	if s.Interval <= 0 {
		s.Interval = 2 * time.Second
	}
	s.step()
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.step()
		}
	}
}

// Latest returns the most recent snapshot, sampling twice (500 ms apart) if
// none exists yet. JobsRunning is refreshed on every call.
func (s *Sampler) Latest(ctx context.Context) (Snapshot, error) {
	s.mu.Lock()
	l := s.latest
	s.mu.Unlock()
	if l == nil {
		if _, err := s.step(); err != nil {
			return Snapshot{}, err
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		}
		snap, err := s.step()
		if err != nil {
			return Snapshot{}, err
		}
		l = snap
	}
	out := *l
	if s.JobsFn != nil {
		out.JobsRunning = s.JobsFn()
	}
	return out, nil
}

// Subscribe returns a channel receiving every new snapshot and a cancel func.
func (s *Sampler) Subscribe() (<-chan Snapshot, func()) {
	ch := make(chan Snapshot, 1)
	s.mu.Lock()
	if s.subs == nil {
		s.subs = map[chan Snapshot]struct{}{}
	}
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}
