package hub

import (
	"context"
	"maps"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/sched"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type hubAdminServer struct {
	pb.UnimplementedHubAdminServer
	h *Hub
}

func by(ctx context.Context) string { return callerFrom(ctx).id.String() }

func (s *hubAdminServer) CreateJoinCode(ctx context.Context, req *pb.CreateJoinCodeRequest) (*pb.JoinCode, error) {
	return s.h.CreateJoinCode(by(ctx), req)
}

func (s *hubAdminServer) ListJoinCodes(ctx context.Context, _ *pb.ListJoinCodesRequest) (*pb.ListJoinCodesResponse, error) {
	return s.h.ListJoinCodesFor(s.h.viewer(callerFrom(ctx)), by(ctx))
}

// Operations on a node by name: the admin must know the name (the hub
// doesn't list other users' nodes to admins); the answer carries no more
// than the admin's user may see of the node, and other users' machines are
// "no node" (opOn).

func (s *hubAdminServer) Approve(ctx context.Context, req *pb.NodeRequest) (*pb.NodeInfo, error) {
	return s.h.opOn(s.h.viewer(callerFrom(ctx)), "node", req.Name, func(t string) (*pb.NodeInfo, error) { return s.h.Approve(by(ctx), t) })
}

func (s *hubAdminServer) Revoke(ctx context.Context, req *pb.NodeRequest) (*pb.NodeInfo, error) {
	return s.h.opOn(s.h.viewer(callerFrom(ctx)), "node", req.Name, func(t string) (*pb.NodeInfo, error) { return s.h.Revoke(by(ctx), t) })
}

func (s *hubAdminServer) SetDrain(ctx context.Context, req *pb.SetDrainRequest) (*pb.NodeInfo, error) {
	return s.h.opOn(s.h.viewer(callerFrom(ctx)), "node", req.Name, func(t string) (*pb.NodeInfo, error) { return s.h.SetDrain(by(ctx), t, req.Draining) })
}

func (s *hubAdminServer) SetLabels(ctx context.Context, req *pb.SetLabelsRequest) (*pb.NodeInfo, error) {
	return s.h.opOn(s.h.viewer(callerFrom(ctx)), "node", req.Name, func(t string) (*pb.NodeInfo, error) { return s.h.SetLabels(by(ctx), t, req.Labels) })
}

func (s *hubAdminServer) PutProfile(ctx context.Context, req *pb.Profile) (*pb.Profile, error) {
	pr, err := s.h.PutProfile(by(ctx), req)
	return s.h.projector(s.h.viewer(callerFrom(ctx))).profile(pr), err
}

func (s *hubAdminServer) ListProfiles(ctx context.Context, _ *pb.ListProfilesRequest) (*pb.ListProfilesResponse, error) {
	return &pb.ListProfilesResponse{Profiles: s.h.ProfilesFor(s.h.viewer(callerFrom(ctx)))}, nil
}

func (s *hubAdminServer) AssignProfile(ctx context.Context, req *pb.AssignProfileRequest) (*pb.NodeInfo, error) {
	return s.h.opOn(s.h.viewer(callerFrom(ctx)), "node", req.Node, func(t string) (*pb.NodeInfo, error) { return s.h.AssignProfile(by(ctx), t, req.Profile) })
}

func (s *hubAdminServer) RenameNode(ctx context.Context, req *pb.RenameNodeRequest) (*pb.NodeInfo, error) {
	c := callerFrom(ctx)
	v := s.h.viewer(c)
	info, err := s.h.RenameNode(c.id.String(), v, true, req.Target, req.NewName)
	return opNodeView(v, info), err
}

// ListJoinCodes lists recent join codes (without the codes themselves).
func (h *Hub) ListJoinCodes() (*pb.ListJoinCodesResponse, error) {
	return h.ListJoinCodesFor(tenancy.Caller{Admin: true}, "")
}

// ListJoinCodesFor lists the join codes v may see: the codes the caller
// (by) created, codes of v's own user (its devices and workers) and fleet
// worker codes (no owner). used_by - the machine that enrolled with it -
// reads "used" unless v may see that machine.
func (h *Hub) ListJoinCodesFor(v tenancy.Caller, by string) (*pb.ListJoinCodesResponse, error) {
	codes_, err := h.store.JoinCodes(time.Now())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &pb.ListJoinCodesResponse{}
	p := h.projector(v) // used_by "used" unless visible, created_by as its user
	for _, c := range codes_ {
		ok := v.Admin || (by != "" && c.CreatedBy == by) || (v.User != "" && (c.Owner == v.User || c.User == v.User)) ||
			(c.Role == string(pki.RoleWorker) && c.Owner == "")
		if !ok {
			continue
		}
		out.Codes = append(out.Codes, p.joinCode(joinCodePB(c)))
	}
	return out, nil
}

// nodeOp resolves a node target (name, ID, ID prefix), applies fn under
// the lock, saves and publishes it.
func (h *Hub) nodeOp(target, what string, fn func(n *node) error) (*pb.NodeInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, err := h.resolveLocked(target, nil, "node")
	if err != nil {
		return nil, err
	}
	if err := fn(n); err != nil {
		return nil, err
	}
	if err := h.store.PutNode(n.rec); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	h.publishNodeLocked(n, what)
	return h.nodeInfoLocked(n), nil
}

// SetDrain sets the admin drain flag: the scheduler sends the worker
// nothing new; running jobs continue.
func (h *Hub) SetDrain(by, name string, draining bool) (*pb.NodeInfo, error) {
	what := "undrained by " + by
	if draining {
		what = "drained by " + by
	}
	info, err := h.nodeOp(name, what, func(n *node) error {
		n.rec.AdminDrain = draining
		h.sendLocked(n, &pb.HubCommand{Msg: &pb.HubCommand_Drain{Drain: &pb.DrainCommand{Draining: draining}}})
		return nil
	})
	if err == nil {
		h.log.Info("drain", "node", name, "draining", draining, "by", by)
		h.kickQueue()
	}
	return info, err
}

// SetLabels merges labels into the node's admin labels ("" deletes).
func (h *Hub) SetLabels(by, name string, labels map[string]string) (*pb.NodeInfo, error) {
	info, err := h.nodeOp(name, "labels changed by "+by, func(n *node) error {
		m := maps.Clone(n.rec.Labels)
		if m == nil {
			m = map[string]string{}
		}
		if err := sched.ValidateLabels(labels, true); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		// longarm.io/... (the admin worker mark) is the hub's: nobody sets
		// or removes it, admins included
		if err := sched.CheckUserLabels(labels); err != nil {
			return status.Error(codes.PermissionDenied, err.Error())
		}
		for k, v := range labels {
			if v == "" {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		n.rec.Labels = m
		return nil
	})
	if err == nil {
		h.kickQueue()
	}
	return info, err
}
