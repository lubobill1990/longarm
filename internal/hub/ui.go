package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/sched"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// UI additions (P1.5): job requests relayed to workers over their hub
// session (kill, logs, submit, toolchain apply) for the web panel and
// longarm admin; longarmd leave; per-client link reports; a short metrics history
// per worker for sparklines.

// historyLen is ~5 min of metrics at the default 2 s heartbeat.
const historyLen = 150

// requestTimeout bounds a request relayed to a worker.
var requestTimeout = 90 * time.Second

func newRequestID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// recordMetricsLocked appends a metrics sample to n's history.
func recordMetricsLocked(n *node, m *pb.MetricsSnapshot) {
	p := &pb.MetricsPoint{Time: m.Time, Cpu: m.CpuPercent}
	if p.Time == nil {
		p.Time = timestamppb.Now()
	}
	if m.MemTotalBytes > 0 {
		p.Mem = 100 * float64(m.MemUsedBytes) / float64(m.MemTotalBytes)
	}
	n.history = append(n.history, p)
	if len(n.history) > historyLen {
		n.history = append(n.history[:0:0], n.history[len(n.history)-historyLen:]...)
	}
}

// History returns the metrics history of one or all workers.
func (h *Hub) History(node string) *pb.HistoryResponse {
	return h.HistoryFor(tenancy.Caller{Admin: true}, node)
}

// HistoryFor is History of the workers v may see.
func (h *Hub) HistoryFor(v tenancy.Caller, node string) *pb.HistoryResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := &pb.HistoryResponse{}
	for name, n := range h.nodes {
		if (node != "" && name != node) || n.rec.Role != string(pki.RoleWorker) {
			continue
		}
		if !v.Admin && !h.mayUseLocked(v.User, n) {
			continue
		}
		out.Nodes = append(out.Nodes, &pb.NodeHistory{Node: name, Points: append([]*pb.MetricsPoint(nil), n.history...)})
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].Node < out.Nodes[j].Node })
	return out
}

// request sends cmd (built with a fresh request id) to worker's session
// and waits for the reply.
func (h *Hub) request(ctx context.Context, worker string, build func(id string) *pb.HubCommand) (*pb.HubReply, error) {
	id := newRequestID()
	ch := make(chan *pb.HubReply, 1)
	h.mu.Lock()
	n := h.nodes[worker]
	if n == nil || n.rec.Role != string(pki.RoleWorker) {
		h.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "no worker %q", worker)
	}
	if n.sess == nil {
		h.mu.Unlock()
		return nil, status.Errorf(codes.Unavailable, "worker %s is offline", worker)
	}
	sess := n.sess
	h.pending[id] = &pendingReq{ch: ch, worker: worker, sess: sess.id}
	h.sendLocked(n, build(id))
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	select {
	case r := <-ch:
		if r == nil {
			return nil, status.Errorf(codes.Unavailable, "worker %s disconnected", worker)
		}
		if r.Error != "" {
			code := codes.Code(r.Code)
			if code == codes.OK {
				code = codes.Unknown
			}
			return nil, status.Error(code, r.Error)
		}
		return r, nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

type pendingReq struct {
	ch     chan *pb.HubReply
	worker string
	sess   uint64
}

// deliverReplyLocked hands a worker's reply to the waiting request.
func (h *Hub) deliverReplyLocked(n *node, sess *nodeSession, r *pb.HubReply) {
	p := h.pending[r.RequestId]
	if p == nil || p.worker != n.rec.Name || p.sess != sess.id {
		return // unknown, or from another worker: ignore
	}
	delete(h.pending, r.RequestId)
	p.ch <- r
}

// failPendingLocked ends the requests waiting on a closed session.
func (h *Hub) failPendingLocked(worker string, sessID uint64) {
	for id, p := range h.pending {
		if p.worker == worker && p.sess == sessID {
			delete(h.pending, id)
			p.ch <- nil
		}
	}
}

// KillJob kills a job through its worker (user: the caller's, see relayCheck).
func (h *Hub) KillJob(ctx context.Context, by, user, id, worker string) (*pb.FleetJob, error) {
	w, err := h.jobWorker(id, worker)
	if err != nil {
		return nil, err
	}
	if err := h.relayCheck(by, user, w, "kill jobs", id); err != nil {
		return nil, err
	}
	r, err := h.request(ctx, w, func(rid string) *pb.HubCommand {
		return &pb.HubCommand{Msg: &pb.HubCommand_KillJob{KillJob: &pb.HubJobKill{RequestId: rid, Id: id, By: by}}}
	})
	if err != nil {
		return nil, err
	}
	h.log.Info("job killed via hub", "job", id, "worker", w, "by", by)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.applyJobsLocked(h.nodes[w], &pb.JobsReport{Jobs: []*pb.Job{r.Job}})
	return &pb.FleetJob{Worker: w, Job: r.Job, UpdatedAt: timestamppb.Now(), WorkerOnline: true}, nil
}

// jobWorker finds the worker of a job (worker may be given).
func (h *Hub) jobWorker(id, worker string) (string, error) {
	if id == "" {
		return "", status.Error(codes.InvalidArgument, "missing job id")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	w, e := h.findJobLocked(id, worker)
	if e == nil {
		if worker != "" {
			return worker, nil // not indexed (yet): ask the worker anyway
		}
		return "", status.Errorf(codes.NotFound, "job %s not known to the hub", id)
	}
	return w, nil
}

// JobLogs reads a job's output through its worker, polling while follow.
func (h *Hub) JobLogs(ctx context.Context, by, user string, req *pb.HubLogsRequest, send func(*pb.LogChunk) error) error {
	w, err := h.jobWorker(req.Id, req.Worker)
	if err != nil {
		return err
	}
	off := req.Offset
	idle := 250 * time.Millisecond
	for {
		// every poll: a followed stream ends once the worker is unshared
		if err := h.relayCheck(by, user, w, "read logs", req.Id); err != nil {
			return err
		}
		r, err := h.request(ctx, w, func(rid string) *pb.HubCommand {
			return &pb.HubCommand{Msg: &pb.HubCommand_LogRequest{LogRequest: &pb.HubLogRequest{RequestId: rid, Id: req.Id, Offset: off, MaxBytes: 256 << 10, By: by}}}
		})
		if err != nil {
			return err
		}
		lc := r.Log
		if lc == nil {
			lc = &pb.LogChunk{Offset: off}
		}
		off = lc.Offset + int64(len(lc.Data))
		if len(lc.Data) > 0 {
			if err := send(lc); err != nil {
				return err
			}
			idle = 250 * time.Millisecond
			if off < r.LogSize {
				continue // more is there already
			}
		}
		if r.LogEof || !req.Follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(idle):
		}
		if idle < 2*time.Second {
			idle += 250 * time.Millisecond
		}
	}
}

// SubmitJob places (worker "" / "auto") and submits a command job through
// the worker's hub session.
func (h *Hub) SubmitJob(ctx context.Context, by string, req *pb.HubSubmitRequest) (*pb.FleetJob, error) {
	return h.SubmitJobAs(ctx, by, tenancy.OwnerFromBy(by), true, req)
}

// SubmitJobAs is SubmitJob for a job of owner (it runs as owner on workers
// with per-user isolation, within the owner's quota); admin: the caller is
// an admin (refusals on others' own workers say so instead of "not found").
func (h *Hub) SubmitJobAs(ctx context.Context, by, owner string, admin bool, req *pb.HubSubmitRequest) (*pb.FleetJob, error) {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return nil, status.Error(codes.InvalidArgument, "missing command")
	}
	id := ulid.Make().String()
	worker := req.Worker
	labels := map[string]string{}
	for k, v := range req.Labels {
		labels[k] = v
	}
	typ := sched.TypeOf(req.Argv)
	mem := h.params.MemFor(typ, req.MemGb)
	labels[sched.LabelMemGB] = strconv.FormatFloat(mem, 'f', -1, 64)
	labels["longarm.via"] = "hub"
	if worker == "" || worker == "auto" {
		var assigned string
		err := h.PlaceFor(ctx, by, owner, &pb.PlaceRequest{JobId: id, MemGb: req.MemGb, Type: typ, Require: req.Require, NoQueue: true},
			func(u *pb.PlaceUpdate) error {
				if u.State == "assigned" {
					assigned = u.Worker
				}
				return nil
			})
		if err != nil {
			return nil, err
		}
		if assigned == "" {
			return nil, status.Error(codes.ResourceExhausted, "no worker available")
		}
		worker = assigned
	}
	spec := &pb.JobSpec{Id: id, Name: req.Name, Argv: req.Argv, Cwd: req.Cwd, Env: req.Env, Labels: labels}
	if req.Worker != "" && req.Worker != "auto" { // placed jobs passed the quota already
		if err := h.relayCheck(by, owner, worker, "submit jobs", id); err != nil {
			if !admin { // others' own workers don't exist for users (admins see them)
				return nil, status.Errorf(codes.NotFound, "no worker %q", worker)
			}
			return nil, err
		}
		h.mu.Lock()
		if h.nodes[worker] == nil {
			h.mu.Unlock()
			return nil, status.Errorf(codes.NotFound, "no worker %q", worker)
		}
		why := h.overQuotaLocked(owner, mem)
		h.mu.Unlock()
		if why != "" {
			return nil, status.Error(codes.ResourceExhausted, why)
		}
	}
	r, err := h.request(ctx, worker, func(rid string) *pb.HubCommand {
		return &pb.HubCommand{Msg: &pb.HubCommand_SubmitJob{SubmitJob: &pb.HubJobSubmit{RequestId: rid, Spec: spec, By: by, Owner: owner}}}
	})
	if err != nil {
		h.mu.Lock()
		delete(h.reservations, id)
		h.mu.Unlock()
		return nil, err
	}
	h.log.Info("job submitted via hub", "job", id, "worker", worker, "by", by, "argv", req.Argv)
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := h.nodes[worker]; n != nil {
		h.applyJobsLocked(n, &pb.JobsReport{Jobs: []*pb.Job{r.Job}})
	}
	return &pb.FleetJob{Worker: worker, Job: r.Job, UpdatedAt: timestamppb.Now(), WorkerOnline: true}, nil
}

// ToolchainApply asks a worker to reconcile its profile now.
func (h *Hub) ToolchainApply(ctx context.Context, by, worker string) (*pb.NodeInfo, error) {
	if _, err := h.request(ctx, worker, func(rid string) *pb.HubCommand {
		return &pb.HubCommand{Msg: &pb.HubCommand_ToolchainApply{ToolchainApply: &pb.HubToolchainApply{RequestId: rid, By: by}}}
	}); err != nil {
		return nil, err
	}
	h.log.Info("toolchain apply requested", "node", worker, "by", by)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nodeInfoLocked(h.nodes[worker]), nil
}

// ---------------------------------------------------------------- links

// linkTTL drops reports of clients that stopped reporting.
const linkTTL = 2 * time.Minute

// ReportLinks stores a client's link report (history stripped).
func (h *Hub) ReportLinks(client string, rep *pb.LinkReport) {
	r := proto.Clone(rep).(*pb.LinkReport)
	r.Client = client
	if r.Time == nil {
		r.Time = timestamppb.Now()
	}
	if len(r.Links) > 256 {
		r.Links = r.Links[:256]
	}
	for _, l := range r.Links {
		if len(l.Legs) > 8 {
			l.Legs = l.Legs[:8]
		}
		for _, leg := range l.Legs {
			leg.History = nil
		}
	}
	h.mu.Lock()
	h.links[client] = &linkEntry{rep: r, at: time.Now()}
	h.mu.Unlock()
	h.bus.publish(&pb.FleetEvent{Kind: "links", Node: client, Message: "links reported"})
}

type linkEntry struct {
	rep *pb.LinkReport
	at  time.Time
}

// Links returns the fresh link reports.
func (h *Hub) Links() *pb.LinksResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := &pb.LinksResponse{}
	for c, e := range h.links {
		if time.Since(e.at) > linkTTL {
			delete(h.links, c)
			continue
		}
		out.Reports = append(out.Reports, e.rep)
	}
	sort.Slice(out.Reports, func(i, j int) bool { return out.Reports[i].Client < out.Reports[j].Client })
	return out
}

// ---------------------------------------------------------------- gRPC

func (s *hubAdminServer) KillJob(ctx context.Context, req *pb.FleetJobRequest) (*pb.FleetJob, error) {
	fj, err := s.h.KillJob(ctx, by(ctx), callerFrom(ctx).id.Owner(), req.Id, req.Worker)
	if err != nil {
		return nil, err
	}
	if !canSeeJob(s.h.viewer(callerFrom(ctx)), fj.Job) { // killed by id: not a way to read others' jobs
		fj = &pb.FleetJob{Worker: fj.Worker, Job: &pb.Job{Spec: &pb.JobSpec{Id: req.Id}, State: fj.Job.GetState()},
			UpdatedAt: fj.UpdatedAt, WorkerOnline: fj.WorkerOnline}
	}
	return fj, nil
}

func (s *hubAdminServer) JobLogs(req *pb.HubLogsRequest, st pb.HubAdmin_JobLogsServer) error {
	// a job's output is read as its reader: admins included (their own and
	// team-shared jobs; others' jobs are "not known", as on GetJob)
	if _, err := s.h.GetJobFor(s.h.viewer(callerFrom(st.Context())), req.Id, req.Worker); err != nil {
		return err
	}
	return s.h.JobLogs(st.Context(), by(st.Context()), callerFrom(st.Context()).id.Owner(), req, st.Send)
}

func (s *hubAdminServer) SubmitJob(ctx context.Context, req *pb.HubSubmitRequest) (*pb.FleetJob, error) {
	return s.h.SubmitJobAs(ctx, by(ctx), callerFrom(ctx).id.Owner(), true, req)
}

func (s *hubAdminServer) ToolchainApply(ctx context.Context, req *pb.NodeRequest) (*pb.NodeInfo, error) {
	return s.h.opResult(s.h.viewer(callerFrom(ctx)))(s.h.ToolchainApply(ctx, by(ctx), req.Name))
}

func (s *fleetServer) ReportLinks(ctx context.Context, req *pb.LinkReport) (*pb.ReportLinksResponse, error) {
	s.h.ReportLinks(callerFrom(ctx).id.Name, req)
	return &pb.ReportLinksResponse{}, nil
}

func (s *fleetServer) Links(ctx context.Context, _ *pb.LinksRequest) (*pb.LinksResponse, error) {
	return s.h.LinksFor(s.h.viewer(callerFrom(ctx))), nil
}

func (s *fleetServer) History(ctx context.Context, req *pb.HistoryRequest) (*pb.HistoryResponse, error) {
	return s.h.HistoryFor(s.h.viewer(callerFrom(ctx)), req.Node), nil
}
