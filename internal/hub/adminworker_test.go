package hub

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestLinkAdminWorker: a pairing that carries an admin worker's pre-made
// CSR (its key held elsewhere: longarmd as LocalSystem) enrolls it with the
// same approval; the page says it runs as SYSTEM; the hub marks it
// (store.Node.Admin, label longarm.io/admin=true) and keeps the rules:
// owner-only (admin certificates, --owner, shares refused), never placed,
// no hub-relayed commands, the reserved label nobody sets or removes.
func TestLinkAdminWorker(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	h.loginLimit, h.joinLimit = newIPLimiter(100, 100), newIPLimiter(100, 100)
	alice := newWeb(t, h)
	alice.passwordSignIn(h, "alice-web", "user", "alice")

	akeyPEM, acsrPEM, err := pki.NewKeyAndCSR("", nil) // the admin longarmd's key: never Link's
	if err != nil {
		t.Fatal(err)
	}
	codeCh := make(chan string, 1)
	done := make(chan error, 1)
	var res *hubclient.JoinResult
	go func() {
		var err error
		res, err = hubclient.Link(context.Background(), h.Addr("grpc"), hubclient.LinkOptions{Kind: linkBoth,
			JoinOptions: hubclient.JoinOptions{Name: "devbox", Fingerprint: pki.Fingerprint(th.ca.Cert)},
			Worker:      hubclient.JoinOptions{Name: "devbox-w"},
			AdminWorker: &hubclient.JoinOptions{CSR: acsrPEM},
			Show:        func(st *pb.StartLinkResponse) { codeCh <- st.UserCode }})
		done <- err
	}()
	var code string
	select {
	case code = <-codeCh:
	case err := <-done:
		t.Fatal(err)
	}
	_, confirm := alice.lookup(code)
	for _, want := range []string{"This includes an ADMINISTRATOR worker", "runs commands as <b>SYSTEM</b>",
		"admin worker (SYSTEM)", "(default: &lt;worker name&gt;-admin)", "it can't be shared",
		"run <b>administrator</b> commands on it remotely"} {
		if !strings.Contains(confirm, want) {
			t.Errorf("confirmation without %q", want)
		}
	}
	if t.Failed() {
		t.Fatalf("confirmation: %s", confirm)
	}
	if c, body := alice.decide(confirm, "approve"); c != 200 || !strings.Contains(body, "Admin worker devbox-w-admin is yours alone") ||
		!strings.Contains(body, "as SYSTEM") {
		t.Fatalf("approve: %d %s", c, body)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Link did not finish")
	}
	a := res.AdminWorker
	if a == nil || a.KeyPEM != nil || a.Resp.Name != "devbox-w-admin" || a.Resp.Owner != "alice" ||
		a.Resp.Labels[sched.AdminLabel] != "true" || res.Worker == nil || res.Worker.Resp.Labels[sched.AdminLabel] != "" {
		t.Fatalf("admin worker: %+v (worker %+v)", a, res.Worker)
	}
	if _, err := tls.X509KeyPair(a.Resp.CertPem, akeyPEM); err != nil {
		t.Fatalf("the admin certificate is not for the pre-made CSR's key: %v", err)
	}
	if id, _ := identityOf(t, a.Resp); id.Role != pki.RoleWorker || id.Name != "devbox-w-admin" {
		t.Fatalf("admin identity: %v", id)
	}
	const aw = "devbox-w-admin"
	h.mu.Lock()
	n := h.nodes[aw]
	info := h.nodeInfoLocked(n)
	plain := h.nodeInfoLocked(h.nodes["devbox-w"])
	h.mu.Unlock()
	if !n.rec.Admin || !info.AdminWorker || info.Labels[sched.AdminLabel] != "true" || plain.AdminWorker {
		t.Fatalf("hub record: %+v %+v", n.rec, info)
	}

	// The reserved label: nobody sets or removes it - owner, admin, join
	// codes, a worker's own hello.
	for who, f := range map[string]func() error{
		"owner removes": func() error {
			_, err := h.SetWorkerLabels("client/alice", "alice", false, &pb.SetLabelsRequest{Name: aw, Labels: map[string]string{sched.AdminLabel: ""}})
			return err
		},
		"admin removes": func() error {
			_, err := h.SetLabels("admin/root", aw, map[string]string{sched.AdminLabel: ""})
			return err
		},
		"owner sets on another": func() error {
			_, err := h.SetWorkerLabels("client/alice", "alice", false, &pb.SetLabelsRequest{Name: "devbox-w", Labels: map[string]string{sched.AdminLabel: "true"}})
			return err
		},
		"join code": func() error {
			_, err := h.CreateJoinCode("admin/root", &pb.CreateJoinCodeRequest{Role: "worker", Labels: map[string]string{"longarm.io/x": "1"}})
			return err
		},
	} {
		if err := f(); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("%s: %v", who, err)
		}
	}
	h.mu.Lock()
	h.applyHelloLocked(h.nodes["devbox-w"], &pb.NodeHello{Labels: map[string]string{sched.AdminLabel: "true", "pool": "a"}})
	got := labelsOf(h.nodes["devbox-w"])
	h.mu.Unlock()
	if got[sched.AdminLabel] != "" || got["pool"] != "a" {
		t.Errorf("a worker marked itself admin: %v", got)
	}
	if _, err := h.SetLabels("admin/root", aw, map[string]string{"pool": "x"}); err != nil {
		t.Errorf("other labels of an admin worker: %v", err)
	}

	// Never shared: not by its owner (even with can_share), not by an admin.
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "alice", SetCanShare: true, CanShare: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ShareWorker("client/alice", "alice", false, &pb.ShareWorkerRequest{Worker: aw, AddUsers: []string{"bob"}}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("owner shares the admin worker: %v", err)
	}
	if _, err := pb.NewFleetClient(th.root).ShareWorker(tctx(t), &pb.ShareWorkerRequest{Worker: aw, SetEveryone: true, Everyone: true}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("an admin certificate shares the admin worker: %v", err)
	}
	// shares that got into the record anyway are ignored
	h.mu.Lock()
	n.rec.Shares = &store.WorkerShares{Everyone: true}
	bobMay, aliceMay, rootMay := h.mayUseLocked("bob", n), h.mayUseLocked("alice", n), h.mayUseLocked("root", n)
	wa := workerAccessLocked(n)
	n.rec.Shares = nil
	h.mu.Unlock()
	if bobMay || rootMay || !aliceMay || wa.Shares != nil || wa.Owner != "alice" {
		t.Errorf("owner-only: bob %v root %v alice %v access %v", bobMay, rootMay, aliceMay, wa)
	}
	r, err := pb.NewFleetClient(th.bob).ListNodes(tctx(t), &pb.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range r.Nodes {
		if n.Name == aw {
			t.Errorf("bob sees the admin worker")
		}
	}

	// Never placed automatically (the hub's scheduler).
	h.mu.Lock()
	pick := h.pickLocked(&pb.PlaceRequest{}, 1, "alice")
	h.mu.Unlock()
	if pick.Worker == aw || !strings.Contains(pick.Reasons[aw], "admin worker") {
		t.Errorf("placement: %+v", pick)
	}

	// No hub-relayed commands: not its owner's, not an admin's as the owner (--owner).
	for _, c := range []struct {
		by, owner string
		admin     bool
	}{{"client/alice", "alice", false}, {"admin/root", "root", true}, {"admin/root", "alice", true}} {
		_, err := h.SubmitJobAs(tctx(t), c.by, c.owner, c.admin, &pb.HubSubmitRequest{Worker: aw, Argv: []string{"whoami"}})
		if status.Code(err) != codes.PermissionDenied && status.Code(err) != codes.NotFound {
			t.Errorf("hub submit by %s as %s: %v", c.by, c.owner, err)
		}
	}
	if err := h.relayCheck("admin/root", "alice", aw, "kill jobs", "J1"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("relay as the owner: %v", err)
	}
}

// TestLinkAdminKind: kind "admin" - an admin worker alone (the tray's
// "enable admin worker" after the first sign-in); requests that don't fit.
func TestLinkAdminKind(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	h.loginLimit, h.joinLimit = newIPLimiter(100, 100), newIPLimiter(100, 100)
	ec := enrollClient(t, h)
	_, csrPEM, _ := pki.NewKeyAndCSR("", nil)
	for what, req := range map[string]*pb.StartLinkRequest{
		"admin with labels":           {Kind: linkAdmin, CsrPem: csrPEM, WorkerLabels: map[string]string{"a": "b"}},
		"admin with an admin CSR":     {Kind: linkAdmin, CsrPem: csrPEM, AdminWorkerCsrPem: csrPEM},
		"admin name without CSR":      {Kind: linkWorker, CsrPem: csrPEM, AdminWorkerName: "x-admin"},
		"admin CSR not a CSR":         {Kind: linkWorker, CsrPem: csrPEM, AdminWorkerCsrPem: []byte("nope")},
		"admin named like the worker": {Kind: linkWorker, CsrPem: csrPEM, Name: "same", AdminWorkerCsrPem: csrPEM, AdminWorkerName: "same"},
		"reserved sign-in label":      {Kind: linkWorker, CsrPem: csrPEM, WorkerLabels: map[string]string{sched.AdminLabel: "true"}},
	} {
		req.Hostname, req.Os, req.Arch, req.Version = "h", "windows", "amd64", "1"
		if _, err := ec.StartLink(tctx(t), req); status.Code(err) != codes.InvalidArgument && status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: %v", what, err)
		}
	}
	keyPEM, csrPEM, _ := pki.NewKeyAndCSR("", nil)
	st, err := ec.StartLink(tctx(t), &pb.StartLinkRequest{Kind: linkAdmin, CsrPem: csrPEM, Hostname: "DevBox-7.corp.example",
		Os: "windows", Arch: "amd64", Version: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	alice := newWeb(t, h)
	alice.passwordSignIn(h, "alice-web", "user", "alice")
	_, confirm := alice.lookup(st.UserCode)
	for _, want := range []string{"<b>admin worker</b> (runs commands as SYSTEM)", "This includes an ADMINISTRATOR worker",
		"(default: &lt;host name&gt;-admin)"} {
		if !strings.Contains(confirm, want) {
			t.Errorf("confirmation without %q: %s", want, confirm)
		}
	}
	if strings.Contains(confirm, "acts as you") {
		t.Errorf("an admin worker pairing reads like a device's: %s", confirm)
	}
	if c, body := alice.decide(confirm, "approve"); c != http.StatusOK || !strings.Contains(body, "Admin worker devbox-7-admin is yours alone") {
		t.Fatalf("approve: %d %s", c, body)
	}
	r := poll(t, h, ec, &device{st: st, keyPEM: keyPEM})
	if r.State != linkApproved || r.Name != "devbox-7-admin" || r.Owner != "alice" || r.Labels[sched.AdminLabel] != "true" || r.AdminWorker != nil {
		t.Fatalf("admin kind: %v", r)
	}
	if _, err := tls.X509KeyPair(r.CertPem, keyPEM); err != nil {
		t.Fatal(err)
	}
}

// TestAdminWorkerNoHubActions: the hub starts nothing on an admin worker by
// itself - no toolchain profile (package operations as SYSTEM) can be
// assigned to it, a profile's rollout skips it, and a release rollout
// neither picks it nor offers it a version.
func TestAdminWorkerNoHubActions(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	if _, err := h.PutProfile("admin", &pb.Profile{Name: "base", Tools: []*pb.ToolSpec{{Name: "git", Version: "2.45.0"}}}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.nodes["box-admin"] = &node{rec: &store.Node{Name: "box-admin", Role: "worker", State: stateApproved, Owner: "alice", Admin: true, UpdateEnabled: true}}
	h.nodes["box"] = &node{rec: &store.Node{Name: "box", Role: "worker", State: stateApproved, Owner: "alice", UpdateEnabled: true}}
	h.mu.Unlock()
	if _, err := h.AssignProfile("admin", "box-admin", "base"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("profile on the admin worker: %v", err)
	}
	if _, err := h.AssignProfile("admin", "box-admin", ""); err != nil {
		t.Fatalf("clearing a profile: %v", err)
	}
	if _, err := h.AssignProfile("admin", "box", "base"); err != nil {
		t.Fatalf("profile on a plain worker: %v", err)
	}
	h.mu.Lock()
	// even a profile already on its record (an older hub) yields nothing
	h.nodes["box-admin"].rec.Profile = "base"
	if d := h.desiredToolchainLocked(h.nodes["box-admin"]); d.Profile != "" || len(d.Tools) != 0 {
		t.Errorf("desired toolchain of the admin worker: %v", d)
	}
	h.rollout = &pb.Rollout{Version: "9.9.9", Channel: "stable", State: rolloutDone}
	if v := h.targetLocked(h.nodes["box-admin"]); v != "" {
		t.Errorf("rollout target of the admin worker: %q", v)
	}
	if v := h.targetLocked(h.nodes["box"]); v != "9.9.9" {
		t.Errorf("rollout target of a plain worker: %q", v)
	}
	h.rollout = nil
	h.mu.Unlock()
}
