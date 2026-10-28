package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/jobs"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// FeatureMachineUpdate: the worker serves MachineUpdate (longarm worker
// update, the tray's "Update to X").
const FeatureMachineUpdate = jobs.FeatureMachineUpdate

// MachineUpdater does what MachineUpdate asks (longarmd: the LongarmUpdate
// service of an MSI install, else longarmd's own self-update). by names the
// owner who asked.
type MachineUpdater interface {
	Start(ctx context.Context, by string, req *pb.MachineUpdateRequest, send func(*pb.MachineUpdateEvent) error) error
	Status(ctx context.Context) (*pb.MachineUpdateEvent, error)
	SetAuto(ctx context.Context, by string, req *pb.MachineUpdateAutoRequest) (*pb.MachineUpdateEvent, error)
}

type machineUpdateServer struct {
	pb.UnimplementedMachineUpdateServer
	s *Server
}

// machineOwner reports whether user owns this worker: an owner pinned in
// longarmd.toml ([access] owners - the admin worker's) or the owner of a
// user-owned worker. A fleet worker without owner has none: its updates
// are the hub's rollouts, not anybody's request.
func (s *Server) machineOwner(user string) bool {
	if user == "" {
		return false
	}
	if s.Access.IsOwner(user) {
		return true
	}
	o := s.WorkerAccess.Owner()
	return o != "" && o != "?" && o == user
}

// ownerOnly refuses everybody but an owner (admin certificates of other
// users too: owner-only means owner-only).
func (m *machineUpdateServer) ownerOnly(ctx context.Context, method string) (string, error) {
	id := IdentityFrom(ctx)
	if !m.s.machineOwner(id.Owner()) {
		m.s.Audit.Log(id, method, map[string]any{"denied": "not the owner"})
		return "", status.Errorf(codes.PermissionDenied, "only the owner of %s may update its machine", m.s.Name)
	}
	if m.s.MachineUpdate == nil {
		return "", status.Error(codes.Unimplemented, "this worker can't update its machine")
	}
	m.s.Audit.Log(id, method, nil)
	return id.Owner(), nil
}

func (m *machineUpdateServer) Start(req *pb.MachineUpdateRequest, st pb.MachineUpdate_StartServer) error {
	by, err := m.ownerOnly(st.Context(), pb.MachineUpdate_Start_FullMethodName)
	if err != nil {
		return err
	}
	if req.Latest == (req.Version != "") {
		return status.Error(codes.InvalidArgument, "a version or latest")
	}
	// what runs now: the caller counts what kept running across the update
	var running, sessions []string
	for _, j := range m.s.Spaces.ListJobs([]jobs.State{jobs.Preparing, jobs.Running}, 0) {
		running = append(running, j.Spec.ID)
	}
	for _, sp := range m.s.Spaces.All() {
		for _, j := range sp.Sessions.List([]jobs.State{jobs.Preparing, jobs.Running}, 0) {
			sessions = append(sessions, j.Spec.ID)
		}
	}
	first := true
	m.s.Log.Info("machine update requested", "by", by, "version", req.Version, "latest", req.Latest)
	return m.s.MachineUpdate.Start(st.Context(), by, req, func(ev *pb.MachineUpdateEvent) error {
		if first {
			ev.RunningJobs, ev.RunningSessions = running, sessions
			first = false
		}
		return st.Send(ev)
	})
}

func (m *machineUpdateServer) Status(ctx context.Context, _ *pb.MachineUpdateStatusRequest) (*pb.MachineUpdateEvent, error) {
	if m.s.MachineUpdate == nil {
		return nil, status.Error(codes.Unimplemented, "this worker can't update its machine")
	}
	return m.s.MachineUpdate.Status(ctx)
}

func (m *machineUpdateServer) SetAuto(ctx context.Context, req *pb.MachineUpdateAutoRequest) (*pb.MachineUpdateEvent, error) {
	by, err := m.ownerOnly(ctx, pb.MachineUpdate_SetAuto_FullMethodName)
	if err != nil {
		return nil, err
	}
	if req.IntervalHours < 0 || req.IntervalHours > 24*7 {
		return nil, status.Error(codes.InvalidArgument, "interval: 1 to 168 hours")
	}
	return m.s.MachineUpdate.SetAuto(ctx, by, req)
}
