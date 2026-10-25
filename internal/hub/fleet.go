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
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type fleetServer struct {
	pb.UnimplementedFleetServer
	h *Hub
}

// ListNodes returns workers (all = every node, sorted by name).
func (h *Hub) ListNodes(all bool) []*pb.NodeInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*pb.NodeInfo
	for _, n := range h.nodes {
		if !all && n.rec.Role != string(pki.RoleWorker) { // revoked workers too: clients must stop using them
			continue
		}
		out = append(out, h.nodeInfoLocked(n))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *fleetServer) ListNodes(_ context.Context, req *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	return &pb.ListNodesResponse{Nodes: s.h.ListNodes(req.All)}, nil
}

// ListJobs returns cached jobs, newest first.
func (h *Hub) ListJobs(req *pb.FleetJobsRequest) []*pb.FleetJob {
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
			out = append(out, h.fleetJobLocked(w, e))
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

func (s *fleetServer) ListJobs(_ context.Context, req *pb.FleetJobsRequest) (*pb.FleetJobsResponse, error) {
	return &pb.FleetJobsResponse{Jobs: s.h.ListJobs(req)}, nil
}

// GetJob finds a job.
func (h *Hub) GetJob(id, worker string) (*pb.FleetJob, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w, e := h.findJobLocked(id, worker)
	if e == nil {
		return nil, status.Errorf(codes.NotFound, "job %s not known to the hub", id)
	}
	return h.fleetJobLocked(w, e), nil
}

func (s *fleetServer) GetJob(_ context.Context, req *pb.FleetJobRequest) (*pb.FleetJob, error) {
	return s.h.GetJob(req.Id, req.Worker)
}

// waitAppear is how long WaitJob waits for an unknown job to be reported.
var waitAppear = 15 * time.Second

// WaitJob streams a job's snapshots until it is terminal.
func (h *Hub) WaitJob(ctx context.Context, req *pb.FleetJobRequest, send func(*pb.FleetJob) error) error {
	sub := h.bus.subscribe([]string{"job"}, req.Id)
	defer h.bus.unsubscribe(sub)
	fj, err := h.GetJob(req.Id, req.Worker)
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
	return s.h.WaitJob(st.Context(), req, st.Send)
}

// Events streams fleet events. "metrics" events (one per worker every 2 s)
// are only sent when asked for explicitly.
func (h *Hub) Events(ctx context.Context, req *pb.EventsRequest, send func(*pb.FleetEvent) error) error {
	kinds := req.Kinds
	if len(kinds) == 0 {
		kinds = []string{"job", "node", "queue"}
	}
	sub := h.bus.subscribe(kinds, req.JobId)
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
	return s.h.Events(st.Context(), req, st.Send)
}

func (s *fleetServer) Place(req *pb.PlaceRequest, st pb.Fleet_PlaceServer) error {
	return s.h.Place(st.Context(), callerFrom(st.Context()).id.String(), req, st.Send)
}

// Audit returns forwarded audit records.
func (h *Hub) Audit(req *pb.AuditRequest) (*pb.AuditResponse, error) {
	var since time.Time
	if req.Since != nil {
		since = req.Since.AsTime()
	}
	rows, err := h.store.Audit(req.Node, since, int(req.Limit))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &pb.AuditResponse{}
	for _, r := range rows {
		out.Records = append(out.Records, &pb.AuditRecord{Node: r.Node, Time: tspb(r.Time), Who: r.Who, Method: r.Method, Json: r.Line})
	}
	return out, nil
}

func (s *fleetServer) Audit(_ context.Context, req *pb.AuditRequest) (*pb.AuditResponse, error) {
	return s.h.Audit(req)
}

func (s *fleetServer) Whoami(ctx context.Context, _ *pb.WhoamiRequest) (*pb.WhoamiResponse, error) {
	c := callerFrom(ctx)
	return &pb.WhoamiResponse{Name: c.id.Name, Role: string(c.id.Role), HubName: s.h.cfg.Name, HubVersion: version.Version,
		CertNotAfter: tspb(c.cert.NotAfter), RenewBefore: durationpb.New(s.h.cfg.RenewBefore(string(c.id.Role))),
		Tailnet: s.h.callerTailnet(c.id.Name)}, nil
}
