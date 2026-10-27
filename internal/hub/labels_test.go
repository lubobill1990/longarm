package hub

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestSetWorkerLabels: an owner labels its own worker (Fleet.SetWorkerLabels),
// an admin any worker; others' and fleet workers are refused; labels are
// validated as for longarm admin label, and every change is audited.
func TestSetWorkerLabels(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	conns := map[string]*grpc.ClientConn{"alice": th.alice, "bob": th.bob, "root": th.root}
	set := func(who string, req *pb.SetLabelsRequest) (*pb.NodeInfo, error) {
		return pb.NewFleetClient(conns[who]).SetWorkerLabels(tctx(t), req)
	}
	n, err := set("alice", &pb.SetLabelsRequest{Name: "aw", Labels: map[string]string{"pool": "devbox", "gpu": "a100"}})
	if err != nil || n.Labels["pool"] != "devbox" || n.Labels["gpu"] != "a100" {
		t.Fatalf("alice labels aw: %v %v", n, err)
	}
	if n, err = set("alice", &pb.SetLabelsRequest{Name: "aw", Labels: map[string]string{"gpu": ""}}); err != nil || n.Labels["gpu"] != "" ||
		n.Labels["pool"] != "devbox" {
		t.Fatalf("alice deletes gpu: %v %v", n, err)
	}
	if _, err := set("bob", &pb.SetLabelsRequest{Name: "aw", Labels: map[string]string{"pool": "x"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("bob labels alice's worker: %v", err)
	}
	if _, err := set("alice", &pb.SetLabelsRequest{Name: "w1", Labels: map[string]string{"pool": "x"}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("alice labels the fleet worker: %v", err)
	}
	for _, bad := range []map[string]string{{"bad key": "x"}, {"k": "a,b"}, nil} {
		if _, err := set("alice", &pb.SetLabelsRequest{Name: "aw", Labels: bad}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("labels %v: %v", bad, err)
		}
	}
	if n, err := set("root", &pb.SetLabelsRequest{Name: "w1", Labels: map[string]string{"zone": "a"}}); err != nil || n.Labels["zone"] != "a" {
		t.Fatalf("admin labels w1: %v %v", n, err)
	}
	if n, err := set("root", &pb.SetLabelsRequest{Name: "aw", Labels: map[string]string{"zone": "b"}}); err != nil || n.Labels["zone"] != "b" {
		t.Fatalf("admin labels aw: %v %v", n, err)
	}
	h.mu.Lock()
	got := maps.Clone(h.nodes["aw"].rec.Labels)
	h.mu.Unlock()
	if !maps.Equal(got, map[string]string{"pool": "devbox", "zone": "b"}) {
		t.Fatalf("stored labels %v", got)
	}
	rows, _ := h.store.Audit("hub", time.Time{}, 100)
	n2 := 0
	for _, r := range rows {
		if r.Method == "workers/label" {
			n2++
			if !strings.Contains(r.Line, `"worker":`) {
				t.Errorf("audit line %s", r.Line)
			}
		}
	}
	if n2 < 4 {
		t.Fatalf("%d workers/label audit records", n2)
	}
}

// TestSignInWorkerLabels: labels a worker's sign-in asks for (longarmd
// login --label) are recorded at enrollment - az (EntraJoin) and the
// pairing, where the approver sees them - and reported back; they must be
// valid and go with workers only.
func TestSignInWorkerLabels(t *testing.T) {
	f := newFakeEntra(t)
	h := newEntraHub(t, f, nil)
	h.loginLimit, h.joinLimit = newIPLimiter(100, 100), newIPLimiter(100, 100)
	fp := pki.Fingerprint(h.ca.Cert)
	alice := f.sign(f.accessClaims("a11ce000-0000-4000-8000-000000000401", "alice@example.com"))
	login := func(kind, name string, labels map[string]string) (*hubclient.JoinResult, error) {
		return hubclient.Login(tctx(t), h.Addr("grpc"), hubclient.LoginOptions{Kind: kind,
			JoinOptions: hubclient.JoinOptions{Name: name, Fingerprint: fp, Labels: labels},
			Token:       func(context.Context, *pb.AuthInfoResponse) (string, error) { return alice, nil }})
	}
	want := map[string]string{"pool": "devbox"}
	r, err := login(linkWorker, "lw1", want)
	if err != nil || !maps.Equal(r.Resp.Labels, want) {
		t.Fatalf("az worker sign-in with labels: %v %v", r, err)
	}
	h.mu.Lock()
	got := maps.Clone(h.nodes["lw1"].rec.Labels)
	h.mu.Unlock()
	if !maps.Equal(got, want) {
		t.Fatalf("recorded %v", got)
	}
	if _, err := login(linkWorker, "lw2", map[string]string{"pool": "a b"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid label: %v", err)
	}
	if _, err := login("", "ld", want); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("labels on a client device: %v", err)
	}

	// the pairing
	ec := enrollClient(t, h)
	keyPEM, csrPEM, _ := pki.NewKeyAndCSR("lw3", nil)
	st, err := ec.StartLink(tctx(t), &pb.StartLinkRequest{Kind: linkWorker, CsrPem: csrPEM, Name: "lw3", Hostname: "lw3.corp.example",
		Os: "linux", Arch: "amd64", Version: "1.2.3", WorkerLabels: map[string]string{"pool": "devbox", "os.flavor": "ubuntu"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ec.StartLink(tctx(t), &pb.StartLinkRequest{Kind: linkClient, CsrPem: csrPEM, Hostname: "c", Os: "linux", Arch: "amd64",
		Version: "1", WorkerLabels: want}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("labels on a client pairing: %v", err)
	}
	web := newWeb(t, h)
	web.entraSignIn(f, "a11ce000-0000-4000-8000-000000000401", "alice@example.com")
	code, confirm := web.lookup(st.UserCode)
	if code != 200 || !strings.Contains(confirm, "worker labels") || !strings.Contains(confirm, "os.flavor=ubuntu, pool=devbox") {
		t.Fatalf("confirmation: %d %s", code, confirm)
	}
	if c, body := web.decide(confirm, "approve"); c != 200 {
		t.Fatalf("approve: %d %s", c, body)
	}
	resp := poll(t, h, ec, &device{st: st, keyPEM: keyPEM})
	if resp.State != linkApproved || resp.Labels["pool"] != "devbox" || resp.Labels["os.flavor"] != "ubuntu" {
		t.Fatalf("pairing result: %v", resp)
	}
	rows, _ := h.store.Audit("hub", time.Time{}, 100)
	var methods []string
	for _, r := range rows {
		if strings.Contains(r.Line, "devbox") {
			methods = append(methods, r.Method)
		}
	}
	if !slices.Contains(methods, "workers/join") || !slices.Contains(methods, "link/start") {
		t.Fatalf("labels not audited: %v", methods)
	}
}
