package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Visibility scope: non-owners - admins included - neither see nor use
// other users' workers and devices. These tests cover the ways around it a
// security review found: a worker named like a user, an admin sharing
// another user's worker with itself, operations by name, node-filtered
// audit counts and other users' reserved memory.

// TestWorkerNamedLikeUser: a worker certificate never acts as a user, even
// one whose name is a user's; and node names never take a user's name.
func TestWorkerNamedLikeUser(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	nodeNames(t, th.bob) // bob's device is known to the hub
	// a worker certificate named "bob" (issued by hand: enrollment refuses
	// the name, below) sees none of bob's machines
	wb := th.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "bob"})
	addWorker(h, "bw", "bob", false, "1.0.0")
	r, err := pb.NewFleetClient(wb).ListNodes(tctx(t), &pb.ListNodesRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range r.Nodes {
		if n.Name == "bob-laptop" || n.Name == "bw" {
			t.Errorf("a worker named bob sees bob's %s: %v", n.Name, n)
		}
	}
	me, err := pb.NewFleetClient(wb).Whoami(tctx(t), &pb.WhoamiRequest{})
	if err != nil || me.User != "" || me.Usage != nil {
		t.Errorf("worker whoami %v %v", me, err)
	}
	h.mu.Lock()
	if u := h.userOfLocked(pki.Identity{Role: pki.RoleWorker, Name: "bob"}, ""); u != "" {
		t.Errorf("worker acts as %q", u)
	}
	// a device whose certificate lacks the user: the hub's record decides
	old := &node{rec: &store.Node{Name: "bobs-old", Role: "client", State: stateApproved, User: "bob"}}
	h.putTestNode(old)
	if u := h.userOfLocked(pki.Identity{Role: pki.RoleClient, Name: "bobs-old"}, old.rec.ID); u != "bob" {
		t.Errorf("recorded device acts as %q", u)
	}
	// the record of another role does not
	if u := h.userOfLocked(pki.Identity{Role: pki.RoleAdmin, Name: "bobs-old"}, old.rec.ID); u != "bobs-old" {
		t.Errorf("admin certificate with a client's name acts as %q", u)
	}
	h.mu.Unlock()

	// alice can't enroll a worker named after a user (code, sign-in)
	jc, err := pb.NewFleetClient(th.alice).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Name: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = hubclient.Join(tctx(t), h.Addr("grpc"), jc.Code, hubclient.JoinOptions{Fingerprint: pki.Fingerprint(th.ca.Cert)})
	if status.Code(err) != codes.AlreadyExists || !strings.Contains(err.Error(), "user's name") {
		t.Fatalf("worker named carol: %v", err)
	}
	if _, err := h.enrollOwnedWorker("test", "alice", deviceRequest{name: "carol", hostname: "x"}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("owned worker named carol: %v", err)
	}
	h.mu.Lock()
	reserved := map[[3]string]bool{
		{"carol", "worker", "alice"}: true,  // a worker never
		{"carol", "worker", "carol"}: true,  // not even carol's own
		{"carol", "client", "alice"}: true,  // another user's device
		{"carol", "client", "carol"}: false, // carol's own device
		{"carol", "admin", ""}:       true,  // a device bound to no user: its name would be its user
		{"dave", "worker", "alice"}:  false, // no user of that name
	}
	for k, want := range reserved {
		if got := h.nodeNameReservedLocked(k[0], k[1], k[2]); got != want {
			t.Errorf("reserved%v = %v", k, got)
		}
	}
	h.mu.Unlock()
}

// TestAdminOperationsByName: an admin's operations on another user's
// machine by name answer exactly like an unknown name and do nothing;
// fleet workers still work.
func TestAdminOperationsByName(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	nodeNames(t, th.bob) // bob-laptop: known to the hub
	ctx := tctx(t)
	adm, fleet := pb.NewHubAdminClient(th.root), pb.NewFleetClient(th.root)
	same := func(what string, call func(name string) error) {
		t.Helper()
		want := call("nosuch")
		for _, name := range []string{"aw", "bob-laptop"} {
			got := call(name)
			if status.Code(want) != codes.NotFound || status.Code(got) != codes.NotFound ||
				strings.ReplaceAll(status.Convert(got).Message(), name, "?") != strings.ReplaceAll(status.Convert(want).Message(), "nosuch", "?") {
				t.Errorf("%s %s: %v (unknown name: %v)", what, name, got, want)
			}
		}
	}
	same("drain", func(n string) error {
		_, err := adm.SetDrain(ctx, &pb.SetDrainRequest{Name: n, Draining: true})
		return err
	})
	same("labels {}", func(n string) error {
		_, err := adm.SetLabels(ctx, &pb.SetLabelsRequest{Name: n, Labels: map[string]string{}})
		return err
	})
	same("labels", func(n string) error {
		_, err := adm.SetLabels(ctx, &pb.SetLabelsRequest{Name: n, Labels: map[string]string{"zone": "x"}})
		return err
	})
	same("profile", func(n string) error {
		_, err := adm.AssignProfile(ctx, &pb.AssignProfileRequest{Node: n})
		return err
	})
	same("approve", func(n string) error { _, err := adm.Approve(ctx, &pb.NodeRequest{Name: n}); return err })
	same("toolchain", func(n string) error { _, err := adm.ToolchainApply(ctx, &pb.NodeRequest{Name: n}); return err })
	same("worker labels", func(n string) error {
		_, err := fleet.SetWorkerLabels(ctx, &pb.SetLabelsRequest{Name: n, Labels: map[string]string{"zone": "x"}})
		return err
	})
	same("share", func(n string) error {
		_, err := fleet.ShareWorker(ctx, &pb.ShareWorkerRequest{Worker: n, AddUsers: []string{"alice"}})
		return err
	})
	same("remove", func(n string) error { _, err := fleet.RemoveWorker(ctx, &pb.NodeRequest{Name: n}); return err })
	same("revoke", func(n string) error { _, err := adm.Revoke(ctx, &pb.NodeRequest{Name: n}); return err })

	// the panel: the same answers
	if _, err := AddUser(h.store, "admin1", "admin-password-1", false); err != nil {
		t.Fatal(err)
	}
	pu, _ := h.store.GetUser("admin1")
	panel := panelLogin(t, h, "admin1", "admin-password-1", pu.TOTPSecret)
	post := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, "http://"+h.Addr("http")+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := panel.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, c := range [][3]string{
		{"PUT", "/api/nodes/%s/labels", "{}"},
		{"POST", "/api/nodes/%s/drain", ""},
		{"POST", "/api/nodes/%s/revoke", ""},
		{"PUT", "/api/nodes/%s/profile", `{"profile":""}`},
		{"POST", "/api/nodes/%s/share", `{"add_users":["admin1"]}`},
		{"POST", "/api/nodes/%s/worker-labels", `{"zone":"x"}`},
		{"POST", "/api/nodes/%s/remove", ""},
	} {
		wc, wb := post(c[0], strings.Replace(c[1], "%s", "nosuch", 1), c[2])
		for _, name := range []string{"aw", "bob-laptop"} {
			gc, gb := post(c[0], strings.Replace(c[1], "%s", name, 1), c[2])
			if gc != http.StatusNotFound || gc != wc || strings.ReplaceAll(gb, name, "?") != strings.ReplaceAll(wb, "nosuch", "?") {
				t.Errorf("panel %s %s: %d %s (unknown name: %d %s)", c[0], strings.Replace(c[1], "%s", name, 1), gc, gb, wc, wb)
			}
		}
	}

	// nothing happened to them
	h.mu.Lock()
	aw, bl := *h.nodeLocked("aw").rec, *h.nodeLocked("bob-laptop").rec
	h.mu.Unlock()
	if aw.AdminDrain || aw.State != stateApproved || aw.Labels["zone"] != "" || aw.Shares != nil || bl.State != stateApproved || bl.AdminDrain {
		t.Fatalf("hidden machines changed: %+v %+v", aw, bl)
	}
	// the fleet worker: the user's view
	if n, err := adm.SetDrain(ctx, &pb.SetDrainRequest{Name: "w1", Draining: true}); err != nil || n.Isolation != "per-user" {
		t.Fatalf("drain w1: %v %v", n, err)
	}
	// alice still manages hers
	if n, err := pb.NewFleetClient(th.alice).SetWorkerLabels(ctx, &pb.SetLabelsRequest{Name: "aw", Labels: map[string]string{"zone": "a"}}); err != nil || n.Labels["zone"] != "a" {
		t.Fatalf("alice labels aw: %v %v", n, err)
	}
}

// TestAdminCantShareOthersWorker: an admin can't share another user's
// worker with itself (or anybody) - not when it is hidden, not when it is
// already shared with the admin; its owner can.
func TestAdminCantShareOthersWorker(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "root", Create: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "alice", SetCanShare: true, CanShare: true}); err != nil {
		t.Fatal(err)
	}
	share := pb.NewFleetClient(th.root).ShareWorker
	if _, err := share(tctx(t), &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"root"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("admin shares a hidden worker with itself: %v", err)
	}
	if _, err := h.ShareWorker("web/admin1", "admin1", true, &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"root"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("panel admin shares a hidden worker: %v", err)
	}
	h.mu.Lock()
	rootMay := h.mayUseLocked("root", h.nodeLocked("aw"))
	h.mu.Unlock()
	if rootMay {
		t.Fatal("root may use aw")
	}
	// alice shares it with bob; root still can't add itself
	if _, err := pb.NewFleetClient(th.alice).ShareWorker(tctx(t), &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := share(tctx(t), &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"root"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("admin adds itself to a worker shared with bob: %v", err)
	}
	// shared with root by alice: root uses it, and still can't re-share it
	if _, err := pb.NewFleetClient(th.alice).ShareWorker(tctx(t), &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"root"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := nodeNames(t, th.root)["aw"]; !ok {
		t.Fatal("root does not see a worker shared with it")
	}
	if _, err := share(tctx(t), &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"carol"}, SetEveryone: true, Everyone: true}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("admin re-shares a worker shared with it: %v", err)
	}
	h.mu.Lock()
	s := h.nodeLocked("aw").rec.Shares
	h.mu.Unlock()
	if s == nil || s.Everyone || len(s.Users) != 2 {
		t.Fatalf("shares %+v", s)
	}
}

// TestAuditNodeFilter: a node filter is authorized first; unknown and
// hidden machines answer alike (nothing), and a filtered answer never
// counts other users' records.
func TestAuditNodeFilter(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	now := time.Now()
	h.mu.Lock()
	awID, w1ID := h.nodeLocked("aw").rec.ID, h.nodeLocked("w1").rec.ID
	h.mu.Unlock()
	var rows []store.AuditRow
	for i := range 3 { // as workers' records arrive: with the node's ID
		rows = append(rows, store.AuditRow{Node: "aw", NodeID: awID, Time: now.Add(time.Duration(i) * time.Millisecond), Who: "client/alice",
			Method: "/longarm.v1.Exec/Run", Line: `{"user":"alice"}`, ActorUser: "alice", SubjectUser: "alice", Attributed: true})
	}
	rows = append(rows, store.AuditRow{Node: "w1", NodeID: w1ID, Time: now, Who: "client/alice", Method: "/longarm.v1.Exec/Run",
		Line: `{"user":"alice"}`, ActorUser: "alice", SubjectUser: "alice", Attributed: true})
	if err := h.store.AddAudit(rows); err != nil {
		t.Fatal(err)
	}
	audit := func(c tenancy.Caller, node string) *pb.AuditResponse {
		r, err := h.AuditFor(c, "", &pb.AuditRequest{Node: node})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	root := tenancy.Caller{User: "root"}
	for _, node := range []string{"aw", "nosuch"} {
		if r := audit(root, node); len(r.Records) != 0 || r.HiddenTotal != 0 || len(r.Hidden) != 0 {
			t.Errorf("admin audit of %s: %v", node, r)
		}
	}
	// a visible fleet worker: own records only, no counts of others'
	if r := audit(root, "w1"); len(r.Records) != 0 || r.HiddenTotal != 0 || len(r.Hidden) != 0 {
		t.Errorf("admin audit of w1: %v", r)
	}
	// the owner sees its records
	if r := audit(tenancy.Caller{User: "alice"}, "aw"); len(r.Records) != 3 {
		t.Errorf("alice's audit of aw: %v", r)
	}
	// the gRPC call answers the same
	r, err := pb.NewFleetClient(th.root).Audit(tctx(t), &pb.AuditRequest{Node: "aw"})
	if err != nil || len(r.Records) != 0 || r.HiddenTotal != 0 {
		t.Errorf("Fleet.Audit node aw: %v %v", r, err)
	}
	// unfiltered: fleet-wide counts by method remain
	if r := audit(root, ""); r.HiddenTotal < 4 {
		t.Errorf("unfiltered hidden total %d", r.HiddenTotal)
	}
	// the hub's own records: counted like the unfiltered log
	h.auditHub("client/alice", "workers/label", map[string]any{"worker": "aw", "owner": "alice"})
	if r := audit(root, "hub"); r.HiddenTotal < 1 || len(r.Records) != 0 {
		t.Errorf("hub records for root: %v", r)
	}
}

// TestUserListUsage: user listings keep only the job count of other users'
// usage - not the memory their jobs reserve.
func TestUserListUsage(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	full := h.ListUsers()
	var bobMem float64
	for _, u := range full.Users {
		if u.Name == "bob" {
			bobMem = u.Usage.GetMemGb()
		}
	}
	if bobMem <= 0 {
		t.Fatalf("test setup: bob reserves %v GB", bobMem)
	}
	for _, u := range h.ListUsersFor(tenancy.Caller{User: "alice"}).Users {
		switch u.Name {
		case "alice":
			if u.Usage.GetMemGb() <= 0 || u.Usage.GetJobs() != 2 {
				t.Errorf("alice's own usage %v", u.Usage)
			}
		default:
			if u.Usage.GetMemGb() != 0 {
				t.Errorf("%s's memory reaches alice: %v", u.Name, u.Usage)
			}
		}
		if u.Name == "bob" && u.Usage.GetJobs() != 1 {
			t.Errorf("bob's job count %v", u.Usage)
		}
	}
	// gRPC (admin certificate) and the panel's answer to PutUser
	r, err := pb.NewHubAdminClient(th.root).ListUsers(tctx(t), &pb.ListUsersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range r.Users {
		if u.Usage.GetMemGb() != 0 {
			t.Errorf("admin ListUsers: %s reserves %v", u.Name, u.Usage)
		}
	}
	pr, err := pb.NewHubAdminClient(th.root).PutUser(tctx(t), &pb.PutUserRequest{Name: "bob"})
	if err != nil || pr.User.GetUsage().GetMemGb() != 0 {
		t.Errorf("admin PutUser bob: %v %v", pr, err)
	}
	if _, err := AddUser(h.store, "admin1", "admin-password-1", false); err != nil {
		t.Fatal(err)
	}
	pu, _ := h.store.GetUser("admin1")
	panel := panelLogin(t, h, "admin1", "admin-password-1", pu.TOTPSecret)
	resp, err := panel.Get("http://" + h.Addr("http") + "/api/tenants")
	if err != nil {
		t.Fatal(err)
	}
	var lu struct {
		Users []struct {
			Name  string `json:"name"`
			Usage struct {
				MemGb float64 `json:"mem_gb"`
			} `json:"usage"`
		} `json:"users"`
	}
	json.NewDecoder(resp.Body).Decode(&lu)
	resp.Body.Close()
	if len(lu.Users) == 0 {
		t.Fatal("no users in /api/tenants")
	}
	for _, u := range lu.Users {
		if u.Usage.MemGb != 0 {
			t.Errorf("/api/tenants: %s reserves %v", u.Name, u.Usage.MemGb)
		}
	}
}

// TestUserBoundCodesFirstDeviceOnly: an admin can't mint itself a device of
// a user who has machines (and act as that user on the user's workers): a
// user-bound join code enrolls a user's first device only.
func TestUserBoundCodesFirstDeviceOnly(t *testing.T) {
	th, _ := ownedHub(t) // alice: a device and a worker (aw)
	h := th.h
	adm := pb.NewHubAdminClient(th.root)
	if _, err := adm.PutUser(tctx(t), &pb.PutUserRequest{Name: "alice", JoinCode: true, Admin: true}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("admin user code alice: %v", err)
	}
	if _, err := adm.CreateJoinCode(tctx(t), &pb.CreateJoinCodeRequest{Role: "client", User: "alice"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("admin join-code --user alice: %v", err)
	}
	// a new user's first device: as before - and that code dies with the
	// first enrollment
	pr, err := adm.PutUser(tctx(t), &pb.PutUserRequest{Name: "dave", Create: true, JoinCode: true})
	if err != nil {
		t.Fatal(err)
	}
	spare, err := adm.CreateJoinCode(tctx(t), &pb.CreateJoinCodeRequest{Role: "client", User: "dave", Name: "dave-2"})
	if err != nil {
		t.Fatal(err)
	}
	opts := hubclient.JoinOptions{Fingerprint: pki.Fingerprint(th.ca.Cert)}
	r, err := hubclient.Join(tctx(t), h.Addr("grpc"), pr.JoinCode.Code, opts)
	if err != nil || r.Resp.State != stateApproved {
		t.Fatalf("dave's first device: %v %v", r, err)
	}
	if _, err := hubclient.Join(tctx(t), h.Addr("grpc"), spare.Code, opts); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a second code of dave's after his first device: %v", err)
	}
	// alice without machines (all revoked): a code again
	h.mu.Lock()
	for _, n := range h.nodes {
		if n.rec.Owner == "alice" || n.rec.Role != "worker" && nodeUser(n.rec) == "alice" {
			n.rec.State = stateRevoked
		}
	}
	h.mu.Unlock()
	if _, err := h.CreateJoinCode("test", &pb.CreateJoinCodeRequest{Role: "client", User: "alice"}); err != nil {
		t.Fatalf("alice without machines: %v", err)
	}
}

type fakeTailnet struct{}

func (fakeTailnet) Name() string                                      { return "fake" }
func (fakeTailnet) Mint(context.Context, mintRequest) (string, error) { return "", nil }

// TestTailnetRekeyHidden: a re-key of another user's machine by name is
// the unknown name's answer and does nothing.
func TestTailnetRekeyHidden(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	h.tailnet = fakeTailnet{}
	adm := pb.NewHubAdminClient(th.root)
	_, want := adm.TailnetRekey(tctx(t), &pb.NodeRequest{Name: "nosuch"})
	_, got := adm.TailnetRekey(tctx(t), &pb.NodeRequest{Name: "aw"})
	if status.Code(want) != codes.NotFound || status.Code(got) != codes.NotFound ||
		strings.ReplaceAll(status.Convert(got).Message(), "aw", "?") != strings.ReplaceAll(status.Convert(want).Message(), "nosuch", "?") {
		t.Fatalf("rekey aw: %v (unknown: %v)", got, want)
	}
	h.mu.Lock()
	rekey := h.nodeLocked("aw").rec.TailnetRekey
	h.mu.Unlock()
	if rekey {
		t.Fatal("aw marked for a re-key")
	}
}

// TestNoAdminCredentialsForUsers: an admin can't make itself credentials
// that act as a user who has machines - a panel login for the user, an
// administrator login named like the user, a device code bound to no user
// (its device would act as the user its name names).
func TestNoAdminCredentialsForUsers(t *testing.T) {
	th, _ := ownedHub(t) // alice has a device and a worker
	h := th.h
	adm := pb.NewHubAdminClient(th.root)
	if _, err := adm.PutUser(tctx(t), &pb.PutUserRequest{Name: "alice", Panel: true}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("admin makes alice a panel login: %v", err)
	}
	if _, err := h.store.GetUser("alice"); err == nil {
		t.Fatal("a panel login for alice exists")
	}
	// a new user: its login comes with it, as before
	if pr, err := adm.PutUser(tctx(t), &pb.PutUserRequest{Name: "erin", Create: true, Panel: true}); err != nil || pr.PanelPassword == "" {
		t.Fatalf("new user with a panel login: %v %v", pr, err)
	}
	// userless client/admin codes: none once users are separated
	for _, req := range []*pb.CreateJoinCodeRequest{{Role: "admin", Name: "alice"}, {Role: "client", Name: "alice-2"}, {Role: "client"}} {
		if _, err := adm.CreateJoinCode(tctx(t), req); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("userless code %v: %v", req, err)
		}
	}
	// ... nor do ones made before users were separated enroll
	code := newJoinCode()
	hash := codeHash(code)
	if err := h.store.AddJoinCode(hash, &store.JoinCode{ID: hash[:8], Role: "admin", Name: "alice", CreatedBy: "admin/root",
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := hubclient.Join(tctx(t), h.Addr("grpc"), code, hubclient.JoinOptions{Fingerprint: pki.Fingerprint(th.ca.Cert)}); err == nil {
		t.Fatal("a userless admin code named alice enrolled")
	}
	// panel: an administrator login can't take a user's name, nor replace a user's login
	if _, err := AddUser(h.store, "admin1", "admin-password-1", false); err != nil {
		t.Fatal(err)
	}
	pu, _ := h.store.GetUser("admin1")
	panel := panelLogin(t, h, "admin1", "admin-password-1", pu.TOTPSecret)
	body := `{"name":"alice","password":"another-password-1","current_password":"admin-password-1"}`
	resp, err := panel.Post("http://"+h.Addr("http")+"/api/users", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("admin login named alice: %d", resp.StatusCode)
	}
	if _, err := AddUser(h.store, "erin", "another-password-1", true); err == nil {
		t.Fatal("erin's user login replaced by an administrator's")
	}
	// ... nor the name of a user without a user record: an old certificate's
	// (its name is its user), a worker's owner
	h.mu.Lock()
	h.putTestNode(&node{rec: &store.Node{Name: "lega", Role: "client", State: stateApproved}})
	h.putTestNode(&node{rec: &store.Node{Name: "gw", Role: "worker", Owner: "ghost", State: stateRevoked}})
	h.mu.Unlock()
	for _, name := range []string{"lega", "ghost"} {
		body := `{"name":"` + name + `","password":"another-password-1","current_password":"admin-password-1"}`
		resp, err := panel.Post("http://"+h.Addr("http")+"/api/users", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("admin login named %s: %d", name, resp.StatusCode)
		}
		if _, err := h.store.GetUser(name); err == nil {
			t.Errorf("a panel login %s exists", name)
		}
	}
}

// TestAdminCantJoinTeams: an admin can't add its own user to a team - that
// would open the workers and jobs others share with the team (an indirect
// self-share); another admin can.
func TestAdminCantJoinTeams(t *testing.T) {
	th, _ := ownedHub(t) // alice (team t1) owns aw
	h := th.h
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "root", Create: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "alice", SetCanShare: true, CanShare: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := pb.NewFleetClient(th.alice).ShareWorker(tctx(t), &pb.ShareWorkerRequest{Worker: "aw", AddTeams: []string{"t1"}}); err != nil {
		t.Fatal(err)
	}
	adm := pb.NewHubAdminClient(th.root)
	if _, err := adm.SetTeam(tctx(t), &pb.SetTeamRequest{Team: "t1", Add: []string{"root"}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("admin adds itself to t1: %v", err)
	}
	if _, err := adm.PutUser(tctx(t), &pb.PutUserRequest{Name: "root", SetTeams: true, Teams: []string{"t1"}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("admin sets its own teams: %v", err)
	}
	if _, err := AddUser(h.store, "root", "admin-password-1", false); err != nil { // a panel administrator "root"
		t.Fatal(err)
	}
	if _, err := h.SetTeam("web/root", &pb.SetTeamRequest{Team: "t1", Add: []string{"root"}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("panel admin root adds itself to t1: %v", err)
	}
	if _, ok := nodeNames(t, th.root)["aw"]; ok {
		t.Fatal("root sees aw")
	}
	// leaving a team, and adding others: as before
	if _, err := adm.SetTeam(tctx(t), &pb.SetTeamRequest{Team: "t2", Add: []string{"bob"}, Remove: []string{"root"}}); err != nil {
		t.Fatalf("admin adds bob to t2: %v", err)
	}
	if _, err := h.SetTeam("admin/other", &pb.SetTeamRequest{Team: "t1", Add: []string{"root"}}); err != nil {
		t.Fatalf("another admin adds root: %v", err)
	}
}

// TestAdminCredentialsForUsersWithAccess: an admin's join code or panel
// login bootstraps a new user only - not a user who signs in with an
// identity provider or has workers shared with it (the credential would
// hand the admin that access), also when the access came after the code
// was made.
func TestAdminCredentialsForUsersWithAccess(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	adm := pb.NewHubAdminClient(th.root)
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "alice", SetCanShare: true, CanShare: true}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"frank", "ivy", "jo"} {
		if _, err := h.PutUser("test", &pb.PutUserRequest{Name: u, Create: true}); err != nil {
			t.Fatal(err)
		}
	}
	// a code for ivy, made while she was new
	code, err := adm.CreateJoinCode(tctx(t), &pb.CreateJoinCodeRequest{Role: "client", User: "ivy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pb.NewFleetClient(th.alice).ShareWorker(tctx(t), &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"frank", "ivy"}}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.store.SetMeta(entraUserKey("jo"), "google:1234") // jo signs in with Google
	h.mu.Unlock()
	for _, u := range []string{"frank", "jo"} {
		if _, err := adm.CreateJoinCode(tctx(t), &pb.CreateJoinCodeRequest{Role: "client", User: u}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("join-code --user %s: %v", u, err)
		}
		if _, err := adm.PutUser(tctx(t), &pb.PutUserRequest{Name: u, JoinCode: true}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("user code %s: %v", u, err)
		}
		if _, err := adm.PutUser(tctx(t), &pb.PutUserRequest{Name: u, Panel: true}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("panel login for %s: %v", u, err)
		}
	}
	// ivy's code, made before aw was shared with her: refused now
	if _, err := hubclient.Join(tctx(t), h.Addr("grpc"), code.Code, hubclient.JoinOptions{Fingerprint: pki.Fingerprint(th.ca.Cert)}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ivy's code after the share: %v", err)
	}
	// a new user without access: as before
	if _, err := adm.PutUser(tctx(t), &pb.PutUserRequest{Name: "kim", Create: true, JoinCode: true}); err != nil {
		t.Fatalf("new user with a code: %v", err)
	}
}

// TestEventStreamFollowsTeams: an open event stream sees what the
// viewer's teams share now - removed from a team, the team's worker
// leaves the stream at once.
func TestEventStreamFollowsTeams(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "alice", SetCanShare: true, CanShare: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := pb.NewFleetClient(th.alice).ShareWorker(tctx(t), &pb.ShareWorkerRequest{Worker: "aw", AddTeams: []string{"t1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetTeam("test", &pb.SetTeamRequest{Team: "t1", Add: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	f := h.eventFilter(h.viewerLocked("bob", false)) // subscribed while in t1
	info := h.nodeInfoLocked(h.nodeLocked("aw"))
	h.mu.Unlock()
	ev := &pb.FleetEvent{Kind: "node", Node: "aw", NodeInfo: info}
	if f(ev, "") == nil {
		t.Fatal("bob (t1) does not see aw's event")
	}
	if _, err := h.SetTeam("test", &pb.SetTeamRequest{Team: "t1", Remove: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
	if out := f(ev, ""); out != nil {
		t.Fatalf("bob, removed from t1, still gets aw's event: %v", out)
	}
	if out := f(&pb.FleetEvent{Kind: "metrics", Node: "aw"}, ""); out != nil {
		t.Fatalf("bob, removed from t1, still gets aw's metrics: %v", out)
	}
}
