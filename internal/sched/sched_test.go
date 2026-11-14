package sched

import (
	"math"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestPick(t *testing.T) {
	p := Defaults()
	// no core count: the score is the memory left
	base := func(name string, free float64) Candidate {
		return Candidate{Name: name, FreeGB: free, CPU: 10, Online: true, Eligible: true, Labels: map[string]string{"os": "windows"}}
	}
	// score = free - reserved - need - 2*running (+10 mirror, +5 warm)
	a, b := base("a", 60), base("b", 50)
	if r := Pick(p, []Candidate{a, b}, Request{MemGB: 2}); r.Worker != "a" || !near(r.Score, 58) {
		t.Fatalf("%+v", r)
	}
	b.HasMirror, b.Warm = true, true // 63 > 58
	if r := Pick(p, []Candidate{a, b}, Request{MemGB: 2}); r.Worker != "b" || !near(r.Score, 63) {
		t.Fatalf("%+v", r)
	}
	a.Running = 1 // 60 - 8 - 2 - 2 = 48
	a.ReservedGB = 8
	b.Running = 2 // 63 - 4 = 59
	if r := Pick(p, []Candidate{a, b}, Request{MemGB: 2}); r.Worker != "b" || !near(r.Score, 59) {
		t.Fatalf("%+v", r)
	}
	// gates
	hot := base("hot", 90)
	hot.Cores, hot.CPU = 8, 95 // 0.4 cores idle
	low := base("low", 11)     // 11 - 8 leaves 3 GB
	drain := base("drain", 90)
	drain.Eligible, drain.Why = false, "draining"
	off := base("off", 90)
	off.Online = false
	full := base("full", 30)
	full.ReservedGB = 25 // 30-25-8 < 4
	r := Pick(p, []Candidate{hot, low, drain, off, full}, Request{MemGB: 8, Cores: 4})
	if r.Worker != "" || len(r.Reasons) != 5 {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"idle 0.4 of 8 cores", "leaves 3.0 GB < 4.0 GB", "draining", "offline", "reserved 25.0"} {
		if !strings.Contains(r.Summary(), want) {
			t.Errorf("summary %q lacks %q", r.Summary(), want)
		}
	}
	// labels and exclusion
	lin := base("lin", 40)
	lin.Labels = map[string]string{"os": "linux"}
	if r := Pick(p, []Candidate{base("win", 80), lin}, Request{Require: map[string]string{"os": "linux"}}); r.Worker != "lin" {
		t.Fatalf("%+v", r)
	}
	if r := Pick(p, []Candidate{base("win", 80), lin}, Request{Exclude: []string{"win"}}); r.Worker != "lin" {
		t.Fatalf("%+v", r)
	}
}

func TestMemAndType(t *testing.T) {
	p := Defaults()
	if p.MemFor("build", 0) != 8 || p.MemFor("", 0) != 2 || p.MemFor("claude", 3) != 3 || p.MemFor("zzz", 0) != 2 {
		t.Fatal("MemFor")
	}
	if p.CoresFor("build", 0) != 4 || p.CoresFor("", 0) != 1 || p.CoresFor("claude", 2.5) != 2.5 || p.CoresFor("test", 0) != 2 {
		t.Fatal("CoresFor")
	}
	if TypeOf([]string{`C:\x\claude.exe`, "-p"}) != "claude" || TypeOf([]string{"/usr/bin/copilot"}) != "copilot" || TypeOf([]string{"pnpm"}) != "" {
		t.Fatal("TypeOf")
	}
}

// The gates are absolute: the memory left after the job's reservation and
// idle cores, whatever the worker's size.
func TestGatesAbsolute(t *testing.T) {
	p := Defaults()
	w := func(name string, total, free, cores, cpu float64) Candidate {
		return Candidate{Name: name, TotalGB: total, FreeGB: free, Cores: cores, CPU: cpu, Online: true, Eligible: true}
	}
	cases := []struct {
		c    Candidate
		ok   bool
		want string
	}{
		{w("laptop16", 16, 7, 8, 10), true, ""},                   // 7 - 2 leaves 5 GB
		{w("laptop16-busy", 16, 5, 8, 10), false, "leaves 3.0 GB"}, // < 4 GB
		{w("box128", 128, 7, 32, 10), true, ""},                   // no 25 % margin
		{w("box64-hot", 64, 40, 64, 80), true, ""},                // 80 % of 64 cores: 12.8 idle
		{w("box4-hot", 16, 10, 4, 80), false, "idle 0.8 of 4 cores"},
		{w("unknown-cores", 16, 7, 0, 99), true, ""}, // no core count: no core gate
	}
	for _, tc := range cases {
		r := Pick(p, []Candidate{tc.c}, Request{MemGB: 2, Cores: 1})
		if got := r.Worker == tc.c.Name; got != tc.ok || !strings.Contains(r.Summary(), tc.want) {
			t.Errorf("%s: placed=%v summary %q, want placed=%v containing %q", tc.c.Name, got, r.Summary(), tc.ok, tc.want)
		}
	}
	p.GateCPU = 70 // the optional ceiling
	if r := Pick(p, []Candidate{w("box64-hot", 64, 40, 64, 80)}, Request{MemGB: 2}); r.Worker != "" || !strings.Contains(r.Summary(), "cpu 80% >= 70%") {
		t.Fatalf("GateCPU: %+v", r)
	}
	p = Defaults()
	p.GateFreeGB, p.GateFreeCores = 0, 0 // the job only has to fit
	if r := Pick(p, []Candidate{w("tiny", 4, 2.5, 2, 100)}, Request{MemGB: 2, Cores: 1}); r.Worker != "tiny" {
		t.Fatalf("gates off: %+v", r)
	}
	// cores of jobs that just started count against the idle ones
	busy := w("warming", 64, 40, 4, 10) // 3.6 idle
	busy.ReservedCores = 3
	if r := Pick(Defaults(), []Candidate{busy}, Request{MemGB: 2, Cores: 1}); r.Worker != "" || !strings.Contains(r.Summary(), "0.6 of 4 cores") {
		t.Fatalf("reserved cores: %+v", r)
	}
}

// The scarcer resource decides: lots of free memory doesn't win a worker
// whose CPU is nearly spent, and the other way round.
func TestScoreBalances(t *testing.T) {
	p := Defaults()
	w := func(name string, free, cores, cpu float64) Candidate {
		return Candidate{Name: name, FreeGB: free, Cores: cores, CPU: cpu, Online: true, Eligible: true}
	}
	req := Request{MemGB: 2, Cores: 1}
	// 98 GB but 1 core left (4 GB-eq: 7.7) vs 18 GB and 6.2 cores (24.8: 20.9)
	if r := Pick(p, []Candidate{w("bigmem", 100, 8, 75), w("idle", 20, 8, 10)}, req); r.Worker != "idle" || !near(r.Score, 2*18*24.8/42.8) {
		t.Fatalf("cpu-bound: %+v", r)
	}
	// 8 GB and 31 cores (15.0) vs 38 GB and 3 cores (12 GB-eq: 18.2)
	if r := Pick(p, []Candidate{w("manycores", 10, 32, 0), w("fewcores", 40, 4, 0)}, req); r.Worker != "fewcores" {
		t.Fatalf("memory-bound: %+v", r)
	}
	// placing one job after another spreads them in proportion to what
	// each worker has of its scarcer resource
	ws := []Candidate{w("w64x16", 60, 16, 5), w("w32x8", 28, 8, 5), w("w100x4", 96, 4, 5)}
	got := map[string]int{}
	for range 24 {
		r := Pick(p, ws, req)
		if r.Worker == "" {
			t.Fatalf("nobody after %v: %s", got, r.Summary())
		}
		got[r.Worker]++
		for i := range ws {
			if ws[i].Name == r.Worker {
				ws[i].ReservedGB += req.MemGB
				ws[i].ReservedCores += req.Cores
				ws[i].Running++
			}
		}
	}
	if got["w64x16"] != 14 || got["w32x8"] != 7 || got["w100x4"] != 3 {
		t.Fatalf("spread %v, want 14/7/3", got)
	}
	// a job wanting more cores than are idle may still go (the gate asks
	// for one), but scores below zero
	if r := Pick(p, []Candidate{w("laptop", 16, 4, 50)}, Request{MemGB: 8, Cores: 4}); r.Worker != "laptop" || r.Score >= 0 {
		t.Fatalf("oversubscribed: %+v", r)
	}
}

// TestPickNeverAdminWorker: an admin worker (AdminLabel) is never placed,
// however good it looks, with or without a matching --require.
func TestPickNeverAdminWorker(t *testing.T) {
	adm := Candidate{Name: "a-admin", Labels: map[string]string{AdminLabel: "true", "os": "windows"}, FreeGB: 500, TotalGB: 512,
		Online: true, Eligible: true}
	plain := Candidate{Name: "w", Labels: map[string]string{"os": "windows"}, FreeGB: 30, TotalGB: 64, Online: true, Eligible: true}
	for _, req := range []Request{{}, {Require: map[string]string{AdminLabel: "true"}}, {Require: map[string]string{"os": "windows"}}} {
		r := Pick(Defaults(), []Candidate{adm, plain}, req)
		if r.Worker == "a-admin" || !strings.Contains(r.Reasons["a-admin"], "admin worker") {
			t.Fatalf("%+v: %+v", req, r)
		}
	}
	if err := CheckUserLabels(map[string]string{"Longarm.IO/admin": "false"}); err == nil {
		t.Fatal("reserved label accepted")
	}
	if err := CheckUserLabels(map[string]string{"pool": "a"}); err != nil {
		t.Fatal(err)
	}
}
