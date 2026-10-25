package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
)

const GB = 1 << 30

func TestCountAgentsLeafRule(t *testing.T) {
	procs := []platform.ProcSample{
		// Claude Code: bg-job daemon -> wrapper -> session (all claude), one session.
		{PID: 10, PPID: 1, Name: "claude", RSS: 1 * GB},
		{PID: 11, PPID: 10, Name: "claude", RSS: 1 * GB},
		{PID: 12, PPID: 11, Name: "claude", RSS: 1 * GB},
		// A second, standalone session started from a terminal.
		{PID: 20, PPID: 2, Name: "Claude", RSS: 2 * GB},
		// A claude session spawning a non-claude child (node) is still a leaf.
		{PID: 30, PPID: 3, Name: "claude", RSS: 1 * GB},
		{PID: 31, PPID: 30, Name: "node", RSS: 5 * GB},
		// Copilot CLI launched by its npm node loader: node -> copilot.
		{PID: 40, PPID: 4, Name: "node", RSS: 1 * GB},
		{PID: 41, PPID: 40, Name: "copilot", RSS: 3 * GB},
		// Unrelated.
		{PID: 50, PPID: 1, Name: "claude-helper", RSS: 1 * GB},
	}
	got := CountAgents(procs, AgentNames)
	if c := got["claude"]; c.Sessions != 3 || c.Processes != 5 || c.MemBytes != 6*GB {
		t.Errorf("claude = %+v", c)
	}
	if c := got["copilot"]; c.Sessions != 1 || c.Processes != 1 || c.MemBytes != 3*GB {
		t.Errorf("copilot = %+v", c)
	}
	empty := CountAgents(nil, AgentNames)
	if empty["claude"] != (AgentStat{}) || len(empty) != 2 {
		t.Errorf("empty = %+v", empty)
	}
}

func TestComputeCPUAndTop(t *testing.T) {
	t0 := time.Unix(1000, 0)
	prev := &Sample{
		Wall: t0,
		Sys:  platform.SysSample{IdleTicks: 1000, TotalTicks: 2000, NumCPU: 4, MemTotal: 16 * GB, MemAvail: 10 * GB},
		Procs: []platform.ProcSample{
			{PID: 1, Name: "build", CPUTime: 1 * time.Second, StartTime: 7},
			{PID: 2, Name: "build", CPUTime: 0, StartTime: 7},
			{PID: 3, Name: "idle", CPUTime: 5 * time.Second, StartTime: 7},
			{PID: 4, Name: "reused", CPUTime: 100 * time.Second, StartTime: 1},
		},
	}
	cur := &Sample{
		Wall: t0.Add(2 * time.Second),
		Sys:  platform.SysSample{IdleTicks: 1300, TotalTicks: 3000, NumCPU: 4, MemTotal: 16 * GB, MemAvail: 4 * GB, Uptime: time.Hour},
		Procs: []platform.ProcSample{
			// 2 cores busy over 2 s on a 4-core box = 50%.
			{PID: 1, Name: "build", CPUTime: 3 * time.Second, StartTime: 7, RSS: 2 * GB},
			{PID: 2, Name: "build", CPUTime: 2 * time.Second, StartTime: 7, RSS: 1 * GB},
			{PID: 3, Name: "idle", CPUTime: 5 * time.Second, StartTime: 7, RSS: 4 * GB},
			// pid reused by a new process: no delta must be computed.
			{PID: 4, Name: "reused", CPUTime: 1 * time.Second, StartTime: 2, RSS: 1},
			{PID: 5, Name: "new", CPUTime: 9 * time.Second, StartTime: 9},
		},
	}
	s := Compute(prev, cur)
	if s.CPUPercent != 70 {
		t.Errorf("cpu = %v, want 70", s.CPUPercent)
	}
	if s.MemUsed != 12*GB || s.MemTotal != 16*GB || s.NProc != 5 || s.Threads != 4 {
		t.Errorf("mem/nproc: %+v", s)
	}
	if s.TopCPU[0].Name != "build" || s.TopCPU[0].CPUPercent != 50 || s.TopCPU[0].Count != 2 {
		t.Errorf("topcpu = %+v", s.TopCPU)
	}
	for _, g := range s.TopCPU {
		if (g.Name == "reused" || g.Name == "new" || g.Name == "idle") && g.CPUPercent != 0 {
			t.Errorf("%s cpu = %v, want 0", g.Name, g.CPUPercent)
		}
	}
	if s.TopMem[0].Name != "idle" || s.TopMem[1].Name != "build" || s.TopMem[1].MemBytes != 3*GB {
		t.Errorf("topmem = %+v", s.TopMem)
	}
	if len(s.TopCPU) != 4 || len(s.TopMem) != 4 { // 4 distinct names < TopN
		t.Errorf("top lengths %d %d", len(s.TopCPU), len(s.TopMem))
	}
	if first := Compute(nil, cur); first.CPUPercent != 0 || first.TopCPU[0].CPUPercent != 0 {
		t.Errorf("no prev: %+v", first)
	}
}

// TestLiveSample exercises the real platform implementation.
func TestLiveSample(t *testing.T) {
	s := &Sampler{DiskPath: t.TempDir(), JobsFn: func() int { return 2 }}
	snap, err := s.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.MemTotal == 0 || snap.MemUsed == 0 || snap.MemUsed > snap.MemTotal {
		t.Errorf("mem %d/%d", snap.MemUsed, snap.MemTotal)
	}
	if snap.NProc < 5 || snap.Threads < 1 || snap.Uptime <= 0 || snap.JobsRunning != 2 {
		t.Errorf("snapshot %+v", snap)
	}
	if snap.CPUPercent < 0 || snap.CPUPercent > 100 {
		t.Errorf("cpu %v", snap.CPUPercent)
	}
	if snap.DiskTotal == 0 || snap.DiskFree > snap.DiskTotal {
		t.Errorf("disk %d/%d", snap.DiskFree, snap.DiskTotal)
	}
	var withMem int
	for _, g := range snap.TopMem {
		if g.MemBytes > 0 {
			withMem++
		}
	}
	if withMem == 0 {
		t.Errorf("no process memory: %+v", snap.TopMem)
	}
	t.Logf("cpu=%.1f%% mem=%.1f/%.1f GB nproc=%d agents=%+v topcpu=%+v", snap.CPUPercent,
		float64(snap.MemUsed)/GB, float64(snap.MemTotal)/GB, snap.NProc, snap.Agents, snap.TopCPU)
}
