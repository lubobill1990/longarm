package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// xavierSecrets are what an admin with no view of xavier's machines must
// never receive: his device and worker names, job ids, arguments, session
// and process names.
var xavierSecrets = []string{"xavlap-q9", "xavadm-q9", "xavws-q9", "XAVJOBQ9", "XAVJOBFLQ9", "xavarg-q9", "xavsess-q9", "xavproc-q9"}

// TestAdminLearnsNothingOfAnotherUsersMachines: a scripted scenario - user
// xavier enrolls devices (one an admin certificate) and a worker, runs jobs
// on his worker and a fleet worker, gets audited, labels the fleet worker,
// creates a port link and a profile canary, is in a rollout, reports links,
// queues a job, and is removed. The admin root - with no view of xavier's
// machines - subscribes to every event kind (gRPC and the panel's SSE) and
// then calls every read API; nothing it receives may name xavier's
// machines, jobs or arguments.
func TestAdminLearnsNothingOfAnotherUsersMachines(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "xavier", Create: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := AddUser(h.store, "admin1", "admin-password-1", false); err != nil {
		t.Fatal(err)
	}
	pu, _ := h.store.GetUser("admin1")
	panel := panelLogin(t, h, "admin1", "admin-password-1", pu.TOTPSecret)

	var mu sync.Mutex
	var seen bytes.Buffer
	record := func(what string, v any) {
		mu.Lock()
		defer mu.Unlock()
		switch x := v.(type) {
		case proto.Message:
			b, _ := protojson.Marshal(x)
			fmt.Fprintf(&seen, "%s: %s\n", what, b)
		case error:
			if x != nil {
				fmt.Fprintf(&seen, "%s: error %v\n", what, x)
			}
		default:
			fmt.Fprintf(&seen, "%s: %v\n", what, x)
		}
	}

	// subscribe: every event kind, over gRPC and the panel's SSE
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	kinds := []string{"job", "node", "queue", "metrics", "links", "release", "profile", "event"}
	evs, err := pb.NewFleetClient(th.root).Events(ctx, &pb.EventsRequest{Kinds: kinds})
	if err != nil {
		t.Fatal(err)
	}
	grpcDone := make(chan struct{})
	go func() {
		defer close(grpcDone)
		for {
			e, err := evs.Recv()
			if err != nil {
				return
			}
			record("event", e)
			if e.Kind == "node" && e.Message == "control" {
				return
			}
		}
	}()
	sseReq, _ := http.NewRequestWithContext(ctx, "GET", "http://"+h.Addr("http")+"/api/events?kinds="+strings.Join(kinds, ","), nil)
	sseResp, err := panel.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	sseDone := make(chan struct{})
	go func() {
		defer close(sseDone)
		defer sseResp.Body.Close()
		sc := bufio.NewScanner(sseResp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			record("sse", sc.Text())
			if strings.Contains(sc.Text(), `"control"`) {
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond) // subscribed

	// --- the scenario
	put := func(r *store.Node) *node {
		if err := h.store.PutNode(r); err != nil { // indexes the name, as enrollment does
			t.Fatal(err)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		n := &node{rec: r}
		h.nodes[r.Name] = n
		return n
	}
	put(&store.Node{Name: "xavlap-q9", Role: "client", State: stateApproved, User: "xavier", Version: "0.2.0"})
	put(&store.Node{Name: "xavadm-q9", Role: "admin", State: stateApproved, User: "xavier"})
	xw := put(&store.Node{Name: "xavws-q9", Role: "worker", State: stateApproved, Owner: "xavier", AccessVersion: 1, Hostname: "xavws-q9.corp"})
	h.mu.Lock()
	xw.sess = &nodeSession{id: 5, out: make(chan *pb.HubCommand, 256), cancel: func() {}}
	xw.metrics = &pb.MetricsSnapshot{MemTotalBytes: 8 << 30, TopCpu: []*pb.ProcGroup{{Name: "xavproc-q9"}},
		Agents: map[string]*pb.AgentStat{"claude": {Sessions: 2}}}
	for _, name := range []string{"xavlap-q9", "xavadm-q9", "xavws-q9"} {
		h.publishNodeLocked(h.nodes[name], "online")
	}
	h.bus.publish(&pb.FleetEvent{Kind: "metrics", Node: "xavws-q9", NodeInfo: h.nodeInfoLocked(xw)})
	h.bus.publish(&pb.FleetEvent{Kind: "node", Node: "xavws-q9", Message: "xavws-q9 updated by client/xavlap-q9"}) // no NodeInfo
	// jobs on his worker and on the fleet worker
	h.applyJobsLocked(xw, &pb.JobsReport{Jobs: []*pb.Job{{Spec: &pb.JobSpec{Id: "XAVJOBQ9", Owner: "xavier", Argv: []string{"xavarg-q9"},
		SubmittedBy: "client/xavlap-q9"}, State: pb.JobState_JOB_STATE_RUNNING}}})
	h.applyJobsLocked(h.nodes["w1"], &pb.JobsReport{Jobs: []*pb.Job{{Spec: &pb.JobSpec{Id: "XAVJOBFLQ9", Owner: "xavier", Argv: []string{"xavarg-q9"},
		SubmittedBy: "client/xavlap-q9"}, State: pb.JobState_JOB_STATE_RUNNING}}})
	// audit from both workers
	line := `{"time":"2026-10-29T10:00:00Z","who":"client/xavlap-q9","user":"xavier","method":"/longarm.v1.Sessions/Attach","session":"xavsess-q9","argv":["xavarg-q9"]}`
	h.storeAuditLocked(xw, nil, &pb.AuditBatch{Lines: []string{line}})
	h.storeAuditLocked(h.nodes["w1"], nil, &pb.AuditBatch{Lines: []string{strings.Replace(line, `"user":"xavier",`, "", 1)}})
	// a port link of his (fleet worker -> his worker), a profile canary, a rollout
	h.portLinks["XLINKQ9"] = &portLinkEntry{l: &pb.PortLink{Id: "XLINKQ9", Owner: "xavier", From: "w1", To: "xavws-q9", Port: 7777,
		ListenPort: 17777, JobId: "XAVJOBQ9", CreatedBy: "client/xavlap-q9"}}
	h.profiles["xp"] = &pb.Profile{Name: "xp", Tools: []*pb.ToolSpec{{Name: "git", Version: "latest"}}, Revision: 1, Rollout: "canary",
		Canary: "xavws-q9", Message: "canary xavws-q9 applying"}
	h.rollout = &pb.Rollout{Version: "9.9.9", State: "canary", Canary: []string{"xavws-q9"}, Targeted: []string{"xavws-q9", "w1"},
		Message: "canary xavws-q9 healthy"}
	h.bus.publish(&pb.FleetEvent{Kind: "release", Message: "rollout of 9.9.9: canary xavws-q9"})
	h.bus.publish(&pb.FleetEvent{Kind: "profile", Node: "xavws-q9", Message: "profile xp canary xavws-q9"})
	h.bus.publish(&pb.FleetEvent{Kind: "event", Node: "xavlap-q9", Message: "xavlap-q9 did something"})
	h.mu.Unlock()
	// a join code his admin device created for the fleet, one for his laptop
	if _, err := h.CreateJoinCode("admin/xavadm-q9", &pb.CreateJoinCodeRequest{Role: "worker"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.CreateJoinCode("admin/xavadm-q9", &pb.CreateJoinCodeRequest{Role: "client", User: "xavier", Name: "xavlap-q9"}); err != nil {
		t.Fatal(err)
	}
	// his admin device labels the fleet worker (node event "... by admin/xavadm-q9")
	if _, err := h.SetLabels("admin/xavadm-q9", "w1", map[string]string{"zone": "x"}); err != nil {
		t.Fatal(err)
	}
	h.ReportLinks("xavlap-q9", &pb.LinkReport{Links: []*pb.WorkerLink{{Worker: "xavws-q9"}, {Worker: "w1"}}})
	h.bus.publishOwned(&pb.FleetEvent{Kind: "queue", Node: "xavws-q9", Message: "job XAVJOBQ9 queued on xavws-q9"}, "xavier")
	// root unchanged-tools profile update: the answer is projected too
	pr, err := pb.NewHubAdminClient(th.root).PutProfile(tctx(t), &pb.Profile{Name: "xp", Tools: []*pb.ToolSpec{{Name: "git", Version: "latest"}}})
	record("PutProfile", pr)
	record("PutProfile err", err)
	// root deletes his link by id (an ack), then removes him (cascades)
	pl, err := pb.NewFleetClient(th.root).RemovePortLink(tctx(t), &pb.RemovePortLinkRequest{Id: "XLINKQ9"})
	record("RemovePortLink", pl)
	record("RemovePortLink err", err)
	h.mu.Lock()
	h.portLinks["XLINK2Q9"] = &portLinkEntry{l: &pb.PortLink{Id: "XLINK2Q9", Owner: "xavier", From: "w1", To: "xavws-q9", Port: 7778, ListenPort: 17778}}
	h.mu.Unlock()
	kr, err := pb.NewHubAdminClient(th.root).KillJob(tctx(t), &pb.FleetJobRequest{Id: "XAVJOBFLQ9"})
	record("KillJob", kr)
	record("KillJob err", err)
	if _, err := h.RemoveUser("admin/root", &pb.RemoveUserRequest{Name: "xavier", Purge: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	h.mu.Lock()
	h.publishNodeLocked(h.nodes["w1"], "control")
	h.mu.Unlock()
	for _, done := range []chan struct{}{grpcDone, sseDone} {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("events did not arrive")
		}
	}

	// --- every read API, as root (gRPC) and admin1 (panel)
	f, a := pb.NewFleetClient(th.root), pb.NewHubAdminClient(th.root)
	c := tctx(t)
	call := func(what string, m proto.Message, err error) {
		record(what, m)
		record(what+" err", err)
	}
	r1, e1 := f.ListNodes(c, &pb.ListNodesRequest{All: true})
	call("ListNodes", r1, e1)
	r2, e2 := f.ListJobs(c, &pb.FleetJobsRequest{})
	call("ListJobs", r2, e2)
	r3, e3 := f.GetJob(c, &pb.FleetJobRequest{Id: "XAVJOBQ9"})
	call("GetJob", r3, e3)
	r4, e4 := f.Audit(c, &pb.AuditRequest{Limit: 2000})
	call("Audit", r4, e4)
	r5, e5 := f.Links(c, &pb.LinksRequest{})
	call("Links", r5, e5)
	r6, e6 := f.History(c, &pb.HistoryRequest{})
	call("History", r6, e6)
	r7, e7 := f.ListPortLinks(c, &pb.ListPortLinksRequest{All: true})
	call("ListPortLinks", r7, e7)
	r8, e8 := f.Whoami(c, &pb.WhoamiRequest{})
	call("Whoami", r8, e8)
	r9, e9 := a.ListJoinCodes(c, &pb.ListJoinCodesRequest{})
	call("ListJoinCodes", r9, e9)
	r10, e10 := a.ListProfiles(c, &pb.ListProfilesRequest{})
	call("ListProfiles", r10, e10)
	r11, e11 := a.ListUsers(c, &pb.ListUsersRequest{})
	call("ListUsers", r11, e11)
	r12, e12 := a.ListReleases(c, &pb.ListReleasesRequest{IncludeIncomplete: true})
	call("ListReleases", r12, e12)
	r13, e13 := a.GetRollout(c, &pb.GetRolloutRequest{})
	call("GetRollout", r13, e13)
	r14, e14 := a.TailnetStatus(c, &pb.TailnetStatusRequest{})
	call("TailnetStatus", r14, e14)
	r15, e15 := a.FleetSummary(c, &pb.FleetSummaryRequest{Rows: true})
	call("FleetSummary", r15, e15)
	r16, e16 := a.ListTeams(c, &pb.ListTeamsRequest{})
	call("ListTeams", r16, e16)
	for _, path := range []string{"/api/nodes?all=1", "/api/jobs", "/api/jobs/XAVJOBQ9", "/api/audit?limit=2000", "/api/joincodes",
		"/api/tenants", "/api/teams", "/api/profiles", "/api/links", "/api/port-links", "/api/history", "/api/admin/summary?rows=1",
		"/status", "/api/users", "/api/me"} {
		resp, err := panel.Get("http://" + h.Addr("http") + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		record(strings.ReplaceAll(path, "XAVJOBQ9", "<asked>"), fmt.Sprintf("%d %s", resp.StatusCode, b)) // the URL is the admin's own question
	}

	mu.Lock()
	all := seen.String()
	mu.Unlock()
	// the admin's own question echoed back the same as for an unknown id is
	// no disclosure
	for _, id := range []string{"XAVJOBQ9", "XAVJOBFLQ9"} {
		all = strings.ReplaceAll(all, "job "+id+" not known to the hub", "job <asked> not known to the hub")
	}
	for _, s := range xavierSecrets {
		if strings.Contains(all, s) {
			var lines []string
			for _, l := range strings.Split(all, "\n") {
				if strings.Contains(l, s) {
					lines = append(lines, l)
				}
			}
			t.Errorf("the admin received %q:\n%s", s, strings.Join(lines, "\n"))
		}
	}
	// the scenario did reach the admin's views (as counts)
	// ... and actors were projected to their user ("labels changed by admin xavier")
	if !strings.Contains(all, "control") || !strings.Contains(all, "hidden_total") || !strings.Contains(all, "admin xavier") {
		t.Errorf("scenario incomplete:\n%s", all)
	}
}

// panelLogin logs a panel account in (password + TOTP).
func panelLogin(t *testing.T, h *Hub, user, pass, secret string) *http.Client {
	t.Helper()
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

// TestEventProjectionAllKinds: every event kind the hub publishes goes
// through the projector; for a viewer without access to xavier's machines
// none of them carries his names - whether the kind is dropped or projected.
// The kinds come from the hub's own source, so a new kind can't skip this.
func TestEventProjectionAllKinds(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	for _, r := range []*store.Node{
		{Name: "xavlap-q9", Role: "client", State: stateApproved, User: "xavier"},
		{Name: "xavws-q9", Role: "worker", State: stateApproved, Owner: "xavier"},
	} {
		h.indexNode(r)
	}
	kinds := map[string]bool{}
	files, _ := filepath.Glob("*.go")
	kindRE := regexp.MustCompile(`pb\.FleetEvent\{Kind: "([a-z]+)"`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range kindRE.FindAllStringSubmatch(string(b), -1) {
			kinds[m[1]] = true
		}
	}
	for _, k := range []string{"job", "node", "queue", "metrics", "links", "release"} {
		if !kinds[k] {
			t.Errorf("kind %q not found in the hub's source (the scan is broken)", k)
		}
	}
	kinds["some-new-kind"] = true
	f := h.eventFilter(h.viewer(caller{id: pki.Identity{Role: pki.RoleAdmin, Name: "root"}}))
	if f == nil {
		t.Fatal("no filter for the admin")
	}
	for k := range kinds {
		evs := []*pb.FleetEvent{
			{Kind: k, Node: "xavws-q9", Message: "xavws-q9 changed by admin/xavlap-q9"},
			{Kind: k, Node: "w1", Message: "labels changed by client/xavlap-q9 for xavws-q9",
				NodeInfo: &pb.NodeInfo{Name: "w1", Role: "worker", State: "approved"}},
			{Kind: k, Node: "xavws-q9", NodeInfo: &pb.NodeInfo{Name: "xavws-q9", Role: "worker", Owner: "xavier"}},
			{Kind: k, Node: "w1", Job: &pb.FleetJob{Worker: "xavws-q9", Job: &pb.Job{Spec: &pb.JobSpec{Id: "XAVJOBQ9", Owner: "xavier",
				Argv: []string{"xavarg-q9"}}}}},
		}
		for _, ev := range evs {
			for _, owner := range []string{"", "xavier", "root"} {
				out := f(ev, owner)
				if out == nil {
					continue
				}
				b, _ := protojson.Marshal(out)
				for _, s := range xavierSecrets {
					if strings.Contains(string(b), s) {
						t.Errorf("kind %s (owner %q): %s", k, owner, b)
					}
				}
			}
		}
	}
}
