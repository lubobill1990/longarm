package hub

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// addWorker puts an online worker into h (fake session, plenty of memory).
func addWorker(h *Hub, name, owner string, admin bool, version string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := &node{rec: &store.Node{Name: name, Role: "worker", State: stateApproved, Owner: owner, Admin: admin, Version: version,
		OS: "linux", Hostname: name + ".corp", Labels: map[string]string{"secret-label": name}},
		sess:    &nodeSession{id: 7, out: make(chan *pb.HubCommand, 64), cancel: func() {}},
		metrics: &pb.MetricsSnapshot{MemTotalBytes: 64 << 30, MemUsedBytes: 1 << 30, CpuPercent: 1, TopCpu: []*pb.ProcGroup{{Name: name + "-proc"}}}}
	if owner != "" {
		n.rec.AccessVersion = 1
	}
	h.nodes[name] = n
	h.indexNode(n.rec)
}

// TestAdminSeesUserView: with multi-tenancy an admin certificate sees
// exactly its user's view - own workers, workers shared with it, fleet
// workers - in ListNodes, History and events; other users' private and
// admin workers are not there, whatever the request asks for.
func TestAdminSeesUserView(t *testing.T) {
	th, _ := ownedHub(t) // alice's "aw" (private), fleet "w1"
	h := th.h
	addWorker(h, "rw", "root", false, "1.0.0")       // the admin's own worker
	addWorker(h, "aw-admin", "alice", true, "1.0.0") // alice's admin worker
	addWorker(h, "bw", "bob", false, "1.0.0")        // bob's, shared with root below

	names := func(m map[string]*pb.NodeInfo) []string {
		var out []string
		for n := range m {
			out = append(out, n)
		}
		slices.Sort(out)
		return out
	}
	if got := names(nodeNames(t, th.root)); !slices.Equal(got, []string{"rw", "w1"}) {
		t.Fatalf("admin's workers %v, want [rw w1]", got)
	}
	if got := names(nodeNames(t, th.alice)); !slices.Equal(got, []string{"aw", "aw-admin", "w1"}) {
		t.Fatalf("alice's workers %v", got)
	}
	// shared with the admin: visible like any shared worker
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "root", Create: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ShareWorker("test", "bob", true, &pb.ShareWorkerRequest{Worker: "bw", AddUsers: []string{"root"}}); err != nil {
		t.Fatal(err)
	}
	m := nodeNames(t, th.root)
	if got := names(m); !slices.Equal(got, []string{"bw", "rw", "w1"}) {
		t.Fatalf("admin's workers after the share %v", got)
	}
	if bw := m["bw"]; bw.Metrics.GetTopCpu() != nil || bw.Shares != nil {
		t.Errorf("admin's view of bob's shared worker: %v", bw)
	}
	// ListNodes all: no other user's devices either
	r, err := pb.NewFleetClient(th.root).ListNodes(tctx(t), &pb.ListNodesRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range r.Nodes {
		if n.Name == "aw" || n.Name == "aw-admin" || (n.Role != "worker" && nodeUserOf(n) != "root") {
			t.Errorf("admin sees %s (%s of %s%s)", n.Name, n.Role, n.Owner, n.User)
		}
	}
	// history
	v := h.viewer(caller{id: pki.Identity{Role: pki.RoleAdmin, Name: "root"}, user: "root"})
	if v.Admin || v.User != "root" {
		t.Fatalf("admin viewer %+v", v)
	}
	for _, hn := range h.HistoryFor(v, "").Nodes {
		if hn.Node == "aw" || hn.Node == "aw-admin" {
			t.Errorf("admin gets the history of %s", hn.Node)
		}
	}

	// events: an admin's stream carries no other user's node details
	ev, err := pb.NewFleetClient(th.root).Events(tctx(t), &pb.EventsRequest{Kinds: []string{"node", "metrics", "job", "queue"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // subscribed
	h.mu.Lock()
	h.applyJobsLocked(h.nodes["aw"], &pb.JobsReport{Jobs: []*pb.Job{{Spec: &pb.JobSpec{Id: "JAW2", Owner: "alice", Argv: []string{"secret"}}, State: pb.JobState_JOB_STATE_RUNNING}}})
	h.publishNodeLocked(h.nodes["aw"], "test")
	h.publishNodeLocked(h.nodes["aw-admin"], "test")
	h.publishNodeLocked(h.nodes["w1"], "test")
	h.mu.Unlock()
	for {
		e, err := ev.Recv()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := protojson.Marshal(e)
		if strings.Contains(string(b), "aw") || strings.Contains(string(b), "JAW2") {
			t.Fatalf("admin's event names alice's machine or job: %s", b)
		}
		if e.Kind == "node" && e.Node == "w1" {
			if e.NodeInfo.GetMetrics().GetTopCpu() != nil {
				t.Errorf("admin's w1 event carries process lists")
			}
			break // w1 came after aw's events: those were dropped
		}
	}
}

// TestAdminPlacement: auto placement for an admin never uses another
// user's private (or admin) worker - only its own, shared and fleet workers.
func TestAdminPlacement(t *testing.T) {
	th, _ := ownedHub(t) // alice's aw is online with free memory; w1 is offline
	h := th.h
	addWorker(h, "aw-admin", "alice", true, "")
	for range 5 {
		w, err := place(t, h, "root")
		if err == nil || status.Code(err) != codes.ResourceExhausted || strings.Contains(err.Error(), "aw") {
			t.Fatalf("admin placed on %q: %v", w, err)
		}
	}
	addWorker(h, "rw", "root", false, "")
	for range 5 {
		if w, err := place(t, h, "root"); err != nil || w != "rw" {
			t.Fatalf("admin placed on %q: %v", w, err)
		}
	}
}

// TestFleetSummary: per user counts by role and state, anonymous rows and a
// version histogram - never names, host names, labels, metrics or jobs.
func TestFleetSummary(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	addWorker(h, "a1", "alice", false, "0.2.0")
	addWorker(h, "a2", "alice", false, "0.1.0")
	addWorker(h, "a1-admin", "alice", true, "0.2.0")
	addWorker(h, "f1", "", false, "0.2.0")
	h.mu.Lock()
	h.nodes["a2"].sess = nil // offline
	h.nodes["a2"].rec.LastSeen = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	h.nodes["bob-laptop"] = &node{rec: &store.Node{Name: "bob-laptop", Role: "client", State: stateApproved, User: "bob", Version: "0.2.0"}}
	h.nodes["gone"] = &node{rec: &store.Node{Name: "gone", Role: "worker", State: stateRevoked, Owner: "alice"}}
	h.mu.Unlock()

	s := h.FleetSummary(true)
	if !s.Tenancy {
		t.Error("tenancy off")
	}
	by := map[string]*pb.UserFleetSummary{}
	var order []string
	for _, u := range s.Users {
		by[u.User] = u
		order = append(order, u.User)
	}
	if !slices.Equal(order, []string{"alice", "bob", "carol", ""}) {
		t.Fatalf("users %v", order)
	}
	a := by["alice"].Counts
	if a.Workers != 2 || a.WorkersOnline != 1 || a.AdminWorkers != 1 || a.AdminWorkersOnline != 1 || a.Clients != 0 {
		t.Errorf("alice %v", a)
	}
	if b := by["bob"].Counts; b.Clients != 1 || b.Workers != 0 {
		t.Errorf("bob %v", b)
	}
	if f := by[""].Counts; f.Workers != 2 || f.WorkersOnline != 1 { // w1 (offline) and f1
		t.Errorf("fleet %v", f)
	}
	if s.Totals.Workers != 4 || s.Totals.AdminWorkers != 1 || s.Totals.Clients != 1 {
		t.Errorf("totals %v", s.Totals)
	}
	// rows: role, state, version, OS, last seen; sorted by those only
	var rows []string
	for _, m := range by["alice"].Machines {
		rows = append(rows, m.Role+"/"+map[bool]string{true: "on", false: "off"}[m.Online]+"/"+m.Version)
	}
	if !slices.Equal(rows, []string{"admin-worker/on/0.2.0", "worker/on/0.2.0", "worker/off/0.1.0"}) {
		t.Errorf("alice's rows %v", rows)
	}
	for _, m := range by["alice"].Machines {
		if m.LastSeen != nil && m.LastSeen.AsTime().Second() != 0 {
			t.Errorf("last seen not truncated: %v", m.LastSeen.AsTime())
		}
	}
	// versions: newest first, workers only
	var vs []string
	for _, v := range s.WorkerVersions {
		vs = append(vs, v.Version)
	}
	if !slices.Equal(vs, []string{"0.2.0", "0.1.0", ""}) || s.WorkerVersions[0].Workers != 3 {
		t.Errorf("versions %v", s.WorkerVersions)
	}
	b, _ := protojson.Marshal(s)
	for _, leak := range []string{"a1", "a2", "f1", "w1", "bob-laptop", "gone", ".corp", "secret-label", "-proc", "alices-build", "secret-repo", "JA"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("the summary names %q: %s", leak, b)
		}
	}

	// gRPC: admins only
	if _, err := pb.NewHubAdminClient(th.bob).FleetSummary(tctx(t), &pb.FleetSummaryRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob asks for the summary: %v", err)
	}
	if r, err := pb.NewHubAdminClient(th.root).FleetSummary(tctx(t), &pb.FleetSummaryRequest{}); err != nil || len(r.Users) < 4 || r.Users[0].Machines != nil {
		t.Errorf("admin's summary without rows: %v %v", r, err)
	}
}

// TestAdminAnswersNameOnlyVisibleNodes: rollouts, profiles, link reports
// and join codes name only the nodes the admin may see as a user.
func TestAdminAnswersNameOnlyVisibleNodes(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	addWorker(h, "rw", "root", false, "")
	h.mu.Lock()
	h.nodes["root"] = &node{rec: &store.Node{Name: "root", Role: "admin", State: stateApproved}}
	h.nodes["alice"] = &node{rec: &store.Node{Name: "alice", Role: "client", State: stateApproved}}
	h.mu.Unlock()
	v := tenancy.Caller{User: "root"}
	h.mu.Lock()
	r := h.rolloutForLocked(v, &pb.Rollout{Canary: []string{"aw"}, Targeted: []string{"aw", "rw", "w1"}, Waiting: []string{"aw"},
		Message: "canary aw healthy; waiting"})
	h.mu.Unlock()
	if !slices.Equal(r.Canary, nil) || !slices.Equal(r.Targeted, []string{"rw", "w1"}) || r.TargetedCount != 3 || r.CanaryCount != 1 ||
		r.WaitingCount != 1 || strings.Contains(r.Message, "aw") {
		t.Errorf("rollout %v", r)
	}
	h.mu.Lock()
	h.profiles["p"] = &pb.Profile{Name: "p", Canary: "aw", Message: "canary aw applying"}
	h.mu.Unlock()
	for _, p := range h.ProfilesFor(v) {
		if p.Name == "p" && (p.Canary == "aw" || strings.Contains(p.Message, "aw ")) {
			t.Errorf("profile %v", p)
		}
	}
	h.ReportLinks("alice", &pb.LinkReport{Links: []*pb.WorkerLink{{Worker: "aw"}}})
	h.ReportLinks("root", &pb.LinkReport{Links: []*pb.WorkerLink{{Worker: "aw"}, {Worker: "w1"}}})
	lr, err := pb.NewFleetClient(th.root).Links(tctx(t), &pb.LinksRequest{})
	if err != nil || len(lr.Reports) != 1 || lr.Reports[0].Client != "root" || len(lr.Reports[0].Links) != 1 || lr.Reports[0].Links[0].Worker != "w1" {
		t.Errorf("links %v %v", lr, err)
	}
	// alice's worker code (ownedHub) is hers: not in the admin's list
	jc, err := pb.NewHubAdminClient(th.root).ListJoinCodes(tctx(t), &pb.ListJoinCodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range jc.Codes {
		if c.Owner == "alice" || c.UsedBy == "aw" {
			t.Errorf("admin lists alice's code %v", c)
		}
	}
}
