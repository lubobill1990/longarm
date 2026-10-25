package hub

import (
	"context"
	"maps"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/sched"
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

func (s *hubAdminServer) ListJoinCodes(context.Context, *pb.ListJoinCodesRequest) (*pb.ListJoinCodesResponse, error) {
	return s.h.ListJoinCodes()
}

func (s *hubAdminServer) Approve(ctx context.Context, req *pb.NodeRequest) (*pb.NodeInfo, error) {
	return s.h.Approve(by(ctx), req.Name)
}

func (s *hubAdminServer) Revoke(ctx context.Context, req *pb.NodeRequest) (*pb.NodeInfo, error) {
	return s.h.Revoke(by(ctx), req.Name)
}

func (s *hubAdminServer) SetDrain(ctx context.Context, req *pb.SetDrainRequest) (*pb.NodeInfo, error) {
	return s.h.SetDrain(by(ctx), req.Name, req.Draining)
}

func (s *hubAdminServer) SetLabels(ctx context.Context, req *pb.SetLabelsRequest) (*pb.NodeInfo, error) {
	return s.h.SetLabels(by(ctx), req.Name, req.Labels)
}

func (s *hubAdminServer) PutProfile(ctx context.Context, req *pb.Profile) (*pb.Profile, error) {
	return s.h.PutProfile(by(ctx), req)
}

func (s *hubAdminServer) ListProfiles(context.Context, *pb.ListProfilesRequest) (*pb.ListProfilesResponse, error) {
	return &pb.ListProfilesResponse{Profiles: s.h.Profiles()}, nil
}

func (s *hubAdminServer) AssignProfile(ctx context.Context, req *pb.AssignProfileRequest) (*pb.NodeInfo, error) {
	return s.h.AssignProfile(by(ctx), req.Node, req.Profile)
}

// ListJoinCodes lists recent join codes (without the codes themselves).
func (h *Hub) ListJoinCodes() (*pb.ListJoinCodesResponse, error) {
	codes_, err := h.store.JoinCodes(time.Now())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &pb.ListJoinCodesResponse{}
	for _, c := range codes_ {
		out.Codes = append(out.Codes, joinCodePB(c))
	}
	return out, nil
}

// nodeOp loads a node, applies fn under the lock, saves and publishes it.
func (h *Hub) nodeOp(name, what string, fn func(n *node) error) (*pb.NodeInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.nodes[name]
	if n == nil {
		return nil, status.Errorf(codes.NotFound, "no node %q", name)
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
