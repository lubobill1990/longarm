package server

import (
	"context"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestAccessDeny: the worker access policy for every combination of the
// caller's role, ownership and user list, on a client call, an admin-only
// call, metrics and Node.Info.
func TestAccessDeny(t *testing.T) {
	var (
		aliceAdmin = pki.Identity{Role: pki.RoleAdmin, Name: "alice-admin", User: "alice"} // an owner
		root       = pki.Identity{Role: pki.RoleAdmin, Name: "root"}                       // a hub admin
		bob        = pki.Identity{Role: pki.RoleClient, Name: "bob-laptop", User: "bob"}
		carol      = pki.Identity{Role: pki.RoleClient, Name: "carol"} // not in users
	)
	exec, gc, metrics, info := pb.Exec_Run_FullMethodName, pb.Admin_Gc_FullMethodName, pb.Metrics_Stream_FullMethodName, pb.Node_Info_FullMethodName
	owners := []string{"alice"}
	tests := []struct {
		name   string
		access *Access
		id     pki.Identity
		method string
		denied bool
		admin  bool // AdminRights
	}{
		{"no policy: admin", nil, root, gc, false, true},
		{"no policy: client", nil, bob, exec, false, false},
		{"full: admin", &Access{Owners: owners, Admins: AdminsFull}, root, gc, false, true},
		{"own: owner admin", &Access{Owners: owners, Admins: AdminsOwn}, aliceAdmin, gc, false, true},
		{"own: admin exec", &Access{Owners: owners, Admins: AdminsOwn}, root, exec, false, false},
		{"own: admin gc", &Access{Owners: owners, Admins: AdminsOwn}, root, gc, true, false},
		{"own: client unaffected", &Access{Owners: owners, Admins: AdminsOwn}, bob, exec, false, false},
		{"observe: admin metrics", &Access{Admins: AdminsObserve}, root, metrics, false, false},
		{"observe: admin exec", &Access{Admins: AdminsObserve}, root, exec, true, false},
		{"observe: owner admin exec", &Access{Owners: owners, Admins: AdminsObserve}, aliceAdmin, exec, false, true},
		{"none: admin metrics", &Access{Admins: AdminsNone}, root, metrics, true, false},
		{"none: admin info", &Access{Admins: AdminsNone}, root, info, false, false},
		{"none: client exec", &Access{Admins: AdminsNone}, bob, exec, false, false},
		{"unknown mode denies", &Access{Admins: "most"}, root, metrics, true, false},
		{"users: listed", &Access{Users: []string{"bob"}}, bob, exec, false, false},
		{"users: owner implicit", &Access{Users: []string{"bob"}, Owners: owners}, aliceAdmin, gc, false, true},
		{"users: not listed", &Access{Users: []string{"bob"}}, carol, exec, true, false},
		{"users: not listed, info", &Access{Users: []string{"bob"}}, carol, info, false, false},
		{"users: full admin not listed", &Access{Users: []string{"bob"}}, root, gc, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if why := tt.access.Deny(tt.id, tt.method); (why != "") != tt.denied {
				t.Errorf("Deny(%s, %s) = %q, want denied %v", tt.id, tt.method, why, tt.denied)
			}
			if got := tt.access.AdminRights(tt.id); got != tt.admin {
				t.Errorf("AdminRights(%s) = %v, want %v", tt.id, got, tt.admin)
			}
		})
	}
}

func TestAccessValidate(t *testing.T) {
	for _, tt := range []struct {
		a    Access
		want string // error substring ("" = valid)
	}{
		{Access{}, ""},
		{Access{Users: []string{"alice"}, Owners: []string{"bob"}, Admins: AdminsObserve}, ""},
		{Access{Admins: "read-only"}, `admins "read-only"`},
		{Access{Users: []string{"Alice"}}, `users: invalid user name "Alice"`},
		{Access{Owners: []string{"a b"}}, `owners: invalid user name`},
	} {
		err := tt.a.Validate()
		if (err == nil) != (tt.want == "") || err != nil && !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%+v: %v, want %q", tt.a, err, tt.want)
		}
	}
	if (&Access{}).Active() || (&Access{Admins: AdminsFull}).Active() || (*Access)(nil).Active() {
		t.Error("a default policy is active")
	}
	if !(&Access{Owners: []string{"alice"}}).Active() || !(&Access{Admins: AdminsOwn}).Active() {
		t.Error("a restricting policy is inactive")
	}
}

// TestAccessOnWorker: a one-person fleet (no separating hub, shared
// isolation) whose owner alice limits the hub's admin root. alice keeps
// acting on everything with her plain certificates; root and bob are held
// to ownership.
func TestAccessOnWorker(t *testing.T) {
	e := newAuthzEnv(t)
	e.srv.Policy.Set(&pb.TenancyPolicy{Version: 2}) // the hub does not separate users
	e.srv.Access = &Access{Owners: []string{"alice"}, Admins: AdminsOwn}
	sleepJob(t, e.bob, "JOBB")
	sleepJob(t, e.alice, "JOBA")

	// The owner: everything, as before.
	if _, err := pb.NewJobsClient(e.alice).Get(cctx(t), &pb.GetJobRequest{Id: "JOBB"}); err != nil {
		t.Errorf("owner reads bob's job: %v", err)
	}
	// own: root is a client of user root.
	if _, err := pb.NewJobsClient(e.root).Get(cctx(t), &pb.GetJobRequest{Id: "JOBA"}); status.Code(err) != codes.NotFound {
		t.Errorf("own: admin reads alice's job: %v", err)
	}
	if _, err := pb.NewAdminClient(e.root).Gc(cctx(t), &pb.GcRequest{DryRun: true}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("own: admin gc: %v", err)
	}
	if err := attachErr(cctx(t), e.root, &pb.AttachOpen{Name: "x", Owner: "alice", NoCreate: true}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("own: admin attaches alice's session: %v", err)
	}
	// bob (no owner) no longer sees alice's job.
	if _, err := pb.NewJobsClient(e.bob).Get(cctx(t), &pb.GetJobRequest{Id: "JOBA"}); status.Code(err) != codes.NotFound {
		t.Errorf("bob reads alice's job: %v", err)
	}

	e.srv.Access = &Access{Owners: []string{"alice"}, Admins: AdminsObserve}
	m, err := pb.NewMetricsClient(e.root).Snapshot(cctx(t), &pb.MetricsRequest{})
	if err != nil || len(m.TopCpu) > 0 || len(m.TopMem) > 0 {
		t.Errorf("observe: metrics %v, process lists %d/%d", err, len(m.GetTopCpu()), len(m.GetTopMem()))
	}
	exe, _ := os.Executable()
	if _, err := pb.NewExecClient(e.root).Run(cctx(t), &pb.ExecRequest{Argv: []string{exe, "-test.run", "^$"}}); status.Code(err) != codes.PermissionDenied ||
		!strings.Contains(err.Error(), "admins = observe") {
		t.Errorf("observe: exec %v", err)
	}

	e.srv.Access = &Access{Users: []string{"bob"}, Owners: []string{"alice"}, Admins: AdminsNone}
	if _, err := pb.NewMetricsClient(e.root).Snapshot(cctx(t), &pb.MetricsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("none: metrics %v", err)
	}
	if _, err := pb.NewNodeClient(e.root).Info(cctx(t), &pb.InfoRequest{}); err != nil {
		t.Errorf("none: node info %v", err)
	}
	if _, err := pb.NewJobsClient(e.carol).List(cctx(t), &pb.ListJobsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("carol is no user here: %v", err)
	}
	if _, err := pb.NewJobsClient(e.bob).Get(cctx(t), &pb.GetJobRequest{Id: "JOBB"}); err != nil {
		t.Errorf("bob uses the worker: %v", err)
	}
	if _, err := e.srv.SubmitAs(context.Background(), "carol", jobs.Spec{ID: "HUBC", Argv: []string{exe}}, nil); status.Code(err) != codes.PermissionDenied {
		t.Errorf("hub submits for carol: %v", err)
	}
	if b, _ := os.ReadFile(e.srv.Audit.path); !strings.Contains(string(b), `"denied":"not a user of this worker"`) {
		t.Errorf("denial not audited:\n%s", b)
	}
}
