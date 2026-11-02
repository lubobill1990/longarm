package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/update"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The hub's view of a worker's update (NodeInfo.update; docs/usage.md
// "Updating") end to end: a fake updater - a worker node of this test
// speaking HubNode.Connect with a real certificate - goes through the
// phases, goes away, comes back on the new version or on the old one with
// a reason; the real hub keeps, times out, persists and shows the state
// (longarm ls, admin nodes --json, the panel's API).

// fakeWorker is a worker session of the test: hello, metrics (so the hub
// keeps it online), update reports.
type fakeWorker struct {
	t      *testing.T
	addr   string
	b      *pki.Bundle
	name   string
	mu     sync.Mutex
	conn   *hubclient.Conn
	stream pb.HubNode_ConnectClient
	cancel context.CancelFunc
	done   chan struct{}
	hints  chan string // UpdateHint versions from the hub
}

func newFakeWorker(t *testing.T, h *hubEnv, name string, r *hubclient.JoinResult) *fakeWorker {
	t.Helper()
	dir := filepath.Join(h.root, "fake-"+name)
	if err := pki.WriteBundle(dir, r.Resp.CaPem, r.Resp.CertPem, r.KeyPEM); err != nil {
		t.Fatal(err)
	}
	b, err := pki.LoadBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeWorker{t: t, addr: h.addr(), b: b, name: name, hints: make(chan string, 8)}
	t.Cleanup(f.disconnect)
	return f
}

// ownedFakeWorker enrolls a worker of alice (a worker code: approved at once).
func ownedFakeWorker(t *testing.T, h *hubEnv, name string) *fakeWorker {
	t.Helper()
	code := strings.TrimSpace(h.mustLongarm(h.client, "worker", "add", name))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := hubclient.Join(ctx, h.addr(), code, hubclient.JoinOptions{Name: name, Fingerprint: h.fp})
	if err != nil || r.Resp.State != "approved" {
		t.Fatalf("join %s: %v %v", name, err, r)
	}
	return newFakeWorker(t, h, name, r)
}

// connect opens a session: hello with version and the update status it
// carries (nil: none); updateEnabled: it can update itself (rollouts).
func (f *fakeWorker) connect(version string, st *pb.UpdateStatus, updateEnabled bool) {
	f.t.Helper()
	f.disconnect()
	conn, err := hubclient.Dial(f.addr, f.b)
	if err != nil {
		f.t.Fatal(err)
	}
	host, _ := os.Hostname() // the pairing's (a session worker and its admin worker: one host)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := conn.Node().Connect(ctx)
	if err == nil {
		err = stream.Send(&pb.NodeReport{Msg: &pb.NodeReport_Hello{Hello: &pb.NodeHello{Version: version, Os: runtime.GOOS, Arch: runtime.GOARCH,
			Hostname: host, Channel: "stable", UpdateEnabled: updateEnabled, Update: st, Capabilities: []string{"node-id"}}}})
	}
	var first *pb.HubCommand
	if err == nil {
		first, err = stream.Recv()
	}
	if err != nil || first.GetWelcome() == nil {
		cancel()
		conn.Close()
		f.t.Fatalf("%s: connect: %v %v", f.name, err, first)
	}
	done := make(chan struct{})
	f.mu.Lock()
	f.conn, f.stream, f.cancel, f.done = conn, stream, cancel, done
	f.mu.Unlock()
	go func() { // the hub's commands: update hints noted, the rest dropped
		for {
			c, err := stream.Recv()
			if err != nil {
				return
			}
			if u := c.GetUpdate(); u != nil {
				select {
				case f.hints <- u.Version:
				default:
				}
			}
		}
	}()
	go func() { // metrics: a live session
		defer close(done)
		for {
			f.send(&pb.NodeReport{Msg: &pb.NodeReport_Metrics{Metrics: &pb.MetricsSnapshot{Time: timestamppb.Now(), UptimeSeconds: 3600}}})
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}()
}

func (f *fakeWorker) send(m *pb.NodeReport) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stream != nil {
		f.stream.Send(m)
	}
}

// report sends an update report (the updater's phases, its result).
func (f *fakeWorker) report(state, phase, from, to string, mod func(*pb.UpdateStatus)) {
	s := &pb.UpdateStatus{State: state, Phase: phase, From: from, To: to, Method: "msi", Initiator: "owner", Attempt: "e2e-1",
		Started: timestamppb.New(time.Now().Add(-time.Minute)), At: timestamppb.Now()}
	if mod != nil {
		mod(s)
	}
	f.send(&pb.NodeReport{Msg: &pb.NodeReport_Update{Update: s}})
}

// disconnect ends the session (the worker restarting).
func (f *fakeWorker) disconnect() {
	f.mu.Lock()
	cancel, conn, done := f.cancel, f.conn, f.done
	f.cancel, f.conn, f.stream = nil, nil, nil
	f.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
		conn.Close()
	}
}

// update is the worker's NodeInfo.update as the hub lists it (nil: none).
func nodeUpd(n map[string]any) map[string]any {
	u, _ := n["update"].(map[string]any)
	return u
}

// lsState is the STATE column of name in alice's longarm ls.
func lsState(t *testing.T, h *hubEnv, name string) string {
	t.Helper()
	out := h.mustLongarm(h.client, "ls", name)
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, name+" ") {
			return l
		}
	}
	t.Fatalf("ls %s:\n%s", name, out)
	return ""
}

func updateHub(t *testing.T, extra string) *hubEnv {
	h := newHubEnv(t, "[updates]\nrestart_timeout = \"8s\"\nrequested_timeout = \"8s\"\n"+extra)
	h.mustLongarm(h.admin, "admin", "user", "add", "alice", "--no-code")
	return h
}

// Success with the phases and the restart; a second update while it runs
// is refused (edge case 5); the RTT gap and "offline" are "updating"
// meanwhile (11).
func TestUpdateStateE2ESuccess(t *testing.T) {
	t.Parallel()
	h := updateHub(t, "")
	f := ownedFakeWorker(t, h, "uw")
	f.connect("1.0.0", nil, false)
	h.waitNode(t, "uw", "online", func(n map[string]any) bool { return isTrue(n["online"]) && nodeUpd(n) == nil })
	f.report("applying", "checking", "1.0.0", "2.0.0", nil)
	f.report("applying", "downloading", "1.0.0", "2.0.0", func(s *pb.UpdateStatus) { s.BytesDone, s.BytesTotal = 12<<20, 42<<20 })
	h.waitNode(t, "uw", "downloading", func(n map[string]any) bool {
		u := nodeUpd(n)
		return u["state"] == "updating" && u["phase"] == "downloading" && u["bytes_done"] != nil
	})
	if s := lsState(t, h, "uw"); !strings.Contains(s, "updating 1.0.0→2.0.0 (downloading 12/42 MB)") {
		t.Fatalf("ls: %s", s)
	}
	// a second update: refused, with what runs
	r := h.longarm(h.client, "worker", "update", "uw", "--version", "2.0.0")
	if r.code != 75 || !strings.Contains(r.stderr, "uw is already updating to 2.0.0 (downloading 12/42 MB)") || !strings.Contains(r.stderr, "by its owner") {
		t.Fatalf("second update: %+v", r)
	}
	f.report("applying", "verifying", "1.0.0", "2.0.0", nil)
	f.report("restarting", "installing", "1.0.0", "2.0.0", nil)
	h.waitNode(t, "uw", "installing", func(n map[string]any) bool { return nodeUpd(n)["phase"] == "installing" })
	f.disconnect()
	n := h.waitNode(t, "uw", "away, updating", func(n map[string]any) bool {
		u := nodeUpd(n)
		return !isTrue(n["online"]) && u["state"] == "updating" && u["offline_since"] != nil
	})
	if u := nodeUpd(n); u["phase"] != "installing" || u["initiator"] != "owner" || u["method"] != "msi" || !isTrue(u["jobs_keep_running"]) {
		t.Fatalf("away: %v", u)
	}
	if s := lsState(t, h, "uw"); !strings.Contains(s, "updating 1.0.0→2.0.0 (restarting, last installing)") || strings.Contains(s, "offline") {
		t.Fatalf("ls while away: %s", s)
	}
	f.connect("2.0.0", &pb.UpdateStatus{State: "ok", From: "1.0.0", To: "2.0.0", Attempt: "e2e-1", At: timestamppb.Now()}, false)
	h.waitNode(t, "uw", "online on 2.0.0", func(n map[string]any) bool {
		return isTrue(n["online"]) && n["version"] == "2.0.0" && nodeUpd(n) == nil
	})
	if s := lsState(t, h, "uw"); strings.Contains(s, "updat") {
		t.Fatalf("ls after: %s", s)
	}
}

// Edge case 1: the update fails; the worker is back on the old version
// with the reason: "failed", never stuck at "updating".
func TestUpdateStateE2EFailure(t *testing.T) {
	t.Parallel()
	h := updateHub(t, "")
	f := ownedFakeWorker(t, h, "fw")
	f.connect("2.0.0", nil, false)
	f.report("applying", "verifying", "2.0.0", "3.0.0", nil)
	h.waitNode(t, "fw", "verifying", func(n map[string]any) bool { return nodeUpd(n)["phase"] == "verifying" })
	f.disconnect()
	f.connect("2.0.0", &pb.UpdateStatus{State: "error", From: "2.0.0", To: "3.0.0", Attempt: "e2e-1", Method: "msi",
		Error: "refusing longarm-windows-amd64.msi 3.0.0: invalid signature", At: timestamppb.Now()}, false)
	n := h.waitNode(t, "fw", "failed", func(n map[string]any) bool { return isTrue(n["online"]) && nodeUpd(n)["state"] == "failed" })
	if u := nodeUpd(n); u["code"] != "signature" || !strings.Contains(fmt.Sprint(u["error"]), "invalid signature") {
		t.Fatalf("failed: %v", u)
	}
	if s := lsState(t, h, "fw"); !strings.Contains(s, "online, update to 3.0.0 failed (signature)") {
		t.Fatalf("ls: %s", s)
	}
	// a new attempt replaces the failure
	f.report("applying", "downloading", "2.0.0", "3.0.1", func(s *pb.UpdateStatus) { s.Attempt = "e2e-2" })
	h.waitNode(t, "fw", "retrying", func(n map[string]any) bool { return nodeUpd(n)["state"] == "updating" && nodeUpd(n)["to"] == "3.0.1" })
}

// Edge case 2: the worker doesn't come back by the deadline: timed_out
// ("didn't finish", last phase); back on the old version later: failed.
func TestUpdateStateE2EDeadline(t *testing.T) {
	t.Parallel()
	h := updateHub(t, "")
	f := ownedFakeWorker(t, h, "dw")
	f.connect("1.0.0", nil, false)
	f.report("restarting", "installing", "1.0.0", "2.0.0", nil)
	h.waitNode(t, "dw", "installing", func(n map[string]any) bool { return nodeUpd(n)["phase"] == "installing" })
	f.disconnect()
	n := h.waitNode(t, "dw", "timed out", func(n map[string]any) bool { return nodeUpd(n)["state"] == "timed_out" })
	if u := nodeUpd(n); u["phase"] != "installing" || u["code"] != "timeout" {
		t.Fatalf("timed out: %v", u)
	}
	if s := lsState(t, h, "dw"); !strings.Contains(s, "offline: update to 2.0.0 didn't finish (last phase installing") {
		t.Fatalf("ls: %s", s)
	}
	f.connect("1.0.0", nil, false)
	h.waitNode(t, "dw", "back, failed", func(n map[string]any) bool {
		return isTrue(n["online"]) && nodeUpd(n)["state"] == "failed" && nodeUpd(n)["code"] == "timeout"
	})
}

// Edge case 8: the hub restarts while the worker updates: the state is
// still there, then resolved by the worker's hello.
func TestUpdateStateE2EHubRestart(t *testing.T) {
	t.Parallel()
	h := updateHub(t, "")
	f := ownedFakeWorker(t, h, "rw")
	f.connect("1.0.0", nil, false)
	f.report("restarting", "installing", "1.0.0", "2.0.0", nil)
	h.waitNode(t, "rw", "installing", func(n map[string]any) bool { return nodeUpd(n)["phase"] == "installing" })
	f.disconnect()
	h.killHub()
	h.startHub(t)
	n := h.waitNode(t, "rw", "updating after the restart", func(n map[string]any) bool { return nodeUpd(n)["state"] == "updating" })
	if u := nodeUpd(n); u["phase"] != "installing" || u["to"] != "2.0.0" {
		t.Fatalf("after the hub restart: %v", u)
	}
	f.connect("2.0.0", nil, false)
	h.waitNode(t, "rw", "online on 2.0.0", func(n map[string]any) bool {
		return isTrue(n["online"]) && n["version"] == "2.0.0" && nodeUpd(n) == nil
	})
}

// Edge case 9: an older worker (no phases, no attempt IDs) updated by a
// rollout: the hub's generic "requested", then "updating" from its legacy
// "applying"; online on the new version at the end.
func TestUpdateStateE2EOldWorker(t *testing.T) {
	t.Parallel()
	h := updateHub(t, "")
	f := ownedFakeWorker(t, h, "ow")
	f.connect("1.0.0", nil, true)
	// a signed release the rollout can push (the fake never runs it)
	_, priv, err := update.GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(h.root, "release.key")
	os.WriteFile(keyFile, priv, 0o600)
	rel := filepath.Join(h.root, "rel9")
	os.MkdirAll(rel, 0o755)
	os.WriteFile(filepath.Join(rel, update.ReleaseName("longarmd", runtime.GOOS, runtime.GOARCH)), []byte("fake longarmd 9.0.0"), 0o755)
	h.hubCmd(t, "release", "sign", rel, "--version", "9.0.0", "--key", keyFile)
	h.hubCmd(t, "release", "add", rel, "--version", "9.0.0")
	h.mustLongarm(h.admin, "admin", "release", "push", "9.0.0", "--canary", "1", "--wait", "1h")
	select {
	case v := <-f.hints:
		if v != "9.0.0" {
			t.Fatalf("hint %s", v)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no update hint")
	}
	n := h.waitNode(t, "ow", "requested", func(n map[string]any) bool { return nodeUpd(n)["state"] == "requested" })
	if u := nodeUpd(n); u["initiator"] != "rollout" || isTrue(u["reported"]) || u["started_at"] == nil {
		t.Fatalf("requested: %v", u)
	}
	// the legacy self-updater: state only
	f.send(&pb.NodeReport{Msg: &pb.NodeReport_Update{Update: &pb.UpdateStatus{State: "applying", From: "1.0.0", To: "9.0.0"}}})
	h.waitNode(t, "ow", "updating", func(n map[string]any) bool { return nodeUpd(n)["state"] == "updating" })
	if s := lsState(t, h, "ow"); !strings.Contains(s, "updating 1.0.0→9.0.0") {
		t.Fatalf("ls: %s", s)
	}
	f.send(&pb.NodeReport{Msg: &pb.NodeReport_Update{Update: &pb.UpdateStatus{State: "restarting", From: "1.0.0", To: "9.0.0"}}})
	f.disconnect()
	f.connect("9.0.0", &pb.UpdateStatus{State: "ok", From: "1.0.0", To: "9.0.0"}, true)
	h.waitNode(t, "ow", "online on 9.0.0", func(n map[string]any) bool {
		return isTrue(n["online"]) && n["version"] == "9.0.0" && nodeUpd(n) == nil
	})
}

// Edge case 7: a network blip without an update: offline, as ever.
func TestUpdateStateE2EBlip(t *testing.T) {
	t.Parallel()
	h := updateHub(t, "")
	f := ownedFakeWorker(t, h, "bw")
	f.connect("1.0.0", nil, false)
	h.waitNode(t, "bw", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	f.disconnect()
	h.waitNode(t, "bw", "offline", func(n map[string]any) bool { return !isTrue(n["online"]) && nodeUpd(n) == nil })
	if s := lsState(t, h, "bw"); !strings.Contains(s, " offline") || strings.Contains(s, "updat") {
		t.Fatalf("ls: %s", s)
	}
}

// Edge case 6: a session worker and its admin worker (one machine, one
// MSI update): the session worker shows its admin worker's update while
// both are away - one updating machine for its owner; another user, who
// may use the shared worker, sees the update without the admin worker.
func TestUpdateStateE2EPair(t *testing.T) {
	t.Parallel()
	a := newFakeAuthority(t)
	h := newHubEnv(t, a.hubConfig(`admins = ["boss@example.com"]`))
	erin := entraPanel(t, h, a, "e7e70000-0000-4000-8000-000000000002", "erin@example.com")
	codes := make(chan string, 1)
	var r *hubclient.JoinResult
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		r, err = hubclient.Link(ctx, h.addr(), hubclient.LinkOptions{JoinOptions: hubclient.JoinOptions{Name: "box", Fingerprint: h.fp}, Kind: "worker",
			AdminWorker: &hubclient.JoinOptions{Name: "box-admin"}, Show: func(st *pb.StartLinkResponse) { codes <- st.UserCode }})
	}()
	select {
	case code := <-codes:
		erin.decide(code, "approve")
	case <-time.After(time.Minute):
		t.Fatal("no pairing code")
	}
	<-done
	if err != nil || r.AdminWorker == nil {
		t.Fatalf("pairing: %v %v", err, r)
	}
	sw := newFakeWorker(t, h, "box", r)
	aw := newFakeWorker(t, h, "box-admin", r.AdminWorker)
	sw.connect("1.0.0", nil, false)
	aw.connect("1.0.0", nil, false)
	erin.waitNode("box", "paired, online", func(n map[string]any) bool { return isTrue(n["online"]) && n["companion"] == "box-admin" })
	// the admin worker (SYSTEM) reports the machine's MSI update; both restart
	aw.report("restarting", "installing", "1.0.0", "2.0.0", nil)
	erin.waitNode("box-admin", "installing", func(n map[string]any) bool { return nodeUpd(n)["phase"] == "installing" })
	sw.disconnect()
	aw.disconnect()
	n := erin.waitNode("box", "updating through its admin worker", func(n map[string]any) bool {
		u := nodeUpd(n)
		return !isTrue(n["online"]) && u["state"] == "updating" && u["via"] == "box-admin"
	})
	if u := nodeUpd(n); u["phase"] != "installing" || n["machine"] != "box" {
		t.Fatalf("box: %v", n)
	}
	erin.waitNode("box-admin", "updating", func(n map[string]any) bool { return nodeUpd(n)["state"] == "updating" && n["machine"] == "box" })
	// both back on the new version: one machine online, no update
	aw.connect("2.0.0", nil, false)
	sw.connect("2.0.0", nil, false)
	erin.waitNode("box", "online on 2.0.0", func(n map[string]any) bool {
		return isTrue(n["online"]) && n["version"] == "2.0.0" && nodeUpd(n) == nil
	})
}
