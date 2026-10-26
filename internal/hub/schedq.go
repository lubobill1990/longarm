package hub

import (
	"context"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/sched"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// queued is a placement request waiting for a worker to pass the gate.
type queued struct {
	seq      uint64
	req      *pb.PlaceRequest
	memGB    float64
	by       string
	owner    string // the user (fleet-wide quota)
	at       time.Time
	ch       chan *pb.PlaceUpdate
	position int
	reason   string
}

// Place implements Fleet.Place: pick a worker now or queue (DESIGN §5.2).
// The reservation (memory per job type) lasts until the worker reports the
// job (then the job's own mem_gb label counts while it is active) or until
// reservation_ttl passes without a submission.
func (h *Hub) Place(ctx context.Context, by string, req *pb.PlaceRequest, send func(*pb.PlaceUpdate) error) error {
	return h.PlaceFor(ctx, by, tenancy.OwnerFromBy(by), req, send)
}

// PlaceFor places a job of owner: with multi-tenancy the owner's fleet-wide
// quota must allow it (else it queues, or fails with no_queue), and only
// the owner's mirrors count for affinity on per-user workers.
func (h *Hub) PlaceFor(ctx context.Context, by, owner string, req *pb.PlaceRequest, send func(*pb.PlaceUpdate) error) error {
	if !jobs.ValidID(req.JobId) {
		return status.Error(codes.InvalidArgument, "job_id must be a valid job id (client-generated ULID)")
	}
	if req.MemGb < 0 {
		return status.Error(codes.InvalidArgument, "mem_gb must not be negative")
	}
	mem := h.params.MemFor(req.Type, req.MemGb)
	h.mu.Lock()
	if r := h.reservations[req.JobId]; r != nil { // retried call: same answer
		u := h.assignedLocked(r, 0)
		h.mu.Unlock()
		return send(u)
	}
	for w := range h.jobs {
		if e := h.jobs[w][req.JobId]; e != nil {
			h.mu.Unlock()
			if !h.viewerLocked(owner, false).CanRead(tenancy.OwnerOf(e.job.GetSpec()), nil) {
				return status.Errorf(codes.AlreadyExists, "job id %s is in use", req.JobId)
			}
			return status.Errorf(codes.AlreadyExists, "job %s already runs on %s", req.JobId, w)
		}
	}
	res := h.pickLocked(req, mem, owner)
	quota := h.overQuotaLocked(owner, mem)
	if quota != "" {
		res = sched.Result{}
	}
	if res.Worker != "" {
		r := h.reserveLocked(req.JobId, res.Worker, mem, owner)
		u := h.assignedLocked(r, res.Score)
		h.log.Info("placed", "job", req.JobId, "worker", res.Worker, "score", res.Score, "mem_gb", mem, "by", by)
		h.mu.Unlock()
		return send(u)
	}
	reason := res.Summary()
	if quota != "" {
		reason = quota
	}
	if req.NoQueue {
		h.mu.Unlock()
		if quota != "" {
			return status.Error(codes.ResourceExhausted, quota)
		}
		return status.Errorf(codes.ResourceExhausted, "no worker passes the gate: %s", reason)
	}
	h.queueSeq++
	q := &queued{seq: h.queueSeq, req: req, memGB: mem, by: by, owner: owner, at: time.Now(), ch: make(chan *pb.PlaceUpdate, 8),
		reason: reason}
	h.queue = append(h.queue, q)
	q.position = len(h.queue)
	h.bus.publishOwned(&pb.FleetEvent{Kind: "queue", Message: fmt.Sprintf("job %s queued (position %d): %s", req.JobId, q.position, q.reason)}, owner)
	h.log.Info("queued", "job", req.JobId, "position", q.position, "reason", q.reason)
	first := &pb.PlaceUpdate{State: "queued", Position: int32(q.position), Reason: q.reason, MemGb: mem}
	h.mu.Unlock()
	defer h.dequeue(q)
	if err := send(first); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case u := <-q.ch:
			if err := send(u); err != nil {
				if u.State == "assigned" { // nobody will submit: release now
					h.mu.Lock()
					delete(h.reservations, req.JobId)
					h.mu.Unlock()
				}
				return err
			}
			if u.State == "assigned" {
				return nil
			}
		}
	}
}

func (h *Hub) dequeue(q *queued) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i := slices.Index(h.queue, q); i >= 0 {
		h.queue = slices.Delete(h.queue, i, i+1)
		h.renumberLocked()
	}
}

func (h *Hub) renumberLocked() {
	for i, q := range h.queue {
		if q.position != i+1 {
			q.position = i + 1
			select {
			case q.ch <- &pb.PlaceUpdate{State: "queued", Position: int32(q.position), Reason: q.reason, MemGb: q.memGB}:
			default:
			}
		}
	}
}

// runQueue assigns queued requests whose turn has come. Requests are
// considered in order, but one that cannot run (e.g. needs os=linux) does
// not block the ones behind it.
func (h *Hub) runQueue() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.queue) == 0 {
		return
	}
	kept := h.queue[:0]
	for _, q := range h.queue {
		if why := h.overQuotaLocked(q.owner, q.memGB); why != "" {
			q.reason = why
			kept = append(kept, q)
			continue
		}
		res := h.pickLocked(q.req, q.memGB, q.owner)
		if res.Worker == "" {
			q.reason = res.Summary()
			kept = append(kept, q)
			continue
		}
		r := h.reserveLocked(q.req.JobId, res.Worker, q.memGB, q.owner)
		h.log.Info("placed from queue", "job", q.req.JobId, "worker", res.Worker, "waited", time.Since(q.at).Round(time.Millisecond))
		for len(q.ch) > 0 { // drop stale position updates so this send cannot block
			select {
			case <-q.ch:
			default:
			}
		}
		q.ch <- h.assignedLocked(r, res.Score) // the only "assigned" for q; only sent under h.mu
		h.bus.publishOwned(&pb.FleetEvent{Kind: "queue", Node: res.Worker,
			Message: fmt.Sprintf("job %s assigned to %s after %s", q.req.JobId, res.Worker, time.Since(q.at).Round(time.Second))}, q.owner)
	}
	clear(h.queue[len(kept):])
	h.queue = kept
	h.renumberLocked()
}

func (h *Hub) reserveLocked(jobID, worker string, mem float64, owner string) *reservation {
	r := &reservation{jobID: jobID, worker: worker, memGB: mem, at: time.Now(), owner: owner}
	h.reservations[jobID] = r
	return r
}

func (h *Hub) assignedLocked(r *reservation, score float64) *pb.PlaceUpdate {
	u := &pb.PlaceUpdate{State: "assigned", Worker: r.worker, Score: score, MemGb: r.memGB}
	if n := h.nodes[r.worker]; n != nil {
		u.Node = h.nodeInfoLocked(n)
	}
	return u
}

// candidatesLocked describes every worker owner may use for the scheduler (repo affinity
// for owner: workers with per-user isolation keep "<user>/<repo>" mirrors).
func (h *Hub) candidatesLocked(repo, owner string) []sched.Candidate {
	var out []sched.Candidate
	for _, n := range h.nodes {
		if n.rec.Role != string(pki.RoleWorker) || n.rec.State == stateRevoked {
			continue
		}
		if !h.mayUseLocked(owner, n) { // other users' own workers: not even in the reasons
			continue
		}
		c := sched.Candidate{Name: n.rec.Name, Labels: labelsOf(n), Online: n.sess != nil && n.metrics != nil, Eligible: true}
		switch {
		case n.rec.State != stateApproved:
			c.Eligible, c.Why = false, n.rec.State
		case n.rec.AdminDrain:
			c.Eligible, c.Why = false, "drained by an admin"
		case n.localDrain:
			c.Eligible, c.Why = false, "draining: "+n.localReason
		}
		if m := n.metrics; m != nil {
			c.FreeGB = float64(m.MemTotalBytes-min(m.MemUsedBytes, m.MemTotalBytes)) / (1 << 30)
			c.CPU = m.CpuPercent
		}
		active, reserved := h.loadLocked(n.rec.Name)
		c.Running, c.ReservedGB = int(active), reserved
		// Local limits (longarmd limits): memory kept free for the desktop user,
		// and a cap on active jobs.
		c.FreeGB = max(0, c.FreeGB-n.reservedGB)
		if c.Eligible && n.maxJobs > 0 && active >= n.maxJobs {
			c.Eligible, c.Why = false, "at its job limit ("+itoa(int64(n.maxJobs))+")"
		}
		if repo != "" {
			userRepo := owner + "/" + repo
			c.HasMirror = slices.Contains(n.rec.Mirrors, repo) || slices.Contains(n.rec.Mirrors, userRepo)
			c.Warm = slices.Contains(n.rec.WarmRepos, repo) || slices.Contains(n.rec.WarmRepos, userRepo)
		}
		out = append(out, c)
	}
	return out
}

func (h *Hub) pickLocked(req *pb.PlaceRequest, mem float64, owner string) sched.Result {
	return sched.Pick(h.params, h.candidatesLocked(req.Repo, owner), sched.Request{MemGB: mem, Require: req.Require, Exclude: req.Exclude})
}
