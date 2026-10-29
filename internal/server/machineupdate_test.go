package server

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type fakeMachineUpdater struct {
	mu    sync.Mutex
	calls []string // "start <by>", "auto <by>"
}

func (f *fakeMachineUpdater) Start(_ context.Context, by string, req *pb.MachineUpdateRequest, send func(*pb.MachineUpdateEvent) error) error {
	f.mu.Lock()
	f.calls = append(f.calls, "start "+by)
	f.mu.Unlock()
	if err := send(&pb.MachineUpdateEvent{Phase: "checking"}); err != nil {
		return err
	}
	return send(&pb.MachineUpdateEvent{Phase: "installing", From: "0.1.8", To: "0.1.9", Method: "msi"})
}

func (f *fakeMachineUpdater) Status(context.Context) (*pb.MachineUpdateEvent, error) {
	return &pb.MachineUpdateEvent{Phase: "status", Installed: "0.1.8"}, nil
}

func (f *fakeMachineUpdater) SetAuto(_ context.Context, by string, _ *pb.MachineUpdateAutoRequest) (*pb.MachineUpdateEvent, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "auto "+by)
	f.mu.Unlock()
	return &pb.MachineUpdateEvent{Phase: "status", Auto: true}, nil
}

func startUpdate(t *testing.T, c *grpc.ClientConn) ([]*pb.MachineUpdateEvent, error) {
	st, err := pb.NewMachineUpdateClient(c).Start(cctx(t), &pb.MachineUpdateRequest{Latest: true})
	if err != nil {
		return nil, err
	}
	var evs []*pb.MachineUpdateEvent
	for {
		ev, err := st.Recv()
		if err == io.EOF {
			return evs, nil
		}
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
	}
}

// TestMachineUpdateOwnerOnly: only the worker's owner may update its
// machine or change automatic updates - not other users, not admin
// certificates of other users (the hub's admins), not a fleet worker's
// admins (no owner: rollouts only).
func TestMachineUpdateOwnerOnly(t *testing.T) {
	e := newAuthzEnv(t)
	f := &fakeMachineUpdater{}
	e.srv.MachineUpdate = f
	denied := func(err error) bool { return status.Code(err) == codes.PermissionDenied }
	setAuto := func(c *grpc.ClientConn) error {
		_, err := pb.NewMachineUpdateClient(c).SetAuto(cctx(t), &pb.MachineUpdateAutoRequest{Enabled: true})
		return err
	}

	// a fleet worker (no owner): nobody
	for name, c := range map[string]*grpc.ClientConn{"alice": e.alice, "root (admin)": e.root} {
		if _, err := startUpdate(t, c); !denied(err) {
			t.Errorf("fleet worker: %s started an update: %v", name, err)
		}
		if err := setAuto(c); !denied(err) {
			t.Errorf("fleet worker: %s set auto-update: %v", name, err)
		}
	}

	// alice's own worker
	wa, _ := tenancy.LoadWorkerAccess("")
	wa.Set(&pb.WorkerAccess{Version: 1, Owner: "alice", Shares: &pb.WorkerShares{Users: []string{"bob"}}})
	e.srv.WorkerAccess = wa
	evs, err := startUpdate(t, e.alice)
	if err != nil || len(evs) != 2 || evs[1].Phase != "installing" {
		t.Fatalf("owner: %v %v", evs, err)
	}
	if err := setAuto(e.alice); err != nil {
		t.Fatalf("owner set-auto: %v", err)
	}
	// bob may use the worker (shared) but not update its machine; the
	// hub's admin certificates get nothing either
	hubAdmin := e.dial(pki.IssueOptions{Role: pki.RoleAdmin, Name: "hub-admin"})
	for name, c := range map[string]*grpc.ClientConn{"bob (shared)": e.bob, "carol": e.carol, "root (admin)": e.root, "hub-admin": hubAdmin} {
		if _, err := startUpdate(t, c); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s started an update: %v", name, err)
		}
		if err := setAuto(c); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s set auto-update: %v", name, err)
		}
	}
	// a worker certificate (another worker, a hub-made one): the role table
	worker := e.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "w2", Hosts: []string{"127.0.0.1"}})
	if _, err := startUpdate(t, worker); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a worker certificate started an update: %v", err)
	}
	if got := strings.Join(f.calls, ","); got != "start alice,auto alice" {
		t.Fatalf("updater calls: %s", got)
	}

	// an owner pinned in longarmd.toml ([access] owners: the admin worker)
	e.srv.WorkerAccess = nil
	e.srv.Access = &Access{Owners: []string{"carol"}}
	if _, err := startUpdate(t, e.carol); err != nil {
		t.Fatalf("pinned owner: %v", err)
	}
	if _, err := startUpdate(t, e.alice); !denied(err) {
		t.Fatalf("not the pinned owner: %v", err)
	}

	// the feature is announced with an updater only
	info, err := pb.NewNodeClient(e.carol).Info(cctx(t), &pb.InfoRequest{})
	if err != nil || !slices.Contains(info.Features, FeatureMachineUpdate) {
		t.Fatalf("features %v %v", info.GetFeatures(), err)
	}
	e.srv.MachineUpdate = nil
	info, _ = pb.NewNodeClient(e.carol).Info(cctx(t), &pb.InfoRequest{})
	if slices.Contains(info.Features, FeatureMachineUpdate) {
		t.Fatal("feature without an updater")
	}
	if slices.Contains(WorkerFeatures, FeatureMachineUpdate) {
		t.Fatal("WorkerFeatures was changed in place")
	}
}

// TestHubCannotTriggerMachineUpdate: the hub reaches a worker through
// HubCommand only; none of its messages asks for a machine update or
// changes automatic updates (UpdateHint only nudges longarmd's own rollout
// check, which never talks to the update service). A new command must be
// added here deliberately.
func TestHubCannotTriggerMachineUpdate(t *testing.T) {
	known := []string{"welcome", "revocations", "toolchain", "audit_ack", "drain", "update", "kill_job", "log_request",
		"submit_job", "toolchain_apply", "tailnet", "tenancy", "worker_access", "port_links",
		"restarting", // restarting: a duration and a reason
		"relays"}     // relays: the relay endpoints to listen on
	md := (&pb.HubCommand{}).ProtoReflect().Descriptor()
	var forbidden = map[protoreflect.FullName]bool{
		(&pb.MachineUpdateRequest{}).ProtoReflect().Descriptor().FullName():     true,
		(&pb.MachineUpdateAutoRequest{}).ProtoReflect().Descriptor().FullName(): true,
	}
	var walk func(m protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool)
	walk = func(m protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) {
		if seen[m.FullName()] {
			return
		}
		seen[m.FullName()] = true
		if forbidden[m.FullName()] {
			t.Fatalf("HubCommand can carry %s", m.FullName())
		}
		fs := m.Fields()
		for i := 0; i < fs.Len(); i++ {
			if f := fs.Get(i); f.Message() != nil {
				walk(f.Message(), seen)
			}
		}
	}
	walk(md, map[protoreflect.FullName]bool{})
	fs := md.Fields()
	for i := 0; i < fs.Len(); i++ {
		if n := string(fs.Get(i).Name()); !slices.Contains(known, n) {
			t.Errorf("new hub command %q: check that it can't update the machine or change automatic updates, then add it here", n)
		}
	}
	// the hub calls no worker RPC (worker certificates reach LinkTunnel.Dial only)
	for _, m := range []string{pb.MachineUpdate_Start_FullMethodName, pb.MachineUpdate_SetAuto_FullMethodName} {
		if slices.Contains(MethodRoles[m], pki.RoleWorker) {
			t.Errorf("%s is open to worker certificates", m)
		}
	}
}
