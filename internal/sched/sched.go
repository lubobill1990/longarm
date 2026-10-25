// Package sched is the placement algorithm of DESIGN §5.2, shared by longarm-hub
// (with reservations) and longarm's client-side fallback when the hub is
// unreachable (without them):
//
//	gate:  free memory >= GateFreeGB and CPU < GateCPU, the job's
//	       reservation fits (free - reserved >= mem_gb), labels match, the
//	       node is online, approved and not draining
//	score: free - reserved - 6*running + 10*has_mirror(repo) + 5*warm_deps
//
// The highest score wins; ties go to the name for determinism. Nobody
// passing is not an error - the hub queues the request.
package sched

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Params are the gate thresholds and per-type memory reservations.
type Params struct {
	GateFreeGB float64            // default 20
	GateCPU    float64            // percent, default 70
	MemGB      map[string]float64 // by job type; "default" fallback
}

// Defaults: claude/copilot sessions 2 GB, builds 8 GB (DESIGN §5.1).
func Defaults() Params {
	return Params{GateFreeGB: 20, GateCPU: 70, MemGB: map[string]float64{
		"claude": 2, "copilot": 2, "build": 8, "test": 4, "default": 2,
	}}
}

// MemFor returns the reservation of a job type (explicit > by type > default).
func (p Params) MemFor(typ string, explicit float64) float64 {
	if explicit > 0 {
		return explicit
	}
	if v, ok := p.MemGB[typ]; ok && typ != "" {
		return v
	}
	if v, ok := p.MemGB["default"]; ok {
		return v
	}
	return 2
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
	Name       string
	Labels     map[string]string // includes os=, arch=
	FreeGB     float64           // memory not in use
	CPU        float64           // percent
	Running    int               // running jobs
	ReservedGB float64           // reservations of placed/active jobs
	HasMirror  bool              // mirror of the requested repo
	Warm       bool              // warm dependency caches for the repo
	Online     bool
	Eligible   bool   // approved worker, not draining
	Why        string // why not eligible (draining, pending, ...)
}

// Request describes the job to place.
type Request struct {
	MemGB   float64
	Require map[string]string
	Exclude []string
}

// Score is the placement score of a candidate.
func Score(c Candidate) float64 {
	s := c.FreeGB - c.ReservedGB - 6*float64(c.Running)
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
	Worker  string
	Score   float64
	Reasons map[string]string // per rejected candidate
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
		s := Score(c)
		if res.Worker == "" || s > best || (s == best && c.Name < res.Worker) {
			res.Worker, res.Score, best = c.Name, s, s
		}
	}
	return res
}

func reject(p Params, c Candidate, req Request) string {
	switch {
	case slices.Contains(req.Exclude, c.Name):
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
	if c.FreeGB < p.GateFreeGB {
		return fmt.Sprintf("free %.1f GB < %.0f GB", c.FreeGB, p.GateFreeGB)
	}
	if c.CPU >= p.GateCPU {
		return fmt.Sprintf("cpu %.0f%% >= %.0f%%", c.CPU, p.GateCPU)
	}
	if c.FreeGB-c.ReservedGB < req.MemGB {
		return fmt.Sprintf("free %.1f GB - reserved %.1f GB < %.1f GB needed", c.FreeGB, c.ReservedGB, req.MemGB)
	}
	return ""
}

// Label keys longarm puts on jobs so the hub can account reservations.
const (
	LabelMemGB = "longarm.mem_gb"
	LabelType  = "longarm.type"
	LabelRepo  = "longarm.repo"
)
