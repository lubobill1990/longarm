package server

import (
	"context"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestWorkerOwner: a user-owned worker (owner and shares from the hub)
// serves its owner and whom it is shared with - by user, team or
// everyone; admins are no exception; Node.Info stays open; and the rule
// only narrows what [access] allows.
func TestWorkerOwner(t *testing.T) {
	e := newAuthzEnv(t)
	wa, _ := tenancy.LoadWorkerAccess("")
	e.srv.WorkerAccess = wa
	var version int64
	set := func(s *pb.WorkerShares) {
		version++
		wa.Set(&pb.WorkerAccess{Version: version, Owner: "alice", Shares: s})
	}
	set(nil)
	list := func(c *grpc.ClientConn) error {
		_, err := pb.NewJobsClient(c).List(cctx(t), &pb.ListJobsRequest{})
		return err
	}
	denied := func(err error) bool {
		return status.Code(err) == codes.PermissionDenied && strings.Contains(err.Error(), "worker owner/shares")
	}
	sleepJob(t, e.alice, "JOBA")

	if err := list(e.alice); err != nil {
		t.Fatalf("owner: %v", err)
	}
	for name, c := range map[string]*grpc.ClientConn{"bob": e.bob, "carol": e.carol, "root": e.root} {
		if err := list(c); !denied(err) {
			t.Errorf("%s lists jobs: %v", name, err)
		}
		if _, err := pb.NewNodeClient(c).Info(cctx(t), &pb.InfoRequest{}); err != nil {
			t.Errorf("%s: Node.Info %v", name, err)
		}
	}
	if _, err := pb.NewAdminClient(e.root).Gc(cctx(t), &pb.GcRequest{DryRun: true}); !denied(err) {
		t.Errorf("admin gc on an owned worker: %v", err)
	}
	exe, _ := os.Executable()
	if _, err := e.srv.SubmitAs(context.Background(), "bob", jobs.Spec{ID: "HUBB", Argv: []string{exe}}, nil); status.Code(err) != codes.PermissionDenied {
		t.Errorf("hub submits for bob: %v", err)
	}

	// shared with bob
	set(&pb.WorkerShares{Users: []string{"bob"}})
	if err := list(e.bob); err != nil {
		t.Errorf("bob (shared): %v", err)
	}
	if _, err := pb.NewJobsClient(e.bob).Get(cctx(t), &pb.GetJobRequest{Id: "JOBA"}); status.Code(err) != codes.NotFound {
		t.Errorf("bob reads alice's job on her shared worker: %v", err)
	}
	if err := list(e.carol); !denied(err) {
		t.Errorf("carol (not shared): %v", err)
	}
	// with team t1 (alice, carol: the hub's tenancy policy)
	set(&pb.WorkerShares{Teams: []string{"t1"}})
	if err := list(e.carol); err != nil {
		t.Errorf("carol (team): %v", err)
	}
	if err := list(e.bob); !denied(err) {
		t.Errorf("bob (unshared): %v", err)
	}
	// with root's user: an admin certificate then works, but holds no
	// admin rights over the owner's jobs
	set(&pb.WorkerShares{Users: []string{"root"}})
	if _, err := pb.NewAdminClient(e.root).Gc(cctx(t), &pb.GcRequest{DryRun: true}); err != nil {
		t.Errorf("admin gc (shared): %v", err)
	}
	if _, err := pb.NewJobsClient(e.root).Get(cctx(t), &pb.GetJobRequest{Id: "JOBA"}); status.Code(err) != codes.NotFound {
		t.Errorf("shared admin reads the owner's job: %v", err)
	}
	// with everyone - but [access] (the worker's own) still wins
	set(&pb.WorkerShares{Everyone: true})
	if err := list(e.bob); err != nil {
		t.Errorf("bob (everyone): %v", err)
	}
	e.srv.Access = &Access{Users: []string{"alice"}}
	if err := list(e.bob); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "not a user of this worker") {
		t.Errorf("[access] users = alice, shared with everyone: bob %v", err)
	}
	e.srv.Access = nil

	// no owner: as before
	wa.Set(&pb.WorkerAccess{Version: version + 1})
	if err := list(e.bob); err != nil {
		t.Errorf("no owner: %v", err)
	}
	if b, _ := os.ReadFile(e.srv.Audit.path); !strings.Contains(string(b), `"denied":"worker owner/shares"`) {
		t.Errorf("denial not audited:\n%s", b)
	}
}
