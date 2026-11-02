package tray

import (
	"testing"

	"time"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The owner's notifications of an update: no "offline" while it restarts
// for it, "updated" instead of "online"; a failure, an update that didn't
// finish (edge case 2), a restart of the machine needed (3); a plain
// network blip still says offline (7).
func TestRulesUpdate(t *testing.T) {
	r := NewRules()
	now := time.Now()
	w := &pb.PoolWorker{Name: "box", State: "online", Node: &pb.NodeInfo{Name: "box", Version: "0.1.29"}}
	v := &pb.PoolView{Workers: []*pb.PoolWorker{w}}
	titles := func() []string {
		var out []string
		for _, n := range r.Pool(v, now) {
			out = append(out, n.Title+": "+n.Body)
		}
		return out
	}
	titles()
	w.Node.Update = &pb.NodeUpdate{State: "updating", From: "0.1.29", To: "0.1.30", Phase: "installing"}
	if n := titles(); len(n) != 0 {
		t.Fatalf("started: %v", n)
	}
	w.State = "offline"
	if n := titles(); len(n) != 0 {
		t.Fatalf("restarting for the update: %v", n)
	}
	w.State, w.Node.Version, w.Node.Update = "online", "0.1.30", nil
	if n := titles(); len(n) != 1 || n[0] != "box updated: now on Longarm 0.1.30" {
		t.Fatalf("back: %v", n)
	}
	// didn't finish
	w.Node.Update = &pb.NodeUpdate{State: "updating", From: "0.1.30", To: "0.1.31", Phase: "installing"}
	titles()
	w.State = "offline"
	titles()
	w.Node.Update = &pb.NodeUpdate{State: "timed_out", From: "0.1.30", To: "0.1.31", Phase: "installing"}
	if n := titles(); len(n) != 1 || n[0] != "box update didn't finish: the update to 0.1.31 didn't finish (last phase: installing); the worker is not back" {
		t.Fatalf("timed out: %v", n)
	}
	// back on the old version: failed
	w.State, w.Node.Update = "online", &pb.NodeUpdate{State: "failed", From: "0.1.30", To: "0.1.31", Code: "timeout", Error: "the update to 0.1.31 didn't finish"}
	if n := titles(); len(n) != 2 || n[0] != "box update failed: the update to 0.1.31 failed: the update to 0.1.31 didn't finish" || n[1] != "box online: reconnected" {
		t.Fatalf("failed: %v", n)
	}
	// 3010
	w.Node.Update = &pb.NodeUpdate{State: "restart_pending", To: "0.1.31"}
	if n := titles(); len(n) != 1 || n[0] != "box needs a restart: restart the machine to finish installing Longarm 0.1.31" {
		t.Fatalf("restart pending: %v", n)
	}
	// a blip without an update: offline as ever
	w.Node.Update = nil
	titles()
	w.State, w.Error = "offline", "connection reset"
	if n := titles(); len(n) != 1 || n[0] != "box offline: connection reset" {
		t.Fatalf("blip: %v", n)
	}
}
