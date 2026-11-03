package hub

import (
	"context"
	"maps"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func (h *Hub) WorkerToolchain(ctx context.Context, by, user string, admin bool, req *pb.WorkerToolchainRequest) (*pb.WorkerToolchainResponse, error) {
	if req.Clear && (req.SetTools || len(req.Tools) > 0) || !req.SetTools && len(req.Tools) > 0 {
		return nil, status.Error(codes.InvalidArgument, "use set_tools or clear, not both")
	}
	if err := validateTools(req.Tools); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	h.mu.Lock()
	n, err := h.ownedWorkerLocked(req.Worker, user, admin)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	if n.rec.Admin && (req.SetTools || req.Clear || req.Apply) {
		h.mu.Unlock()
		return nil, status.Error(codes.PermissionDenied, "admin workers take no hub-initiated package operations; install tools with explicit commands")
	}
	id, name := n.rec.ID, n.rec.Name
	if req.SetTools || req.Clear {
		tools := map[string]string{}
		for _, t := range req.Tools {
			tools[t.Name] = t.Version
		}
		if !maps.Equal(n.rec.OwnerTools, tools) {
			rec := *n.rec
			rec.OwnerTools, rec.OwnerToolRevision = tools, rec.OwnerToolRevision+1
			if err := h.store.PutNode(&rec); err != nil {
				h.mu.Unlock()
				return nil, status.Error(codes.Internal, err.Error())
			}
			n.rec = &rec
			h.sendLocked(n, &pb.HubCommand{Msg: &pb.HubCommand_Toolchain{Toolchain: h.desiredToolchainLocked(n)}})
			h.publishNodeLocked(n, "owner toolchain changed by "+by)
		}
	}
	h.mu.Unlock()
	if req.SetTools || req.Clear || req.Apply {
		h.auditHub(by, "workers/toolchain", map[string]any{"worker": name, "id": id, "tools": req.Tools, "clear": req.Clear, "apply": req.Apply})
	}
	if req.Apply {
		if _, err := h.ToolchainApply(ctx, by, "id:"+id); err != nil {
			return nil, err // desired tools remain stored even if the worker is offline
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n = h.nodes[id]
	if n == nil {
		return nil, status.Error(codes.NotFound, "worker removed")
	}
	resp := &pb.WorkerToolchainResponse{Worker: n.rec.Name, WorkerId: id,
		OwnerTools: toolSpecs(n.rec.OwnerTools), Desired: h.desiredToolchainLocked(n)}
	if n.toolchain != nil {
		resp.Toolchain = proto.Clone(n.toolchain).(*pb.ToolchainReport)
	}
	return resp, nil
}

func (s *fleetServer) WorkerToolchain(ctx context.Context, req *pb.WorkerToolchainRequest) (*pb.WorkerToolchainResponse, error) {
	c := callerFrom(ctx)
	return s.h.WorkerToolchain(ctx, c.id.String(), c.user, c.id.Role == pki.RoleAdmin, req)
}
