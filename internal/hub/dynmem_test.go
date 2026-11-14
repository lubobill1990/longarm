package hub

import (
	"testing"

	pb "longarm/proto/longarm/v1"
)

// TestDynamicMemoryPlacement: a Hyper-V Dynamic Memory guest showing 4 GB
// (2 GB used) that can grow to 88 GB takes a 20 GB job - placement counts
// against the ceiling; the same worker without the ceiling does not.
func TestDynamicMemoryPlacement(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.nodeLocked("aw")
	n.metrics = &pb.MetricsSnapshot{MemTotalBytes: 4 << 30, MemUsedBytes: 2 << 30, MemMaxBytes: 88 << 30, CpuPercent: 1}
	for _, c := range h.candidatesLocked("", "alice") {
		if c.Name == "aw" && (c.TotalGB != 88 || c.FreeGB != 86) {
			t.Errorf("candidate: total %v free %v, want 88 86", c.TotalGB, c.FreeGB)
		}
	}
	if r := h.pickLocked(&pb.PlaceRequest{}, 20, "alice"); r.Worker != "aw" {
		t.Errorf("dynamic: not placed: %+v", r)
	}
	recordMetricsLocked(n, n.metrics)
	if p := n.history[len(n.history)-1]; p.Mem < 2.2 || p.Mem > 2.3 { // 2 of 88 GB
		t.Errorf("history mem %v%%", p.Mem)
	}

	n.metrics.MemMaxBytes = 0 // static 4 GB: 2 GB free
	if r := h.pickLocked(&pb.PlaceRequest{}, 20, "alice"); r.Worker != "" {
		t.Errorf("static: placed on %s", r.Worker)
	}
}
