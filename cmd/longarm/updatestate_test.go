package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// longarm ls' STATE of a worker whose machine updates: what the hub says
// instead of "offline" (edge cases 1-3, 9, 12); a worker without an
// update keeps its plain state (edge case 7).
func TestLsStateUpdate(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	started := timestamppb.New(now.Add(-7 * time.Minute))
	for _, c := range []struct {
		state string
		u     *pb.NodeUpdate
		want  string
	}{
		{"offline", &pb.NodeUpdate{State: "updating", From: "0.1.29", To: "0.1.30", Phase: "installing"}, "updating 0.1.29→0.1.30 (restarting, last installing)"},
		{"online", &pb.NodeUpdate{State: "updating", From: "0.1.29", To: "0.1.30", Phase: "installing"}, "updating 0.1.29→0.1.30 (installing)"},
		{"online", &pb.NodeUpdate{State: "updating", From: "0.1.29", To: "0.1.30", Phase: "downloading", BytesDone: 12 << 20, BytesTotal: 42 << 20},
			"updating 0.1.29→0.1.30 (downloading 12/42 MB)"},
		{"online", &pb.NodeUpdate{State: "updating", From: "0.1.20", To: "0.1.30"}, "updating 0.1.20→0.1.30"},
		{"offline", &pb.NodeUpdate{State: "updating", From: "0.1.20", To: "0.1.30"}, "updating 0.1.20→0.1.30 (restarting)"},
		{"online", &pb.NodeUpdate{State: "requested", From: "0.1.20", To: "0.1.30"}, "update requested 0.1.20→0.1.30"},
		{"offline", &pb.NodeUpdate{State: "timed_out", From: "0.1.29", To: "0.1.30", Phase: "installing", StartedAt: started},
			"offline: update to 0.1.30 didn't finish (last phase installing, started 7m ago)"},
		{"online", &pb.NodeUpdate{State: "failed", From: "0.1.29", To: "0.1.30", Code: "signature"}, "online, update to 0.1.30 failed (signature)"},
		{"online", &pb.NodeUpdate{State: "mismatch", From: "0.1.29", To: "0.1.30"}, "online, came back on another version than 0.1.30"},
		{"online", &pb.NodeUpdate{State: "restart_pending", From: "0.1.29", To: "0.1.30"}, "online, restart the machine to finish 0.1.30"},
		{"offline", nil, "offline"},
	} {
		r := lsRow{name: "w", state: c.state, node: &pb.NodeInfo{Name: "w", Update: c.u}}
		if got := lsState(r, now); got != c.want {
			t.Errorf("%v: %q, want %q", c.u, got, c.want)
		}
	}
	// in the table
	rows := []lsRow{{name: "w", state: "offline", path: "hub", node: &pb.NodeInfo{Name: "w", Role: "worker", State: "approved", Version: "0.1.29",
		Update: &pb.NodeUpdate{State: "updating", From: "0.1.29", To: "0.1.30", Phase: "installing"}}}}
	var b strings.Builder
	writeLsTable(&b, rows, false, now)
	if !strings.Contains(b.String(), "updating 0.1.29→0.1.30 (restarting, last installing)") {
		t.Fatalf("table:\n%s", b.String())
	}
}

// Edge case 5: longarm worker update refuses a second update while the
// hub shows one in progress - of the worker or of its companion (one MSI
// updates the machine) - with what runs; a timed-out one doesn't block.
func TestUpdateBusyRefused(t *testing.T) {
	now := time.Now()
	nodes := []*pb.NodeInfo{
		{Name: "box", Companion: "box-admin", Online: true, Update: &pb.NodeUpdate{State: "updating", From: "0.1.29", To: "0.1.30", Phase: "downloading",
			BytesDone: 12 << 20, BytesTotal: 42 << 20, Initiator: "rollout", StartedAt: timestamppb.New(now.Add(-2 * time.Minute))}},
		{Name: "box-admin", Companion: "box"},
		{Name: "lab", Update: &pb.NodeUpdate{State: "timed_out", To: "0.1.30"}},
		{Name: "pc", Update: &pb.NodeUpdate{State: "failed", To: "0.1.30"}},
	}
	err := updateBusyError("box", nodes, now)
	var ce *codedError
	if !errors.As(err, &ce) || ce.code != exitTempFail || !strings.Contains(err.Error(), "already updating to 0.1.30 (downloading 12/42 MB), started 2m ago by a rollout") {
		t.Fatalf("box: %v", err)
	}
	if err := updateBusyError("box-admin", nodes, now); err == nil || !strings.Contains(err.Error(), "box is already updating") {
		t.Fatalf("its admin worker: %v", err)
	}
	for _, w := range []string{"lab", "pc", "other"} {
		if err := updateBusyError(w, nodes, now); err != nil {
			t.Errorf("%s: %v", w, err)
		}
	}
}
