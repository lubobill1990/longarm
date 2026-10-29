package main

import (
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lubobill1990/longarm/internal/machine"
	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// lsRow is one worker in longarm ls: from the hub (node) or probed directly.
type lsRow struct {
	name  string
	state string // online | offline | draining | <hub state> | ok (probed)
	snap  *pb.MetricsSnapshot
	path  string
	err   error
	node  *pb.NodeInfo
	// src: where snap comes from - "direct" (the daemon's stream or a
	// probe), "hub" - and when it was taken (snap.time)
	src string
}

// lsSource is the METRICS column: "direct 2s", "hub 1s", "hub 3m (stale)".
func lsSource(r lsRow, now time.Time) string {
	if r.snap == nil || r.src == "" {
		return "-"
	}
	s := r.src
	if t := r.snap.GetTime(); t != nil {
		age := now.Sub(t.AsTime())
		if age < 0 {
			age = 0
		}
		s += " " + humanAgo(age)
		if age > 30*time.Second {
			s += " (stale)"
		}
	}
	return s
}

// writeLsTable prints longarm ls' table and returns how many rows failed.
//
// One row per machine (foldMachines): a session worker's admin worker shows
// in ADMIN. The default columns: WORKER STATE OS CPU% MEM JOBS LABELS;
// wide: today's long form (sessions, disk, uptime, top CPU, path). ADMIN
// and OWNER come last, only when some worker is an admin worker / has an
// owner. OWNER: "-" = a fleet worker, else its owner, "+shared" when the
// owner shares it, "+admin" for an admin worker.
func writeLsTable(w io.Writer, rows []lsRow, wide bool, now time.Time) (failed int) {
	nodes := map[string]*pb.NodeInfo{}
	var names []string
	byName := map[string]lsRow{}
	for _, r := range rows {
		names = append(names, r.name)
		byName[r.name] = r
		if r.node != nil {
			nodes[r.name] = r.node
		}
	}
	shown, adminOf := foldMachines(names, nodes)
	admins, owners := false, false
	for _, r := range rows {
		admins = admins || (r.node != nil && machine.FromNode(r.node).Admin)
		owners = owners || r.node.GetOwner() != ""
	}
	tail := func(r lsRow) string {
		var s string
		if admins {
			switch a, ok := adminOf[r.name]; {
			case ok:
				s += "\t" + lsState(byName[a], now) + " (" + a + ")"
			case r.node != nil && machine.FromNode(r.node).Admin:
				s += "\tthis worker"
			default:
				s += "\t-"
			}
		}
		if owners {
			o := dash(r.node.GetOwner())
			if sharesString(r.node.GetShares()) != "-" {
				o += " +shared"
			}
			if r.node.GetAdminWorker() {
				o += " +admin"
			}
			s += "\t" + o
		}
		return s
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	head := "WORKER\tSTATE\tOS\tVERSION\tCPU%\tMEM GB\tJOBS\tLABELS"
	if wide {
		// VERSION last: STATE is followed by the metrics ("online  -": none yet)
		head = "WORKER\tSTATE\tCPU%\tMEM GB\tJOBS\tCLAUDE\tCOPILOT\tDISK FREE\tUPTIME\tTOP CPU\tMETRICS\tPATH\tVERSION"
	}
	avail := map[string][]string{} // newer version -> workers (shown)
	if admins {
		head += "\tADMIN"
	}
	if owners {
		head += "\tOWNER"
	}
	fmt.Fprintln(tw, head)
	for _, r := range rows {
		if r.err != nil {
			failed++
		}
	}
	for _, n := range shown {
		r := byName[n]
		if u := r.node.GetUpdateAvailable().GetVersion(); u != "" && r.err == nil {
			avail[u] = append(avail[u], r.name)
		}
		if r.err != nil {
			fmt.Fprintf(tw, "%s\terror\tERROR: %s\n", r.name, oneLine(r.err))
			continue
		}
		m := r.snap
		if r.state == "offline" { // last known numbers of an offline worker are no state
			m = nil
		}
		jobs := "-"
		if m != nil {
			jobs = fmt.Sprint(m.JobsRunning)
			if r.node != nil && r.node.ReservedGb > 0 {
				jobs = fmt.Sprintf("%d (%.0fG res)", r.node.JobsActive, r.node.ReservedGb)
			}
		}
		if !wide {
			cpu, mem := "-", "-"
			if m != nil {
				cpu = fmt.Sprintf("%.0f", m.CpuPercent)
				mem = humanBytes(m.MemUsedBytes) + "/" + humanBytes(m.MemTotalBytes)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s%s\n", r.name, lsState(r, now), dash(r.node.GetOs()), lsVersion(r.node),
				cpu, mem, jobs, lsLabels(r.node.GetLabels()), tail(r))
			continue
		}
		if m == nil {
			fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t-\t-\t-\t-\t-\t-\t%s\t%s%s\n", r.name, lsState(r, now), r.path, lsVersion(r.node), tail(r))
			continue
		}
		agent := func(k string) string {
			a := m.Agents[k]
			if a == nil || a.Processes == 0 {
				return "0"
			}
			return fmt.Sprintf("%d (%sG)", a.Sessions, humanBytes(a.MemBytes))
		}
		var top []string
		for _, g := range m.TopCpu {
			if len(top) == 3 {
				break
			}
			top = append(top, fmt.Sprintf("%s %.0f%%", g.Name, g.CpuPercent))
		}
		fmt.Fprintf(tw, "%s\t%s\t%.1f\t%s/%s\t%s\t%s\t%s\t%s G\t%s\t%s\t%s\t%s\t%s%s\n", r.name, lsState(r, now), m.CpuPercent,
			humanBytes(m.MemUsedBytes), humanBytes(m.MemTotalBytes), jobs, agent("claude"),
			agent("copilot"), humanBytes(m.DiskFreeBytes), humanDuration(time.Duration(m.UptimeSeconds)*time.Second),
			strings.Join(top, ", "), lsSource(r, now), r.path, lsVersion(r.node), tail(r))
	}
	tw.Flush()
	vs := make([]string, 0, len(avail))
	for v := range avail {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	for _, v := range vs {
		fmt.Fprintf(w, "↑ %s available for %s: longarm worker update <worker> (owners)\n", v, strings.Join(avail[v], ", "))
	}
	return failed
}

// lsVersion: the worker's version, "↑" when the hub has a newer stable
// release for it (NodeInfo.update_available).
func lsVersion(n *pb.NodeInfo) string {
	v := strings.TrimPrefix(n.GetVersion(), "v")
	if v == "" {
		return "-"
	}
	if n.GetUpdateAvailable().GetVersion() != "" {
		v += "↑"
	}
	return v
}

// lsState is a row's state; an offline worker says when the hub last saw
// it ("offline 3d").
func lsState(r lsRow, now time.Time) string {
	if r.state == "offline" {
		if seen := r.node.GetLastSeen(); seen != nil && seen.AsTime().Before(now) {
			return "offline " + humanAgo(now.Sub(seen.AsTime()))
		}
	}
	return r.state
}

// humanAgo: "45s", "12m", "5h", "3d".
func humanAgo(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= 90*time.Minute:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= 90*time.Second:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// lsLabels: a worker's labels without os / arch (the OS column) and the
// hub's own longarm.io/ ones; "-" when none, long lists cut.
func lsLabels(m map[string]string) string {
	m = maps.Clone(m)
	for k := range m {
		if k == "os" || k == "arch" || sched.ReservedLabel(k) {
			delete(m, k)
		}
	}
	s := labelString(m)
	if s == "" {
		return "-"
	}
	if r := []rune(s); len(r) > 40 {
		s = string(r[:39]) + "…"
	}
	return s
}
