package main

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func TestParseLabelEdits(t *testing.T) {
	m, err := parseLabelEdits([]string{"pool=devbox", "gpu-", "zone=a,old-", "x="})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"pool": "devbox", "gpu": "", "zone": "a", "old": "", "x": ""}; !maps.Equal(m, want) {
		t.Fatalf("got %v, want %v", m, want)
	}
	for _, bad := range []string{"novalue", "a b=c", "-", "k=v,,"} {
		if _, err := parseLabelEdits([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestFoldMachines: longarm ls shows a session worker and its admin worker
// as one row (ADMIN), an admin worker alone keeps its row.
func TestFoldMachines(t *testing.T) {
	nodes := map[string]*pb.NodeInfo{
		"box":        {Name: "box", Role: "worker", State: "approved", Owner: "u", Hostname: "box"},
		"box-admin":  {Name: "box-admin", Role: "worker", State: "approved", Owner: "u", Hostname: "box", AdminWorker: true},
		"solo-admin": {Name: "solo-admin", Role: "worker", State: "approved", Owner: "u", Hostname: "solo", AdminWorker: true},
	}
	shown, adminOf := foldMachines([]string{"box", "box-admin", "probe-only", "solo-admin"}, nodes)
	if want := []string{"box", "probe-only", "solo-admin"}; !slices.Equal(shown, want) {
		t.Errorf("shown %v, want %v", shown, want)
	}
	if !maps.Equal(adminOf, map[string]string{"box": "box-admin"}) {
		t.Errorf("adminOf %v", adminOf)
	}
	// named alone (longarm ls box-admin): its own row
	if shown, _ := foldMachines([]string{"box-admin"}, nodes); !slices.Equal(shown, []string{"box-admin"}) {
		t.Errorf("alone: %v", shown)
	}
}

// TestLsTable: the compact default (WORKER ID STATE OS CPU% MEM GB JOBS
// LABELS), -o wide, one row per machine (ADMIN, by the hub's node-ID
// link), "offline 3d", a revoked node of a reused name left out.
func TestLsTable(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	m := &pb.MetricsSnapshot{CpuPercent: 12.4, MemUsedBytes: 8 << 30, MemTotalBytes: 32 << 30, JobsRunning: 1}
	rows := []lsRow{
		{name: "box", state: "offline", path: "hub", node: &pb.NodeInfo{Name: "box", Id: "0000dead-0000-4000-8000-000000000000", Role: "worker",
			State: "revoked", Owner: "u"}}, // an earlier box
		{name: "box", state: "online", snap: m, path: "hub", node: &pb.NodeInfo{Name: "box", Id: "1111aaaa-0000-4000-8000-000000000001", Role: "worker",
			State: "approved", Owner: "u", Hostname: "box", Os: "windows", Version: "0.1.8", UpdateAvailable: &pb.UpdateAvailable{Version: "0.1.9"},
			Labels:    map[string]string{"os": "windows", "arch": "amd64", "pool": "devbox"},
			Companion: "sys", CompanionId: "2222bbbb-0000-4000-8000-000000000002"}},
		{name: "sys", state: "online", snap: m, path: "hub", node: &pb.NodeInfo{Name: "sys", Id: "2222bbbb-0000-4000-8000-000000000002", Role: "worker",
			State: "approved", Owner: "u", Hostname: "box", AdminWorker: true, Labels: map[string]string{"longarm.io/admin": "true"},
			Companion: "box", CompanionId: "1111aaaa-0000-4000-8000-000000000001", Machine: "box"}},
		{name: "old", state: "offline", snap: m, path: "hub", node: &pb.NodeInfo{Name: "old", Role: "worker", State: "approved", Owner: "u",
			Version: "v0.1.9", LastSeen: timestamppb.New(now.Add(-75 * time.Hour))}},
	}
	var b strings.Builder
	if failed := writeLsTable(&b, rows, false, now); failed != 0 {
		t.Fatalf("failed %d", failed)
	}
	out := b.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || strings.Join(strings.Fields(lines[0]), " ") != "WORKER ID STATE OS VERSION CPU% MEM GB JOBS LABELS ADMIN OWNER" {
		t.Fatalf("compact:\n%s", out)
	}
	if f := strings.Fields(lines[1]); !slices.Equal(f, []string{"box", "1111aaaa", "online", "windows", "0.1.8↑", "12", "8.0/32.0", "1", "pool=devbox", "online", "(sys)", "u"}) {
		t.Errorf("box row: %q", f)
	}
	if f := strings.Fields(lines[2]); !slices.Equal(f, []string{"old", "-", "offline", "3d", "-", "0.1.9", "-", "-", "-", "-", "-", "u"}) {
		t.Errorf("old row: %q", f)
	}
	if lines[3] != "↑ 0.1.9 available for box: longarm worker update <worker> (owners)" {
		t.Errorf("footer: %q", lines[3])
	}
	b.Reset()
	writeLsTable(&b, rows, true, now)
	if out := b.String(); !strings.Contains(out, "CLAUDE") || !strings.Contains(out, "PATH") || strings.Contains(out, "sys  ") {
		t.Fatalf("wide:\n%s", out)
	}
}
