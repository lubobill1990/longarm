// Package sched is the placement algorithm of DESIGN §5.2, shared by longarm-hub
// (with reservations) and longarm's client-side fallback when the hub is
// unreachable (without them). Every figure is absolute - GB and cores, not
// shares of a machine - so a big worker's headroom counts in full and the
// headroom left is what spreads the load:
//
//	mem_left = free_gb - reserved_gb - job.mem_gb
//	idle     = cores * (100 - cpu%) / 100 - reserved_cores
//	cpu_left = core_gb * (idle - job.cores)
//	gate:    mem_left >= gate_free_gb, idle >= gate_free_cores (and, when
//	         set, cpu% < gate_cpu); labels match; the node is online,
//	         approved and not draining
//	score:   harmonic_mean(mem_left, cpu_left)
//	         - 2*running + 10*has_mirror(repo) + 5*warm_deps
//
// The score is in GB (an idle core weighs CoreGB); the harmonic mean is
// dominated by the scarcer of the two resources, so placements spread to
// keep both memory and CPU headroom even across workers. Without a core
// count it is the memory left. A Hyper-V Dynamic Memory guest counts against its
// ceiling (memcap). The highest score wins; ties go to the name for
// determinism. Nobody passing is not an error - the hub queues the request.
package sched

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Params are the gate thresholds, the score's exchange rate and the
// per-type reservations.
type Params struct {
	// GateFreeGB is the memory a worker must still have free after the
	// job's reservation (default 4; 0 = the job only has to fit).
	GateFreeGB float64
	// GateFreeCores is the idle CPU a worker needs to take a job, in cores
	// after the reservations of jobs that just started (default 1; 0 = no
	// CPU gate). The job's own cores are not required: an idle 4-core
	// laptop still takes a 4-core build, it just scores low.
	GateFreeCores float64
	// GateCPU is an optional ceiling in percent (0 = none): a worker at or
	// above it takes nothing, however many cores it has idle.
	GateCPU float64
	// CoreGB is what an idle core is worth against a GB of free memory in
	// the score (default 4: the GB per vCPU of a general-purpose VM).
	CoreGB float64
	// CPUWarmup is how long, in seconds, a started job's cores stay
	// reserved - until its load shows in the worker's CPU% (default 60).
	CPUWarmup float64
	MemGB     map[string]float64 // by job type; "default" fallback
	Cores     map[string]float64 // CPU by job type; "default" fallback
}

// Defaults: claude/copilot sessions 2 GB and a core, builds 8 GB and 4
// cores (DESIGN §5.1).
func Defaults() Params {
	return Params{GateFreeGB: 4, GateFreeCores: 1, CoreGB: 4, CPUWarmup: 60,
		MemGB: map[string]float64{"claude": 2, "copilot": 2, "build": 8, "test": 4, "default": 2},
		Cores: map[string]float64{"claude": 1, "copilot": 1, "build": 4, "test": 2, "default": 1},
	}
}

// MemFor returns the reservation of a job type (explicit > by type > default).
func (p Params) MemFor(typ string, explicit float64) float64 {
	return byType(p.MemGB, typ, explicit, 2)
}

// CoresFor returns the CPU of a job type in cores (explicit, e.g. the
// job's --cpus > by type > default).
func (p Params) CoresFor(typ string, explicit float64) float64 {
	return byType(p.Cores, typ, explicit, 1)
}

func byType(m map[string]float64, typ string, explicit, fallback float64) float64 {
	if explicit > 0 {
		return explicit
	}
	if v, ok := m[typ]; ok && typ != "" {
		return v
	}
	if v, ok := m["default"]; ok {
		return v
	}
	return fallback
}

// TypeOf infers a job type from its command (claude / copilot sessions).
func TypeOf(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	base := strings.ToLower(argv[0])
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(base, ".exe"), ".cmd"), ".ps1")
	switch base {
	case "claude", "copilot":
		return base
	}
	return ""
}

// Candidate is one worker as the scheduler sees it.
type Candidate struct {
	Name          string            // display name (reasons, tie-breaks)
	ID            string            // node ID (Result.WorkerID; Exclude matches it too)
	Labels        map[string]string // includes os=, arch=
	FreeGB        float64           // memory not in use (a Dynamic Memory guest: of its ceiling)
	TotalGB       float64           // memory (a Dynamic Memory guest: its ceiling); 0 = unknown
	CPU           float64           // percent of the whole machine
	Cores         float64           // logical CPUs; 0 = unknown: no core gate, CPU left out of the score
	Running       int               // running jobs
	ReservedGB    float64           // reservations of placed/active jobs
	ReservedCores float64           // cores of jobs placed or started within CPUWarmup
	HasMirror     bool              // mirror of the requested repo
	Warm          bool              // warm dependency caches for the repo
	Online        bool
	Eligible      bool   // approved worker, not draining
	Why           string // why not eligible (draining, pending, ...)
}

// Request describes the job to place.
type Request struct {
	MemGB   float64
	Cores   float64 // CPU the job will use (Params.CoresFor); 0 = none counted
	Require map[string]string
	Exclude []string
}

// IdleCores is c's idle CPU in cores less what jobs that just started will
// take (0 for an unknown core count).
func IdleCores(c Candidate) float64 {
	return c.Cores*max(0, 100-c.CPU)/100 - c.ReservedCores
}

// headroom is what c has left once req is placed there: memory in GB and
// idle CPU in GB as well (CoreGB per core). Without a core count the CPU
// side is left out (it equals the memory side).
func (p Params) headroom(c Candidate, req Request) (mem, cpu float64) {
	mem = c.FreeGB - c.ReservedGB - req.MemGB
	if c.Cores <= 0 || p.CoreGB <= 0 {
		return mem, mem
	}
	return mem, p.CoreGB * (IdleCores(c) - req.Cores)
}

// Score is the placement score of a candidate for req, in GB: the harmonic
// mean of the memory and the CPU it has left with the job - the scarcer
// one dominates, a worker plentiful in one and short of the other doesn't
// win on the plentiful one - less 2 per running job, plus repo affinity.
// Headroom at or below zero scores as such (the job oversubscribes CPU).
func (p Params) Score(c Candidate, req Request) float64 {
	mem, cpu := p.headroom(c, req)
	s := min(mem, cpu)
	if s > 0 {
		s = 2 * mem * cpu / (mem + cpu)
	}
	s -= 2 * float64(c.Running)
	if c.HasMirror {
		s += 10
	}
	if c.Warm {
		s += 5
	}
	return s
}

// Result of Pick.
type Result struct {
	Worker   string // the winner's name
	WorkerID string // and node ID
	Score    float64
	Reasons  map[string]string // per rejected candidate
}

// Summary is a one-line explanation of why nobody passed.
func (r Result) Summary() string {
	if len(r.Reasons) == 0 {
		return "no workers"
	}
	names := make([]string, 0, len(r.Reasons))
	for n := range r.Reasons {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + ": " + r.Reasons[n]
	}
	return strings.Join(parts, "; ")
}

// Pick returns the best candidate passing the gate ("" if none).
func Pick(p Params, cands []Candidate, req Request) Result {
	res := Result{Reasons: map[string]string{}}
	best := -1e18
	for _, c := range cands {
		if why := reject(p, c, req); why != "" {
			res.Reasons[c.Name] = why
			continue
		}
		s := p.Score(c, req)
		if res.Worker == "" || s > best || (s == best && c.Name < res.Worker) {
			res.Worker, res.WorkerID, res.Score, best = c.Name, c.ID, s, s
		}
	}
	return res
}

func reject(p Params, c Candidate, req Request) string {
	switch {
	case IsAdminWorker(c.Labels):
		// never placed: an admin worker is used by its name only
		return "admin worker (by name only)"
	case slices.Contains(req.Exclude, c.Name) || (c.ID != "" && slices.Contains(req.Exclude, c.ID)):
		return "excluded"
	case !c.Online:
		return "offline"
	case !c.Eligible:
		if c.Why != "" {
			return c.Why
		}
		return "not eligible"
	}
	for k, v := range req.Require {
		if c.Labels[k] != v {
			return fmt.Sprintf("label %s=%q, want %q", k, c.Labels[k], v)
		}
	}
	if left := c.FreeGB - c.ReservedGB - req.MemGB; left < p.GateFreeGB {
		if p.GateFreeGB <= 0 {
			return fmt.Sprintf("free %.1f GB - reserved %.1f GB < %.1f GB needed", c.FreeGB, c.ReservedGB, req.MemGB)
		}
		return fmt.Sprintf("free %.1f GB - reserved %.1f GB - %.1f GB needed leaves %.1f GB < %.1f GB",
			c.FreeGB, c.ReservedGB, req.MemGB, left, p.GateFreeGB)
	}
	if p.GateCPU > 0 && c.CPU >= p.GateCPU {
		return fmt.Sprintf("cpu %.0f%% >= %.0f%%", c.CPU, p.GateCPU)
	}
	if idle := IdleCores(c); c.Cores > 0 && p.GateFreeCores > 0 && idle < p.GateFreeCores {
		return fmt.Sprintf("idle %.1f of %.0f cores (cpu %.0f%%, %.1f reserved) < %.1f", max(0, idle), c.Cores, c.CPU, c.ReservedCores, p.GateFreeCores)
	}
	return ""
}

// Label keys longarm puts on jobs so the hub can account reservations.
const (
	LabelMemGB = "longarm.mem_gb"
	LabelType  = "longarm.type"
	LabelRepo  = "longarm.repo"
)
