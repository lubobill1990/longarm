package hub

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "longarm/proto/longarm/v1"
)

// TestPlacementCountsCPU: a worker's cores come from its metrics, and the
// cores of placements and of jobs that started within the CPU warm-up
// count against its idle ones (older jobs' load is in its CPU%).
func TestPlacementCountsCPU(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.nodeLocked("aw")
	id := n.rec.ID
	n.metrics = &pb.MetricsSnapshot{MemTotalBytes: 64 << 30, MemUsedBytes: 4 << 30, CpuPercent: 10, Threads: 8}
	now := time.Now()
	running := func(spec *pb.JobSpec, started time.Duration) *jobEntry {
		return &jobEntry{job: &pb.Job{Spec: spec, State: pb.JobState_JOB_STATE_RUNNING, StartedAt: timestamppb.New(now.Add(-started))}}
	}
	h.jobs[id] = map[string]*jobEntry{
		"j1": running(&pb.JobSpec{Argv: []string{"claude", "-p"}}, 10*time.Second),                      // 1 core, warming
		"j2": running(&pb.JobSpec{Argv: []string{"make"}, Labels: map[string]string{"longarm.type": "build"}}, 5*time.Minute), // in the CPU%
		"j3": running(&pb.JobSpec{Argv: []string{"make"}, CpuLimit: 3}, 0),                               // --cpus 3, warming
	}
	defer delete(h.jobs, id)
	if got := h.warmingCoresLocked(id, now); got != 4 {
		t.Fatalf("warming cores %v, want 4", got)
	}
	r := h.reserveLocked(&pb.PlaceRequest{JobId: "01J0000000000000000000000B", Type: "build"}, id, 8, "alice") // 4 more
	defer delete(h.reservations, r.jobID)
	for _, c := range h.candidatesLocked("", "alice") {
		if c.Name == "aw" && (c.Cores != 8 || c.ReservedCores != 8) {
			t.Fatalf("candidate: cores %v reserved %v, want 8 8", c.Cores, c.ReservedCores)
		}
	}
	// 7.2 idle - 8 reserved: nothing more fits until the jobs' load shows
	if res := h.pickLocked(&pb.PlaceRequest{Type: "claude"}, 2, "alice"); res.Worker != "" || !strings.Contains(res.Reasons["aw"], "cores") {
		t.Fatalf("placed on a worker whose cores are taken: %+v", res)
	}
	delete(h.reservations, r.jobID)
	if res := h.pickLocked(&pb.PlaceRequest{Type: "claude"}, 2, "alice"); res.Worker != "aw" {
		t.Fatalf("not placed with 3.2 cores idle: %+v", res)
	}
}
