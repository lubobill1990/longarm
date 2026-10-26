package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestTenancyRoleTable: what a client certificate may call on the hub at
// all - checked against every RPC of every longarm service. Everything else
// is the admin's (or a worker's / the release credential's); what a client
// sees through these is filtered by ownership (TestTenancyViews).
func TestTenancyRoleTable(t *testing.T) {
	clientMay := []string{
		pb.Fleet_Events_FullMethodName, pb.Fleet_GetJob_FullMethodName, pb.Fleet_History_FullMethodName,
		pb.Fleet_ListJobs_FullMethodName, pb.Fleet_ListNodes_FullMethodName, pb.Fleet_Place_FullMethodName,
		pb.Fleet_ReportLinks_FullMethodName, pb.Fleet_TailnetKey_FullMethodName, pb.Fleet_WaitJob_FullMethodName,
		pb.Fleet_Whoami_FullMethodName, pb.HubNode_Leave_FullMethodName, pb.HubNode_Renew_FullMethodName,
		pb.Telemetry_Push_FullMethodName, // the node's own, scrubbed telemetry
		pb.Diagnostics_Upload_FullMethodName,
		pb.Fleet_CreateWorkerCode_FullMethodName, pb.Fleet_ShareWorker_FullMethodName, pb.Fleet_RemoveWorker_FullMethodName,
	}
	sort.Strings(clientMay)
	var got []string
	protoregistry.GlobalFiles.RangeFilesByPackage("longarm.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := 0; i < fd.Services().Len(); i++ {
			sd := fd.Services().Get(i)
			for j := 0; j < sd.Methods().Len(); j++ {
				m := "/" + string(sd.FullName()) + "/" + string(sd.Methods().Get(j).Name())
				if slices.Contains(hubMethodRoles[m], pki.RoleClient) {
					got = append(got, m)
				}
			}
		}
		return true
	})
	sort.Strings(got)
	if !slices.Equal(got, clientMay) {
		t.Fatalf("clients may call %v\nwant %v", got, clientMay)
	}
	for _, m := range []string{pb.HubAdmin_PutUser_FullMethodName, pb.HubAdmin_ListUsers_FullMethodName,
		pb.HubAdmin_RemoveUser_FullMethodName, pb.HubAdmin_SetTeam_FullMethodName, pb.HubAdmin_ListTeams_FullMethodName,
		pb.Fleet_Audit_FullMethodName, pb.Fleet_Links_FullMethodName, pb.HubAdmin_KillJob_FullMethodName,
		pb.HubAdmin_JobLogs_FullMethodName, pb.HubAdmin_SubmitJob_FullMethodName} {
		if !slices.Equal(hubMethodRoles[m], adminOnly) {
			t.Errorf("%s: %v, want admins only", m, hubMethodRoles[m])
		}
	}
}

type tenancyHub struct {
	h                       *Hub
	ca                      *pki.CA
	alice, bob, carol, root *grpc.ClientConn
}

func newTenancyHub(t *testing.T) *tenancyHub {
	t.Helper()
	dir := t.TempDir()
	ca, err := InitCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: dir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "127.0.0.1:0"}
	cfg.Web.InsecureCookie = true
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	h, err := Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; h.Close() })
	for h.Addr("grpc") == "" || h.Addr("http") == "" {
		time.Sleep(10 * time.Millisecond)
	}
	th := &tenancyHub{h: h, ca: ca}
	dial := func(o pki.IssueOptions) *grpc.ClientConn {
		certPEM, keyPEM, err := ca.Issue(o)
		if err != nil {
			t.Fatal(err)
		}
		bdir := filepath.Join(dir, "c-"+o.Name)
		if err := pki.WriteBundle(bdir, ca.CertPEM, certPEM, keyPEM); err != nil {
			t.Fatal(err)
		}
		b, err := pki.LoadBundle(bdir)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	th.alice = dial(pki.IssueOptions{Role: pki.RoleClient, Name: "alice"})
	th.bob = dial(pki.IssueOptions{Role: pki.RoleClient, Name: "bob-laptop", User: "bob"})
	th.carol = dial(pki.IssueOptions{Role: pki.RoleClient, Name: "carol"})
	th.root = dial(pki.IssueOptions{Role: pki.RoleAdmin, Name: "root"})
	for _, u := range []*pb.PutUserRequest{
		{Name: "alice", Create: true, Teams: []string{"t1"}},
		{Name: "bob", Create: true, Quota: &pb.Quota{MaxJobs: 1}},
		{Name: "carol", Create: true, Teams: []string{"t1"}},
	} {
		if _, err := h.PutUser("test", u); err != nil {
			t.Fatal(err)
		}
	}
	// a worker with jobs of alice (one shared with t1) and bob
	h.mu.Lock()
	n := &node{rec: &store.Node{Name: "w1", Role: "worker", State: stateApproved, Isolation: tenancy.PerUser,
		Mirrors: []string{"alice/secret-repo", "bob/bobs-repo"}},
		metrics: &pb.MetricsSnapshot{TopCpu: []*pb.ProcGroup{{Name: "alices-build"}}}}
	h.nodes["w1"] = n
	h.applyJobsLocked(n, &pb.JobsReport{Jobs: []*pb.Job{
		{Spec: &pb.JobSpec{Id: "JA", Owner: "alice"}, State: pb.JobState_JOB_STATE_RUNNING},
		{Spec: &pb.JobSpec{Id: "JS", Owner: "alice", ShareTeams: []string{"t1"}}, State: pb.JobState_JOB_STATE_RUNNING},
		{Spec: &pb.JobSpec{Id: "JB", Owner: "bob"}, State: pb.JobState_JOB_STATE_RUNNING},
	}})
	h.mu.Unlock()
	return th
}

func tctx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func jobIDs(t *testing.T, conn *grpc.ClientConn) []string {
	t.Helper()
	r, err := pb.NewFleetClient(conn).ListJobs(tctx(t), &pb.FleetJobsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, j := range r.Jobs {
		ids = append(ids, j.Job.Spec.Id)
	}
	sort.Strings(ids)
	return ids
}

// TestTenancyViews: the hub shows every user its own jobs (and those shared
// with its teams), nodes without others' process lists and mirrors, and
// the user/team administration to admins only.
func TestTenancyViews(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	for _, c := range []struct {
		conn *grpc.ClientConn
		want []string
	}{{th.alice, []string{"JA", "JS"}}, {th.bob, []string{"JB"}}, {th.carol, []string{"JS"}}, {th.root, []string{"JA", "JB", "JS"}}} {
		if got := jobIDs(t, c.conn); !slices.Equal(got, c.want) {
			t.Errorf("jobs %v, want %v", got, c.want)
		}
	}
	fleet := func(c *grpc.ClientConn) pb.FleetClient { return pb.NewFleetClient(c) }
	if _, err := fleet(th.bob).GetJob(tctx(t), &pb.FleetJobRequest{Id: "JA"}); status.Code(err) != codes.NotFound {
		t.Errorf("bob gets JA: %v", err)
	}
	if _, err := fleet(th.carol).GetJob(tctx(t), &pb.FleetJobRequest{Id: "JS"}); err != nil {
		t.Errorf("carol gets the shared JS: %v", err)
	}
	waitAppear = 200 * time.Millisecond
	st, err := fleet(th.bob).WaitJob(tctx(t), &pb.FleetJobRequest{Id: "JA"})
	if err == nil {
		_, err = st.Recv()
	}
	if status.Code(err) != codes.NotFound {
		t.Errorf("bob waits for JA: %v", err)
	}

	// nodes: no process lists, only one's own mirrors
	nl, err := fleet(th.bob).ListNodes(tctx(t), &pb.ListNodesRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nl.Nodes {
		if n.Role != "worker" && n.User != "bob" {
			t.Errorf("bob sees node %s of %s", n.Name, n.User)
		}
		if n.Name == "w1" && (n.Metrics.GetTopCpu() != nil || !slices.Equal(n.Mirrors, []string{"bobs-repo"})) {
			t.Errorf("bob's view of w1: top %v mirrors %v", n.Metrics.GetTopCpu(), n.Mirrors)
		}
	}
	nl, _ = fleet(th.root).ListNodes(tctx(t), &pb.ListNodesRequest{})
	if len(nl.Nodes) != 1 || len(nl.Nodes[0].Metrics.GetTopCpu()) != 1 || len(nl.Nodes[0].Mirrors) != 2 {
		t.Errorf("admin's view of w1: %v", nl.Nodes)
	}

	// events: bob gets his job's, not alice's
	ev, err := fleet(th.bob).Events(tctx(t), &pb.EventsRequest{Kinds: []string{"job", "node"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // subscribed
	h.mu.Lock()
	h.applyJobsLocked(h.nodes["w1"], &pb.JobsReport{Jobs: []*pb.Job{
		{Spec: &pb.JobSpec{Id: "JA", Owner: "alice"}, State: pb.JobState_JOB_STATE_DONE},
		{Spec: &pb.JobSpec{Id: "JB", Owner: "bob"}, State: pb.JobState_JOB_STATE_DONE},
	}})
	h.publishNodeLocked(h.nodes["w1"], "test")
	h.mu.Unlock()
	var seen []string
	for len(seen) < 2 {
		e, err := ev.Recv()
		if err != nil {
			t.Fatal(err)
		}
		switch e.Kind {
		case "job":
			seen = append(seen, e.Job.Job.Spec.Id)
		case "node":
			if e.NodeInfo.GetMetrics().GetTopCpu() != nil {
				t.Errorf("bob's node event shows process lists")
			}
			seen = append(seen, "node")
		}
	}
	if slices.Contains(seen, "JA") || !slices.Contains(seen, "JB") {
		t.Errorf("bob's events: %v", seen)
	}

	// admin-only: users, teams, audit
	adm := func(c *grpc.ClientConn) pb.HubAdminClient { return pb.NewHubAdminClient(c) }
	if _, err := adm(th.bob).ListUsers(tctx(t), &pb.ListUsersRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob lists users: %v", err)
	}
	if _, err := adm(th.bob).PutUser(tctx(t), &pb.PutUserRequest{Name: "bob", SetQuota: true}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob changes his quota: %v", err)
	}
	if _, err := fleet(th.bob).Audit(tctx(t), &pb.AuditRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob reads the audit: %v", err)
	}
	lu, err := adm(th.root).ListUsers(tctx(t), &pb.ListUsersRequest{})
	if err != nil || len(lu.Users) != 3 || !lu.Tenancy {
		t.Fatalf("users: %v %v", lu, err)
	}
	for _, u := range lu.Users {
		if u.Name == "bob" && (!slices.Equal(u.Devices, []string{"bob-laptop"}) || u.Usage.GetJobs() != 0) {
			t.Errorf("bob: %+v", u)
		}
	}
	teams, err := adm(th.root).ListTeams(tctx(t), &pb.ListTeamsRequest{})
	if err != nil || len(teams.Teams) != 1 || !slices.Equal(teams.Teams[0].Members, []string{"alice", "carol"}) {
		t.Fatalf("teams %v %v", teams, err)
	}

	// whoami
	who, err := fleet(th.bob).Whoami(tctx(t), &pb.WhoamiRequest{})
	if err != nil || who.User != "bob" || who.Quota.GetMaxJobs() != 1 || !who.Tenancy {
		t.Fatalf("whoami %v %v", who, err)
	}
}

// TestTenancyQuotaAndPolicy: fleet-wide quotas in the scheduler, the
// policy for workers, disabling and removing users.
func TestTenancyQuotaAndPolicy(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	h.mu.Lock()
	why := h.overQuotaLocked("bob", 1) // JB runs; max_jobs 1
	ok := h.overQuotaLocked("alice", 1)
	p := h.policyLocked()
	h.mu.Unlock()
	if !strings.Contains(why, "quota") || ok != "" {
		t.Fatalf("quota: bob %q alice %q", why, ok)
	}
	if !p.Enabled || len(p.Users) != 3 || p.Version == 0 {
		t.Fatalf("policy %v", p)
	}
	// placement: bob's request fails on the quota, not on the gate
	err := h.PlaceFor(tctx(t), "client/bob-laptop", "bob", &pb.PlaceRequest{JobId: "J2", NoQueue: true}, func(*pb.PlaceUpdate) error { return nil })
	if status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("place: %v", err)
	}
	// a disabled user is refused
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "bob", SetDisabled: true, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := pb.NewFleetClient(th.bob).Whoami(tctx(t), &pb.WhoamiRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("disabled bob: %v", err)
	}
	h.mu.Lock()
	disabledInPolicy := false
	for _, u := range h.policyLocked().Users {
		disabledInPolicy = disabledInPolicy || (u.Name == "bob" && u.Disabled)
	}
	h.mu.Unlock()
	if !disabledInPolicy {
		t.Fatal("the workers' policy does not disable bob")
	}
	// removing with purge revokes bob's devices and tells workers to purge
	r, err := h.RemoveUser("test", &pb.RemoveUserRequest{Name: "bob", Purge: true})
	if err != nil || !slices.Equal(r.Revoked, []string{"bob-laptop"}) {
		t.Fatalf("remove: %v %v", r, err)
	}
	h.mu.Lock()
	p = h.policyLocked()
	h.mu.Unlock()
	if !slices.Contains(p.Purge, "bob") {
		t.Fatalf("purge list %v", p.Purge)
	}
	// a join code for a user's device carries the user into the certificate
	pr, err := h.PutUser("test", &pb.PutUserRequest{Name: "dave", Create: true, JoinCode: true})
	if err != nil || pr.JoinCode.GetUser() != "dave" || pr.JoinCode.GetName() != "dave" || pr.JoinCode.GetCode() == "" {
		t.Fatalf("join code %v %v", pr, err)
	}
}

// TestTenancyPanelRoles: a panel login of a user sees its own jobs and
// cannot use the administration endpoints.
func TestTenancyPanelRoles(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	pr, err := h.PutUser("test", &pb.PutUserRequest{Name: "carol", Panel: true})
	if err != nil || pr.PanelPassword == "" {
		t.Fatalf("panel login: %v %v", pr, err)
	}
	if _, err := AddUser(h.store, "admin1", "admin-password-1", false); err != nil {
		t.Fatal(err)
	}
	u, _ := h.store.GetUser("admin1")
	login := func(user, pass, secret string) *http.Client {
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar}
		code, _ := TOTP(secret, time.Now())
		body, _ := json.Marshal(map[string]string{"user": user, "password": pass, "totp": code})
		resp, err := c.Post("http://"+h.Addr("http")+"/api/login", "application/json", bytes.NewReader(body))
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("login %s: %v %v", user, err, resp)
		}
		resp.Body.Close()
		return c
	}
	get := func(c *http.Client, path string) (int, string) {
		resp, err := c.Get("http://" + h.Addr("http") + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	carol := login("carol", pr.PanelPassword, pr.TotpSecret)
	admin := login("admin1", "admin-password-1", u.TOTPSecret)
	if code, body := get(carol, "/api/jobs"); code != 200 || !strings.Contains(body, `"JS"`) || strings.Contains(body, `"JA"`) || strings.Contains(body, `"JB"`) {
		t.Errorf("carol's jobs: %d %s", code, body)
	}
	if code, body := get(carol, "/api/me"); code != 200 || !strings.Contains(body, `"role": "user"`) || !strings.Contains(body, `"tenant": "carol"`) {
		t.Errorf("carol's me: %d %s", code, body)
	}
	for _, p := range []string{"/api/audit", "/api/tenants", "/api/users", "/api/joincodes", "/api/links", "/api/profiles"} {
		if code, _ := get(carol, p); code != http.StatusForbidden {
			t.Errorf("carol GET %s: %d", p, code)
		}
	}
	if code, body := get(admin, "/api/jobs"); code != 200 || !strings.Contains(body, `"JA"`) || !strings.Contains(body, `"JB"`) {
		t.Errorf("admin's jobs: %d %s", code, body)
	}
	if code, body := get(admin, "/api/tenants"); code != 200 || !strings.Contains(body, `"carol"`) {
		t.Errorf("admin's tenants: %d %s", code, body)
	}
}
