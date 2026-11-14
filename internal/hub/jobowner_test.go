package hub

import (
	"testing"

	pb "longarm/proto/longarm/v1"
)

// TestOwnWorkerForgedJobs: a user's own worker can't put jobs of another
// user who may not use it into the index (their quota, their lists), nor
// drop a reservation placed on another worker.
func TestOwnWorkerForgedJobs(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	h.mu.Lock()
	defer h.mu.Unlock()
	aw := h.nodeLocked("aw")
	before := h.usageLocked("bob")
	h.reservations["BOBRES"] = &reservation{jobID: "BOBRES", worker: h.nodeLocked("w1").rec.ID, owner: "bob"}
	running := pb.JobState_JOB_STATE_RUNNING
	h.applyJobsLocked(aw, &pb.JobsReport{Jobs: []*pb.Job{
		{Spec: &pb.JobSpec{Id: "MINE", Owner: "alice"}, State: running},
		{Spec: &pb.JobSpec{Id: "BOBJOB", Owner: "bob", MemLimitBytes: 512 << 30}, State: running},
		{Spec: &pb.JobSpec{Id: "BOBRES", Owner: "bob"}, State: running},
	}})
	jm := h.jobs[aw.rec.ID]
	if jm["MINE"] == nil || jm["BOBJOB"] != nil || jm["BOBRES"] != nil {
		t.Fatalf("index of alice's worker: %v", jm)
	}
	if h.reservations["BOBRES"] == nil {
		t.Fatal("alice's worker dropped bob's reservation on w1")
	}
	if u := h.usageLocked("bob"); u.Jobs != before.Jobs+1 || u.MemGb != before.MemGb { // + the reservation only
		t.Fatalf("bob's usage %+v, before %+v", u, before)
	}
}

func TestGuessLimiterIPv6Net(t *testing.T) {
	l := newGuessLimiter(0.001, 2)
	if !l.allow("[2001:db8:1:2::1]:5000") || !l.allow("[2001:db8:1:2::ffff]:5000") || l.allow("[2001:db8:1:2:aaaa::7]:1") {
		t.Fatal("one /64 got more than its burst")
	}
	if !l.allow("[2001:db8:1:3::1]:5000") || !l.allow("192.0.2.1:1") || !l.allow("192.0.2.2:1") {
		t.Fatal("other networks share the bucket")
	}
	p := newIPLimiter(0.001, 1)
	if !p.allow("[2001:db8::1]:1") || !p.allow("[2001:db8::2]:1") {
		t.Fatal("a plain limiter is per address")
	}
}
