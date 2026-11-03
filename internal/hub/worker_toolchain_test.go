package hub

import (
	"maps"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func TestWorkerToolchainOwnershipAndMerge(t *testing.T) {
	th, out := ownedHub(t)
	h := th.h
	ctx := tctx(t)
	req := &pb.WorkerToolchainRequest{Worker: "aw", SetTools: true, Tools: []*pb.ToolSpec{
		{Name: "node", Version: "22"}, {Name: "claude"}, {Name: "uv"},
	}}
	for who, conn := range map[string]*grpc.ClientConn{"bob": th.bob, "root": th.root} {
		if _, err := pb.NewFleetClient(conn).WorkerToolchain(ctx, req); status.Code(err) != codes.NotFound {
			t.Fatalf("%s manages private worker: %v", who, err)
		}
	}
	alice := pb.NewFleetClient(th.alice)
	rep, err := alice.WorkerToolchain(ctx, req)
	if err != nil || rep.Desired.OwnerRevision != 1 || rep.WorkerId == "" || len(rep.OwnerTools) != 3 {
		t.Fatalf("set: %v %v", rep, err)
	}
	stored, err := h.store.Node(rep.WorkerId)
	if err != nil || !maps.Equal(stored.OwnerTools, map[string]string{"node": "22", "claude": "latest", "uv": "latest"}) {
		t.Fatalf("persisted: %v %v", stored, err)
	}
	var desired *pb.ToolchainDesired
	for len(out) > 0 {
		if d := (<-out).GetToolchain(); d != nil {
			desired = d
		}
	}
	if desired == nil || desired.OwnerRevision != 1 || len(desired.Tools) != 3 {
		t.Fatalf("not pushed: %v", desired)
	}
	if _, err := h.PutProfile("test", &pb.Profile{Name: "dev", Tools: []*pb.ToolSpec{{Name: "node", Version: "24"}, {Name: "git", Version: "latest"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AssignProfile("test", "aw", "dev"); err != nil {
		t.Fatal(err)
	}
	rep, err = alice.WorkerToolchain(ctx, &pb.WorkerToolchainRequest{Worker: rep.WorkerId})
	if err != nil {
		t.Fatal(err)
	}
	effective := map[string]string{}
	for _, spec := range rep.Desired.Tools {
		effective[spec.Name] = spec.Version
	}
	if !maps.Equal(effective, map[string]string{"node": "24", "git": "latest", "claude": "latest", "uv": "latest"}) {
		t.Fatalf("admin did not win: %v", effective)
	}
	// Shared access permits jobs, not package operations (including apply).
	if _, err := h.ShareWorker("test", "alice", true, &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
	for _, req := range []*pb.WorkerToolchainRequest{
		{Worker: "aw"}, {Worker: "aw", Clear: true}, {Worker: "aw", Apply: true},
	} {
		if _, err := pb.NewFleetClient(th.bob).WorkerToolchain(ctx, req); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("shared user manages tools: %v", err)
		}
	}
	rep, err = alice.WorkerToolchain(ctx, &pb.WorkerToolchainRequest{Worker: "aw", Clear: true})
	if err != nil || len(rep.OwnerTools) != 0 || rep.Desired.OwnerRevision != 2 || len(rep.Desired.Tools) != 2 {
		t.Fatalf("clear: %v %v", rep, err)
	}
	// Applying is a request, not a blocking installation; it uses the
	// same authenticated worker session and propagates worker errors.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case cmd := <-out:
				if apply := cmd.GetToolchainApply(); apply != nil {
					h.mu.Lock()
					n := h.nodeLocked("aw")
					h.deliverReplyLocked(n, n.sess, &pb.HubReply{RequestId: apply.RequestId})
					h.mu.Unlock()
					return
				}
			}
		}
	}()
	if _, err := alice.WorkerToolchain(ctx, &pb.WorkerToolchainRequest{Worker: "aw", Apply: true}); err != nil {
		t.Fatal(err)
	}
	<-done
	for _, req := range []*pb.WorkerToolchainRequest{
		{Worker: "aw", SetTools: true, Clear: true},
		{Worker: "aw", SetTools: true, Tools: []*pb.ToolSpec{{Name: "unknown"}}},
		{Worker: "aw", SetTools: true, Tools: []*pb.ToolSpec{{Name: "node", Version: "--bad"}}},
		{Worker: "aw", SetTools: true, Tools: []*pb.ToolSpec{{Name: "git"}, {Name: "git"}}},
	} {
		if _, err := alice.WorkerToolchain(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("invalid request accepted: %v %v", req, err)
		}
	}
	// Admin workers retain their stronger boundary, even for the owner.
	h.mu.Lock()
	n := h.nodeLocked("aw")
	rec := *n.rec
	rec.Admin = true
	n.rec = &rec
	h.mu.Unlock()
	if _, err := alice.WorkerToolchain(ctx, &pb.WorkerToolchainRequest{Worker: "aw"}); err != nil {
		t.Fatalf("admin worker inventory: %v", err)
	}
	if _, err := alice.WorkerToolchain(ctx, &pb.WorkerToolchainRequest{Worker: "aw", Apply: true}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("admin worker package operations: %v", err)
	}
}

func TestOwnerToolsRespectProfileCanary(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.nodeLocked("aw")
	n.rec.OwnerTools = map[string]string{"node": "18", "uv": "latest"}
	n.rec.Profile = "dev"
	h.profiles["dev"] = &pb.Profile{Name: "dev", Revision: 2, Rollout: "canary", Canary: "other-worker",
		Tools: []*pb.ToolSpec{{Name: "node", Version: "24"}, {Name: "claude", Version: "latest"}}}
	h.stable["dev"] = &pb.Profile{Name: "dev", Revision: 1, Tools: []*pb.ToolSpec{{Name: "node", Version: "22"}}}
	d := h.desiredToolchainLocked(n)
	if d.Revision != 1 || len(d.Tools) != 2 || d.Tools[0].Name != "node" || d.Tools[0].Version != "22" {
		t.Fatalf("owner bypassed stable profile: %v", d)
	}
	delete(h.stable, "dev")
	d = h.desiredToolchainLocked(n)
	if d.Revision != 0 || len(d.Tools) != 1 || d.Tools[0].Name != "uv" {
		t.Fatalf("owner bypassed pending canary: %v", d)
	}
}
