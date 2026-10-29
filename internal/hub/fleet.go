package hub

import (
	"context"
	"slices"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type fleetServer struct {
	pb.UnimplementedFleetServer
	h *Hub
}

// ListNodes returns workers (all = every node, sorted by name).
func (h *Hub) ListNodes(all bool) []*pb.NodeInfo {
	return h.ListNodesFor(tenancy.Caller{Admin: true}, all)
}

// ListNodesFor is ListNodes as v may see it: other users' devices, process
// lists and mirrors are left out for non-admins.
func (h *Hub) ListNodesFor(v tenancy.Caller, all bool) []*pb.NodeInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*pb.NodeInfo
	for _, n := range h.nodes {
		if !all && n.rec.Role != string(pki.RoleWorker) { // revoked workers too: clients must stop using them
			continue
		}
		info := h.nodeInfoLocked(n)
		if !canSeeNode(v, info) { // other users' devices and workers
			continue
		}
		out = append(out, nodeView(v, info))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *fleetServer) ListNodes(ctx context.Context, req *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	return &pb.ListNodesResponse{Nodes: s.h.ListNodesFor(s.h.viewer(callerFrom(ctx)), req.All)}, nil
}

// ListJobs returns cached jobs, newest first.
func (h *Hub) ListJobs(req *pb.FleetJobsRequest) []*pb.FleetJob {
	return h.ListJobsFor(tenancy.Caller{Admin: true}, req)
}

// ListJobsFor is ListJobs as v may see it (own and team-shared jobs).
func (h *Hub) ListJobsFor(v tenancy.Caller, req *pb.FleetJobsRequest) []*pb.FleetJob {
	p := h.projector(v)
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*pb.FleetJob
	for w, jm := range h.jobs {
		if req.Worker != "" && w != req.Worker {
			continue
		}
		for _, e := range jm {
			if len(req.States) > 0 && !slices.Contains(req.States, e.job.State) {
				continue
			}
			if !canSeeJob(v, e.job) {
				continue
			}
			out = append(out, p.fleetJob(h.fleetJobLocked(w, e)))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Job.GetCreatedAt().AsTime(), out[j].Job.GetCreatedAt().AsTime()
		if !a.Equal(b) {
			return a.After(b)
		}
		return out[i].Job.GetSpec().GetId() > out[j].Job.GetSpec().GetId()
	})
	if req.Limit > 0 && len(out) > int(req.Limit) {
		out = out[:req.Limit]
	}
	return out
}

func (s *fleetServer) ListJobs(ctx context.Context, req *pb.FleetJobsRequest) (*pb.FleetJobsResponse, error) {
	return &pb.FleetJobsResponse{Jobs: s.h.ListJobsFor(s.h.viewer(callerFrom(ctx)), req)}, nil
}

// GetJob finds a job.
func (h *Hub) GetJob(id, worker string) (*pb.FleetJob, error) {
	return h.GetJobFor(tenancy.Caller{Admin: true}, id, worker)
}

// GetJobFor finds a job v may see (others' jobs are "not known").
func (h *Hub) GetJobFor(v tenancy.Caller, id, worker string) (*pb.FleetJob, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w, e := h.findJobLocked(id, worker)
	if e == nil || !canSeeJob(v, e.job) {
		return nil, status.Errorf(codes.NotFound, "job %s not known to the hub", id)
	}
	return h.projector(v).fleetJob(h.fleetJobLocked(w, e)), nil
}

func (s *fleetServer) GetJob(ctx context.Context, req *pb.FleetJobRequest) (*pb.FleetJob, error) {
	return s.h.GetJobFor(s.h.viewer(callerFrom(ctx)), req.Id, req.Worker)
}

// waitAppear is how long WaitJob waits for an unknown job to be reported.
var waitAppear = 15 * time.Second

// WaitJob streams a job's snapshots until it is terminal.
func (h *Hub) WaitJob(ctx context.Context, req *pb.FleetJobRequest, send func(*pb.FleetJob) error) error {
	return h.WaitJobFor(ctx, tenancy.Caller{Admin: true}, req, send)
}

// WaitJobFor is WaitJob for a job v may see.
func (h *Hub) WaitJobFor(ctx context.Context, v tenancy.Caller, req *pb.FleetJobRequest, send func(*pb.FleetJob) error) error {
	sub := h.bus.subscribeFiltered([]string{"job"}, req.Id, h.eventFilter(v))
	defer h.bus.unsubscribe(sub)
	fj, err := h.GetJobFor(v, req.Id, req.Worker)
	if err == nil {
		if err := send(fj); err != nil {
			return err
		}
		if terminal(fj.Job.State) {
			return nil
		}
	}
	var appear <-chan time.Time
	if fj == nil {
		appear = time.After(waitAppear)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-appear:
			return status.Errorf(codes.NotFound, "job %s not known to the hub", req.Id)
		case ev, ok := <-sub.ch:
			if !ok {
				return status.Error(codes.Unavailable, "hub event stream closed")
			}
			if req.Worker != "" && ev.Job.Worker != req.Worker {
				continue
			}
			appear = nil
			if err := send(ev.Job); err != nil {
				return err
			}
			if terminal(ev.Job.Job.State) {
				return nil
			}
		}
	}
}

func (s *fleetServer) WaitJob(req *pb.FleetJobRequest, st pb.Fleet_WaitJobServer) error {
	return s.h.WaitJobFor(st.Context(), s.h.viewer(callerFrom(st.Context())), req, st.Send)
}

// Events streams fleet events. "metrics" events (one per worker every 2 s)
// are only sent when asked for explicitly.
func (h *Hub) Events(ctx context.Context, req *pb.EventsRequest, send func(*pb.FleetEvent) error) error {
	return h.EventsFor(ctx, tenancy.Caller{Admin: true}, req, send)
}

// EventsFor streams the events v may see (eventFilter).
func (h *Hub) EventsFor(ctx context.Context, v tenancy.Caller, req *pb.EventsRequest, send func(*pb.FleetEvent) error) error {
	kinds := req.Kinds
	if len(kinds) == 0 {
		kinds = []string{"job", "node", "queue"}
	}
	sub := h.bus.subscribeFiltered(kinds, req.JobId, h.eventFilter(v))
	defer h.bus.unsubscribe(sub)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-sub.ch:
			if !ok {
				return status.Error(codes.Unavailable, "hub event stream closed")
			}
			if err := send(ev); err != nil {
				return err
			}
		}
	}
}

func (s *fleetServer) Events(req *pb.EventsRequest, st pb.Fleet_EventsServer) error {
	return s.h.EventsFor(st.Context(), s.h.viewer(callerFrom(st.Context())), req, st.Send)
}

func (s *fleetServer) Place(req *pb.PlaceRequest, st pb.Fleet_PlaceServer) error {
	c := callerFrom(st.Context())
	return s.h.PlaceFor(st.Context(), c.id.String(), c.user, req, st.Send)
}

// Audit returns forwarded audit records, all of them (the hub's own
// callers; the read APIs use AuditFor).
func (h *Hub) Audit(req *pb.AuditRequest) (*pb.AuditResponse, error) {
	return h.AuditFor(tenancy.Caller{Admin: true}, "", req)
}

func (s *fleetServer) Audit(ctx context.Context, req *pb.AuditRequest) (*pb.AuditResponse, error) {
	c := callerFrom(ctx)
	return s.h.AuditFor(s.h.viewer(c), c.id.String(), req)
}

func (s *fleetServer) Whoami(ctx context.Context, req *pb.WhoamiRequest) (*pb.WhoamiResponse, error) {
	c := callerFrom(ctx)
	resp := &pb.WhoamiResponse{Name: c.id.Name, Role: string(c.id.Role), HubName: s.h.cfg.Name, HubVersion: version.Version,
		CertNotAfter: tspb(c.cert.NotAfter), RenewBefore: durationpb.New(s.h.cfg.RenewBefore(string(c.id.Role))),
		Tailnet: s.h.callerTailnet(c.id.Name), User: c.user, PublicUrl: s.h.cfg.PublicURL}
	latest, files := s.h.latestNotice()
	resp.LatestVersion, resp.LatestPath = latest, latestPath(latest, files, req.GetOs(), req.GetArch())
	s.h.mu.Lock()
	if resp.Tenancy = s.h.tenancyOnLocked(); resp.Tenancy && c.id.Role != pki.RoleWorker {
		if t := s.h.tenants[c.user]; t != nil {
			resp.Teams, resp.CanShare, resp.Upn = t.u.Teams, t.u.CanShare, t.u.Upn
		}
		if q := s.h.quotaLocked(c.user); !tenancy.QuotaEmpty(q) {
			resp.Quota = q
		}
		resp.Usage = s.h.usageLocked(c.user)
	}
	s.h.mu.Unlock()
	return resp, nil
}
