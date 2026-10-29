package hub

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// ownedHub is a tenancy hub where alice registered worker "aw" with a
// worker code of her own; aw is online (a fake session whose commands the
// test reads) with plenty of free memory.
func ownedHub(t *testing.T) (*tenancyHub, chan *pb.HubCommand) {
	t.Helper()
	th := newTenancyHub(t)
	jc, err := pb.NewFleetClient(th.alice).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Name: "aw"})
	if err != nil || jc.Owner != "alice" || jc.Role != "worker" || jc.Name != "aw" {
		t.Fatalf("worker code %v %v", jc, err)
	}
	if ttl := time.Until(jc.ExpiresAt.AsTime()); ttl < 23*time.Hour || ttl > 25*time.Hour {
		t.Fatalf("default ttl %v", ttl)
	}
	r, err := hubclient.Join(tctx(t), th.h.Addr("grpc"), jc.Code, hubclient.JoinOptions{Fingerprint: pki.Fingerprint(th.ca.Cert)})
	if err != nil {
		t.Fatal(err)
	}
	// approved at once (the hub is not auto_approve), and told whose it is
	if r.Resp.State != stateApproved || len(r.Resp.CertPem) == 0 || r.Resp.Owner != "alice" || r.Resp.Name != "aw" {
		t.Fatalf("join: %v", r.Resp)
	}
	h := th.h
	out := make(chan *pb.HubCommand, 128)
	h.mu.Lock()
	n := h.nodes["aw"]
	n.sess = &nodeSession{id: 99, out: out, cancel: func() {}}
	n.metrics = &pb.MetricsSnapshot{MemTotalBytes: 64 << 30, MemUsedBytes: 1 << 30, CpuPercent: 1}
	h.mu.Unlock()
	return th, out
}

func nodeNames(t *testing.T, conn *grpc.ClientConn) map[string]*pb.NodeInfo {
	t.Helper()
	r, err := pb.NewFleetClient(conn).ListNodes(tctx(t), &pb.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]*pb.NodeInfo{}
	for _, n := range r.Nodes {
		m[n.Name] = n
	}
	return m
}

// place asks for a worker for user (no queue).
func place(t *testing.T, h *Hub, user string) (string, error) {
	t.Helper()
	var got string
	err := h.PlaceFor(tctx(t), "client/"+user, user, &pb.PlaceRequest{JobId: newULID(), NoQueue: true}, func(u *pb.PlaceUpdate) error {
		got = u.Worker
		return nil
	})
	return got, err
}

func newULID() string {
	idSeq++
	return fmt.Sprintf("01J0000000000000000000%04d", idSeq)
}

var idSeq int

// lastAccess drains out and returns the last worker_access command.
func lastAccess(out chan *pb.HubCommand) *pb.WorkerAccess {
	var wa *pb.WorkerAccess
	for {
		select {
		case c := <-out:
			if x := c.GetWorkerAccess(); x != nil {
				wa = x
			}
		default:
			return wa
		}
	}
}

// TestOwnedWorkers: a user's own worker joins approved, is seen and placed
// for its owner only, and shared - with an admin's permission - by user,
// team or everyone; every change reaches the worker.
func TestOwnedWorkers(t *testing.T) {
	th, out := ownedHub(t)
	h := th.h
	if !h.TenancyOn() {
		t.Fatal("an owned worker must turn separation on")
	}
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "bob", SetQuota: true}); err != nil { // placement, not quota
		t.Fatal(err)
	}

	// visibility
	if n := nodeNames(t, th.alice)["aw"]; n == nil || n.Owner != "alice" {
		t.Fatalf("alice's list: %v", n)
	}
	for name, c := range map[string]*grpc.ClientConn{"bob": th.bob, "carol": th.carol} {
		if _, ok := nodeNames(t, c)["aw"]; ok {
			t.Errorf("%s sees alice's worker", name)
		}
		if _, ok := nodeNames(t, c)["w1"]; !ok {
			t.Errorf("%s does not see the fleet worker", name)
		}
	}
	if n, ok := nodeNames(t, th.root)["aw"]; ok { // admins see what users see
		t.Fatalf("admin's list shows alice's worker: %v", n)
	}
	if hist := h.HistoryFor(h.viewer(caller{id: pki.Identity{Role: pki.RoleClient, Name: "bob-laptop", User: "bob"}}), "aw"); len(hist.Nodes) != 0 {
		t.Errorf("bob gets aw's history")
	}

	// placement: alice lands on aw; bob never does (w1 is offline: nothing)
	if w, err := place(t, h, "alice"); err != nil || w != "aw" {
		t.Fatalf("alice placed on %q: %v", w, err)
	}
	if w, err := place(t, h, "bob"); err == nil || strings.Contains(err.Error(), "aw") || strings.Contains(err.Error(), "quota") {
		t.Fatalf("bob placed on %q: %v", w, err)
	}
	if _, err := h.SubmitJobAs(tctx(t), "client/bob-laptop", "bob", false, &pb.HubSubmitRequest{Worker: "aw", Argv: []string{"x"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("bob submits to aw through the hub: %v", err)
	}

	// the hub session: admins can't kill, read or submit on alice's worker
	h.mu.Lock()
	h.applyJobsLocked(h.nodes["aw"], &pb.JobsReport{Jobs: []*pb.Job{{Spec: &pb.JobSpec{Id: "JAW", Owner: "alice"}, State: pb.JobState_JOB_STATE_RUNNING}}})
	h.mu.Unlock()
	if _, err := pb.NewHubAdminClient(th.root).KillJob(tctx(t), &pb.FleetJobRequest{Id: "JAW"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("admin kills alice's job through the hub: %v", err)
	}
	st, err := pb.NewHubAdminClient(th.root).JobLogs(tctx(t), &pb.HubLogsRequest{Id: "JAW"})
	if err == nil {
		_, err = st.Recv()
	}
	if status.Code(err) != codes.NotFound { // not even known to the admin
		t.Fatalf("admin reads alice's job through the hub: %v", err)
	}
	if _, err := pb.NewHubAdminClient(th.root).SubmitJob(tctx(t), &pb.HubSubmitRequest{Worker: "aw", Argv: []string{"x"}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("admin submits to alice's worker through the hub: %v", err)
	}
	// alice herself (her panel login acts as alice) passes the check
	if err := h.relayCheck("web/alice", "alice", "aw", "kill jobs", "JAW"); err != nil {
		t.Fatalf("alice through the hub: %v", err)
	}
	if err := h.relayCheck("admin/root", "root", "w1", "kill jobs", "JB"); err != nil {
		t.Fatalf("admin on a fleet worker: %v", err)
	}

	// sharing needs can_share; others can't touch it
	share := func(c *grpc.ClientConn, req *pb.ShareWorkerRequest) (*pb.NodeInfo, error) {
		return pb.NewFleetClient(c).ShareWorker(tctx(t), req)
	}
	if _, err := share(th.alice, &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"bob"}}); status.Code(err) != codes.PermissionDenied ||
		!strings.Contains(err.Error(), "can-share") {
		t.Fatalf("share without can_share: %v", err)
	}
	if _, err := share(th.bob, &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"bob"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("bob shares alice's worker: %v", err)
	}
	if _, err := share(th.root, &pb.ShareWorkerRequest{Worker: "w1", AddUsers: []string{"bob"}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("share a fleet worker: %v", err)
	}
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "alice", SetCanShare: true, CanShare: true}); err != nil {
		t.Fatal(err)
	}
	if me, err := pb.NewFleetClient(th.alice).Whoami(tctx(t), &pb.WhoamiRequest{}); err != nil || !me.CanShare {
		t.Fatalf("whoami %v %v", me, err)
	}
	if _, err := share(th.alice, &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"nobody"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("share with an unknown user: %v", err)
	}
	if _, err := share(th.alice, &pb.ShareWorkerRequest{Worker: "aw", AddTeams: []string{"Bad Team"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("share with a bad team: %v", err)
	}
	lastAccess(out)
	info, err := share(th.alice, &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"bob"}})
	if err != nil || !slices.Equal(info.Shares.GetUsers(), []string{"bob"}) {
		t.Fatalf("share with bob: %v %v", info, err)
	}
	wa := lastAccess(out)
	if wa == nil || wa.Owner != "alice" || !slices.Equal(wa.Shares.GetUsers(), []string{"bob"}) || wa.Version < 2 {
		t.Fatalf("pushed %v", wa)
	}
	if n := nodeNames(t, th.bob)["aw"]; n == nil || n.Shares != nil {
		t.Fatalf("bob's view of the shared worker: %v", n)
	}
	if w, err := place(t, h, "bob"); err != nil || w != "aw" {
		t.Fatalf("bob placed on %q: %v", w, err)
	}
	if err := h.relayCheck("web/bob", "bob", "aw", "read logs", "JAW"); err != nil {
		t.Errorf("bob (shared) through the hub: %v", err)
	}
	if _, ok := nodeNames(t, th.carol)["aw"]; ok {
		t.Error("carol sees a worker shared with bob")
	}

	// unshare; share with team t1 (alice, carol); then everyone
	if _, err := share(th.alice, &pb.ShareWorkerRequest{Worker: "aw", RemoveUsers: []string{"bob"}, AddTeams: []string{"t1"}}); err != nil {
		t.Fatal(err)
	}
	if wa := lastAccess(out); wa == nil || len(wa.Shares.GetUsers()) != 0 || !slices.Equal(wa.Shares.GetTeams(), []string{"t1"}) {
		t.Fatalf("pushed %v", wa)
	}
	if _, ok := nodeNames(t, th.bob)["aw"]; ok {
		t.Error("bob still sees aw")
	}
	if _, ok := nodeNames(t, th.carol)["aw"]; !ok {
		t.Error("carol (team t1) does not see aw")
	}
	if _, err := share(th.root, &pb.ShareWorkerRequest{Worker: "aw", RemoveTeams: []string{"t1"}, SetEveryone: true, Everyone: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := nodeNames(t, th.bob)["aw"]; !ok {
		t.Error("bob does not see a worker shared with everyone")
	}
	if _, err := share(th.root, &pb.ShareWorkerRequest{Worker: "aw", SetEveryone: true}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	shares := h.nodes["aw"].rec.Shares
	h.mu.Unlock()
	if shares != nil {
		t.Fatalf("all unshared, shares %v", shares)
	}

	// the hub audits it
	rows, _ := h.store.Audit("hub", time.Time{}, 100)
	var methods []string
	for _, r := range rows {
		methods = append(methods, r.Method)
	}
	for _, m := range []string{"workers/code", "workers/join", "workers/share", "workers/denied"} {
		if !slices.Contains(methods, m) {
			t.Errorf("no %s audit record in %v", m, methods)
		}
	}

	// removal: owner (or admin) only
	if _, err := pb.NewFleetClient(th.bob).RemoveWorker(tctx(t), &pb.NodeRequest{Name: "aw"}); status.Code(err) != codes.NotFound {
		t.Fatalf("bob removes aw: %v", err)
	}
	if _, err := pb.NewFleetClient(th.alice).RemoveWorker(tctx(t), &pb.NodeRequest{Name: "w1"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("alice removes the fleet worker: %v", err)
	}
	if n, err := pb.NewFleetClient(th.alice).RemoveWorker(tctx(t), &pb.NodeRequest{Name: "aw"}); err != nil || n.State != stateRevoked {
		t.Fatalf("alice removes aw: %v %v", n, err)
	}
}

// TestWorkerCodes: owners only on worker codes, valid users, a cap on
// unused codes per user, disabled users get none.
func TestWorkerCodes(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	if _, err := h.CreateJoinCode("test", &pb.CreateJoinCodeRequest{Role: "client", Owner: "alice"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("client code with owner: %v", err)
	}
	if _, err := h.CreateJoinCode("test", &pb.CreateJoinCodeRequest{Role: "worker", Owner: "Not A User"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad owner: %v", err)
	}
	jc, err := pb.NewHubAdminClient(th.root).CreateJoinCode(tctx(t), &pb.CreateJoinCodeRequest{Role: "worker", Owner: "bob", Ttl: durationpb.New(time.Hour)})
	if err != nil || jc.Owner != "bob" {
		t.Fatalf("admin's code for bob: %v %v", jc, err)
	}
	for i := 1; i < maxWorkerCodes; i++ {
		if _, err := pb.NewFleetClient(th.bob).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{}); err != nil {
			t.Fatalf("code %d: %v", i, err)
		}
	}
	if _, err := pb.NewFleetClient(th.bob).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("code over the cap: %v", err)
	}
	if _, err := pb.NewFleetClient(th.alice).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Name: "Bad Name"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := pb.NewFleetClient(th.alice).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Labels: map[string]string{"bad key": "x"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad label: %v", err)
	}
	// a code of a user disabled meanwhile does not join
	jc, err = pb.NewFleetClient(th.carol).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Name: "cw"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "carol", SetDisabled: true, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := hubclient.Join(tctx(t), h.Addr("grpc"), jc.Code, hubclient.JoinOptions{Fingerprint: pki.Fingerprint(th.ca.Cert)}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("join with a disabled user's code: %v", err)
	}
	if _, err := h.CreateWorkerCode("test", "carol", &pb.CreateWorkerCodeRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("disabled user's code: %v", err)
	}
}

// TestCanSeeNode: the event stream's filter hides other users' workers.
func TestCanSeeNode(t *testing.T) {
	bob := tenancy.Caller{User: "bob", Teams: []string{"t2"}}
	for _, c := range []struct {
		n    *pb.NodeInfo
		want bool
	}{
		{&pb.NodeInfo{Role: "worker"}, true},
		{&pb.NodeInfo{Role: "worker", Owner: "alice"}, false},
		{&pb.NodeInfo{Role: "worker", Owner: "bob"}, true},
		{&pb.NodeInfo{Role: "worker", Owner: "alice", Shares: &pb.WorkerShares{Users: []string{"bob"}}}, true},
		{&pb.NodeInfo{Role: "worker", Owner: "alice", Shares: &pb.WorkerShares{Teams: []string{"t2"}}}, true},
		{&pb.NodeInfo{Role: "worker", Owner: "alice", Shares: &pb.WorkerShares{Teams: []string{"t1"}}}, false},
		{&pb.NodeInfo{Role: "worker", Owner: "alice", Shares: &pb.WorkerShares{Everyone: true}}, true},
		{&pb.NodeInfo{Role: "client", Name: "alice"}, false},
	} {
		if got := canSeeNode(bob, c.n); got != c.want {
			t.Errorf("%v: %v, want %v", c.n, got, c.want)
		}
		ev := eventFilter(bob)(&pb.FleetEvent{Kind: "node", NodeInfo: c.n}, "")
		if (ev != nil) != c.want {
			t.Errorf("event %v: %v", c.n, ev)
		}
	}
	if !canSeeNode(tenancy.Caller{Admin: true}, &pb.NodeInfo{Role: "worker", Owner: "alice"}) {
		t.Error("admins see every worker")
	}
}

// TestFollowedLogsEndOnUnshare: a followed log stream through the hub is
// re-authorized on every poll: unsharing ends bob's stream.
func TestFollowedLogsEndOnUnshare(t *testing.T) {
	th, out := ownedHub(t)
	h := th.h
	if _, err := h.ShareWorker("test", "", true, &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
	// the fake worker answers every log request with one more byte, never EOF
	go func() {
		for c := range out {
			lr := c.GetLogRequest()
			if lr == nil {
				continue
			}
			h.mu.Lock()
			if n := h.nodes["aw"]; n.sess != nil {
				h.deliverReplyLocked(n, n.sess, &pb.HubReply{RequestId: lr.RequestId,
					Log: &pb.LogChunk{Offset: lr.Offset, Data: []byte("x")}, LogSize: lr.Offset + 1})
			}
			h.mu.Unlock()
		}
	}()
	got := make(chan struct{}, 100)
	done := make(chan error, 1)
	go func() {
		done <- h.JobLogs(tctx(t), "client/bob-laptop", "bob", &pb.HubLogsRequest{Id: "JAW", Worker: "aw", Follow: true},
			func(*pb.LogChunk) error { got <- struct{}{}; return nil })
	}()
	select {
	case <-got:
	case err := <-done:
		t.Fatalf("bob's stream ended at once: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no log output for bob")
	}
	if _, err := h.ShareWorker("test", "", true, &pb.ShareWorkerRequest{Worker: "aw", RemoveUsers: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("stream ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bob's followed stream survived the unshare")
	}
}

// TestWorkerCodesDie: a removed or disabled user's unused worker codes are
// gone, and codes of unknown users are neither made nor redeemed.
func TestWorkerCodesDie(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	if _, err := h.CreateJoinCode("test", &pb.CreateJoinCodeRequest{Role: "worker", Owner: "dave"}); status.Code(err) != codes.NotFound {
		t.Fatalf("code for an unknown user: %v", err)
	}
	join := func(code string) error {
		_, err := hubclient.Join(tctx(t), h.Addr("grpc"), code, hubclient.JoinOptions{Fingerprint: pki.Fingerprint(th.ca.Cert)})
		return err
	}
	bobs, err := h.CreateWorkerCode("test", "bob", &pb.CreateWorkerCodeRequest{Name: "bw"})
	if err != nil {
		t.Fatal(err)
	}
	carols, err := h.CreateWorkerCode("test", "carol", &pb.CreateWorkerCodeRequest{Name: "cw"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.RemoveUser("test", &pb.RemoveUserRequest{Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "carol", SetDisabled: true, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"bob", "carol"} {
		if n, err := h.activeWorkerCodes(u); n != 0 || err != nil {
			t.Errorf("%s still has %d worker codes (%v)", u, n, err)
		}
	}
	for _, c := range []string{bobs.Code, carols.Code} {
		if err := join(c); status.Code(err) != codes.PermissionDenied {
			t.Errorf("join with a dropped code: %v", err)
		}
	}
	// a code made for a user who is gone by the time it is used (e.g.
	// removed while the store write raced) is refused at the join too
	alices, err := h.CreateWorkerCode("test", "alice", &pb.CreateWorkerCodeRequest{Name: "aw2"})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	delete(h.tenants, "alice")
	h.mu.Unlock()
	if err := join(alices.Code); status.Code(err) != codes.PermissionDenied {
		t.Errorf("join with the code of a vanished user: %v", err)
	}
}
