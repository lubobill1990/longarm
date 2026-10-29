package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestJoinCodesPrivacy: an admin lists the codes it created, those of its
// own user and fleet worker codes; other users' codes - and client codes
// without owner or user that others created - are not listed, and used_by
// names only machines the admin may see.
func TestJoinCodesPrivacy(t *testing.T) {
	th, _ := ownedHub(t) // alice's worker code "aw" (used by aw)
	h := th.h
	now := time.Now()
	add := func(id string, c store.JoinCode) {
		c.ID, c.CreatedAt, c.ExpiresAt = id, now, now.Add(time.Hour)
		if err := h.store.AddJoinCode("hash-"+id, &c); err != nil {
			t.Fatal(err)
		}
	}
	add("c-mine", store.JoinCode{Role: "client", CreatedBy: "admin/root"})                                    // created by root
	add("c-other", store.JoinCode{Role: "client", CreatedBy: "admin/other"})                                  // nobody's, another admin's
	add("c-bob", store.JoinCode{Role: "client", User: "bob", CreatedBy: "admin/other", UsedBy: "bob-laptop"}) // bob's device
	add("c-root", store.JoinCode{Role: "client", User: "root", CreatedBy: "test"})                            // root's user
	add("w-fleet", store.JoinCode{Role: "worker", CreatedBy: "admin/other", UsedBy: "w1"})                    // fleet worker code
	add("w-mine", store.JoinCode{Role: "worker", Owner: "alice", CreatedBy: "admin/root", UsedBy: "aw"})      // root made it for alice

	r, err := pb.NewHubAdminClient(th.root).ListJoinCodes(tctx(t), &pb.ListJoinCodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*pb.JoinCode{}
	for _, c := range r.Codes {
		got[c.Id] = c
	}
	for _, id := range []string{"c-mine", "c-root", "w-fleet", "w-mine"} {
		if got[id] == nil {
			t.Errorf("admin misses code %s", id)
		}
	}
	for _, id := range []string{"c-other", "c-bob"} {
		if got[id] != nil {
			t.Errorf("admin lists %s: %v", id, got[id])
		}
	}
	for _, c := range r.Codes {
		if c.Owner == "alice" && c.Id != "w-mine" {
			t.Errorf("admin lists alice's own worker code %v", c)
		}
	}
	if c := got["w-mine"]; c != nil && c.UsedBy != "used" { // aw is alice's: hidden from root
		t.Errorf("used_by of alice's worker: %q", c.UsedBy)
	}
	if c := got["w-fleet"]; c != nil && c.UsedBy != "w1" { // the fleet worker is visible
		t.Errorf("used_by of the fleet worker: %q", c.UsedBy)
	}
}

// TestReleasePushErrorCounts: "no worker can self-update" counts every
// worker and names only those the caller may see.
func TestReleasePushErrorCounts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := InitCA(dir); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: dir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "off"}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	h, err := Open(cfg, slog.New(slog.NewTextHandler(&syncWriter{w: &bytes.Buffer{}}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	files := map[string]string{"longarmd-windows-amd64.exe": "d", "longarmd-windows-amd64.exe.minisig": "sig"}
	if _, err := addRelease(ctx, h.rel, h.store, releaseDir(t, files), "1.2.0", "", "", false); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.nodes["msi-fleet"] = &node{rec: &store.Node{Name: "msi-fleet", Role: "worker", State: stateApproved, Version: "1.1.0"}}
	h.nodes["msi-alice"] = &node{rec: &store.Node{Name: "msi-alice", Role: "worker", State: stateApproved, Version: "1.1.0", Owner: "alice"}}
	h.nodes["msi-root"] = &node{rec: &store.Node{Name: "msi-root", Role: "worker", State: stateApproved, Version: "1.1.0", Owner: "root"}}
	for _, n := range h.nodes {
		h.indexNode(n.rec)
	}
	h.mu.Unlock()
	root := tenancy.Caller{User: "root"}
	_, err = h.pushReleaseAs(root, &pb.PushReleaseRequest{Version: "1.2.0"}, "root")
	if status.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), "msi-alice") ||
		!strings.Contains(err.Error(), "3 worker(s)") || !strings.Contains(err.Error(), "msi-fleet") || !strings.Contains(err.Error(), "msi-root") {
		t.Fatalf("push error: %v", err)
	}
	r0, err := h.pushReleaseAs(root, &pb.PushReleaseRequest{Version: "1.2.0", AllowNone: true}, "root")
	r, err := h.rolloutFor(root, r0, err)
	if err != nil || r.NotCapableCount != 3 || len(r.NotCapable) != 2 || strings.Contains(strings.Join(r.NotCapable, ","), "msi-alice") {
		t.Fatalf("allow_none for root: %v %v", r, err)
	}
}

// TestPortLinksOwnOnly: with multi-tenancy an admin's --all lists its own
// user's links only, like any user; the panel too.
func TestPortLinksOwnOnly(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	h.mu.Lock()
	h.portLinks["l-alice"] = &portLinkEntry{l: &pb.PortLink{Id: "l-alice", Owner: "alice", From: "w1", To: "w1", JobId: "JA", CreatedBy: "client/alice"}}
	h.portLinks["l-root"] = &portLinkEntry{l: &pb.PortLink{Id: "l-root", Owner: "root", From: "w1", To: "w1"}}
	h.mu.Unlock()
	r, err := pb.NewFleetClient(th.root).ListPortLinks(tctx(t), &pb.ListPortLinksRequest{All: true})
	if err != nil || len(r.Links) != 1 || r.Links[0].Id != "l-root" {
		t.Fatalf("admin's links --all: %v %v", r, err)
	}
	if _, err := pb.NewFleetClient(th.bob).ListPortLinks(tctx(t), &pb.ListPortLinksRequest{All: true}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bob --all: %v", err)
	}
}

// TestAuditProjection: audit reads show the caller's user's records - its
// calls, calls on its workers, hub actions about it and its own hub actions
// - and only counts of the rest; users/remove keeps counts, not names.
func TestAuditProjection(t *testing.T) {
	th, _ := ownedHub(t) // alice's aw; fleet w1
	h := th.h
	now := time.Now()
	line := func(m map[string]any) string { b, _ := json.Marshal(m); return string(b) }
	// attributed as they would be when written (storeAuditLocked, auditHub)
	rows := []store.AuditRow{
		{Node: "w1", Time: now, Who: "client/bob-laptop", Method: "/longarm.v1.Jobs/Submit", ActorUser: "bob", Attributed: true,
			Line: line(map[string]any{"who": "client/bob-laptop", "user": "bob", "argv": []string{"secret-bob-cmd"}})},
		{Node: "aw", Time: now, Who: "client/alice", Method: "/longarm.v1.Sessions/Attach", ActorUser: "alice", SubjectUser: "alice", Attributed: true,
			Line: line(map[string]any{"who": "client/alice", "session": "alice-session"})},
		{Node: "w1", Time: now, Who: "admin/root", Method: "/longarm.v1.Exec/Run", ActorUser: "root", Attributed: true,
			Line: line(map[string]any{"who": "admin/root"})},
		{Node: "hub", Time: now, Who: "admin/other", Method: "users/set", ActorUser: "other", SubjectUser: "carol", Attributed: true,
			Line: line(map[string]any{"who": "admin/other", "user": "carol"})},
		{Node: "hub", Time: now, Who: "admin/root", Method: "workers/label", ActorUser: "root", Attributed: true,
			Line: line(map[string]any{"who": "admin/root", "worker": "w1"})},
		// from before attribution: only its own "user" field counts
		{Node: "w1", Time: now, Who: "client/root-old", Method: "/longarm.v1.Files/Read", Line: line(map[string]any{"who": "client/root-old"})},
	}
	if err := h.store.AddAudit(rows); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.nodes["bob-laptop"] = &node{rec: &store.Node{Name: "bob-laptop", Role: "client", State: stateApproved, User: "bob"}}
	h.mu.Unlock()
	if _, err := h.RemoveUser("admin/other", &pb.RemoveUserRequest{Name: "bob"}); err != nil {
		t.Fatal(err)
	}

	r, err := pb.NewFleetClient(th.root).Audit(tctx(t), &pb.AuditRequest{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var methods []string
	for _, rec := range r.Records {
		methods = append(methods, rec.Method)
		for _, leak := range []string{"secret-bob-cmd", "alice-session", "bob-laptop", "carol"} {
			if strings.Contains(rec.Json, leak) {
				t.Errorf("admin's audit shows %q: %s", leak, rec.Json)
			}
		}
	}
	if !slicesContains(methods, "/longarm.v1.Exec/Run") || !slicesContains(methods, "workers/label") {
		t.Errorf("admin misses its own records: %v", methods)
	}
	if r.HiddenTotal == 0 {
		t.Fatalf("no hidden count: %v", r)
	}
	counted := map[string]int32{}
	for _, c := range r.Hidden {
		counted[c.Method] = c.Count
	}
	if counted["/longarm.v1.Jobs/Submit"] != 1 || counted["/longarm.v1.Sessions/Attach"] != 1 || counted["users/set"] != 1 ||
		counted["users/remove"] != 1 || counted["/longarm.v1.Files/Read"] != 1 {
		t.Errorf("hidden counts %v", counted)
	}

	// alice: her own calls and those on her worker
	ar := h.mustAudit(t, tenancy.Caller{User: "alice"}, "client/alice")
	if !strings.Contains(ar, "alice-session") || strings.Contains(ar, "secret-bob-cmd") {
		t.Errorf("alice's audit: %s", ar)
	}

	// users/remove is stored with a count of the revoked machines
	all, _ := h.Audit(&pb.AuditRequest{Node: "hub", Limit: 500})
	for _, rec := range all.Records {
		if rec.Method == "users/remove" && (strings.Contains(rec.Json, "bob-laptop") || !strings.Contains(rec.Json, `"revoked":1`)) {
			t.Errorf("users/remove record: %s", rec.Json)
		}
	}
}

func (h *Hub) mustAudit(t *testing.T, v tenancy.Caller, by string) string {
	t.Helper()
	r, err := h.AuditFor(v, by, &pb.AuditRequest{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, rec := range r.Records {
		b.WriteString(rec.Json + "\n")
	}
	return b.String()
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestAuditAttributionIsHistorical: an admin who revokes another user's
// device and enrolls the name again for itself does not get the old
// records: reads go by the user recorded when each was written.
func TestAuditAttributionIsHistorical(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	h.mu.Lock()
	n := &node{rec: &store.Node{Name: "bob-pc", Role: "client", State: stateApproved, User: "bob"}}
	h.nodes["bob-pc"] = n
	h.indexNode(n.rec)
	w1 := h.nodes["w1"]
	h.storeAuditLocked(w1, nil, &pb.AuditBatch{Lines: []string{
		`{"time":"2026-10-29T10:00:00Z","who":"client/bob-pc","method":"/longarm.v1.Jobs/Submit","argv":["bobs-secret-arg"]}`,
	}})
	h.mu.Unlock()
	// the name goes to root
	h.mu.Lock()
	n.rec.User, n.rec.Role = "root", "admin"
	h.indexNode(n.rec)
	h.mu.Unlock()
	r, err := pb.NewFleetClient(th.root).Audit(tctx(t), &pb.AuditRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range r.Records {
		if strings.Contains(rec.Json, "bobs-secret-arg") {
			t.Fatalf("re-enrolled name opened bob's record: %s", rec.Json)
		}
	}
	if r.HiddenTotal == 0 {
		t.Fatal("bob's record not counted")
	}
	// bob still reads it
	if !strings.Contains(h.mustAudit(t, tenancy.Caller{User: "bob"}, ""), "bobs-secret-arg") {
		t.Fatal("bob lost his own record")
	}
}
