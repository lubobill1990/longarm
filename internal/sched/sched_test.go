package sched

import (
	"strings"
	"testing"
)

func TestPick(t *testing.T) {
	p := Defaults()
	base := func(name string, free float64) Candidate {
		return Candidate{Name: name, FreeGB: free, CPU: 10, Online: true, Eligible: true, Labels: map[string]string{"os": "windows"}}
	}
	// score = free - reserved - 6*running (+10 mirror, +5 warm)
	a, b := base("a", 60), base("b", 50)
	if r := Pick(p, []Candidate{a, b}, Request{MemGB: 2}); r.Worker != "a" || r.Score != 60 {
		t.Fatalf("%+v", r)
	}
	b.HasMirror, b.Warm = true, true // 65 > 60
	if r := Pick(p, []Candidate{a, b}, Request{MemGB: 2}); r.Worker != "b" || r.Score != 65 {
		t.Fatalf("%+v", r)
	}
	a.Running = 1 // 54
	a.ReservedGB = 8
	b.Running = 2 // 65 - 12 = 53
	if r := Pick(p, []Candidate{a, b}, Request{MemGB: 2}); r.Worker != "b" || r.Score != 53 {
		// a: 60 - 8 - 6 = 46
		t.Fatalf("%+v", r)
	}
	// gates
	hot := base("hot", 90)
	hot.CPU = 70
	low := base("low", 19)
	drain := base("drain", 90)
	drain.Eligible, drain.Why = false, "draining"
	off := base("off", 90)
	off.Online = false
	full := base("full", 30)
	full.ReservedGB = 25 // 30-25 < 8
	r := Pick(p, []Candidate{hot, low, drain, off, full}, Request{MemGB: 8})
	if r.Worker != "" || len(r.Reasons) != 5 {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"cpu 70%", "free 19.0 GB < 20", "draining", "offline", "reserved 25.0"} {
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
	if TypeOf([]string{`C:\x\claude.exe`, "-p"}) != "claude" || TypeOf([]string{"/usr/bin/copilot"}) != "copilot" || TypeOf([]string{"pnpm"}) != "" {
		t.Fatal("TypeOf")
	}
}
