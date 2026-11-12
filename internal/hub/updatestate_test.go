package hub

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"longarm/internal/hub/store"
	"longarm/internal/nodeid"
	"longarm/internal/pki"
	"longarm/internal/tenancy"
	pb "longarm/proto/longarm/v1"
)

// updHub is a hub (not serving: the tests drive time) with workers.
type updHub struct {
	t   *testing.T
	h   *Hub
	dir string
	seq uint64
}

func newUpdHub(t *testing.T, dir string) *updHub {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
		if _, err := InitCA(dir); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{DataDir: dir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "off"}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	h, err := Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return &updHub{t: t, h: h, dir: dir}
}

// worker adds an approved worker of owner running version.
func (u *updHub) worker(name, owner, version string, mod func(*store.Node)) *node {
	r := &store.Node{ID: nodeid.New(), Name: name, Role: string(pki.RoleWorker), State: stateApproved, Owner: owner, Version: version, Hostname: name}
	if mod != nil {
		mod(r)
	}
	if err := u.h.store.PutNode(r); err != nil {
		u.t.Fatal(err)
	}
	n := &node{rec: r}
	u.h.mu.Lock()
	u.h.nodes[r.ID] = n
	u.h.mu.Unlock()
	return n
}

// connect is Connect's part for the update: a session and the hello.
func (u *updHub) connect(n *node, version string, st *pb.UpdateStatus) *nodeSession {
	u.h.mu.Lock()
	defer u.h.mu.Unlock()
	u.seq++
	sess := &nodeSession{id: u.seq, out: make(chan *pb.HubCommand, 128), cancel: func() {}}
	n.sess, n.lastReport = sess, time.Now()
	hello := &pb.NodeHello{Version: version, Update: st}
	u.h.applyHelloLocked(n, hello)
	u.h.updateHelloLocked(n, hello, time.Now())
	n.encodeUpdate()
	u.h.store.PutNode(n.rec)
	return sess
}

func (u *updHub) disconnect(n *node) {
	u.h.mu.Lock()
	defer u.h.mu.Unlock()
	if n.sess != nil {
		u.h.setOfflineLocked(n, n.sess.id)
	}
}

func (u *updHub) report(n *node, sess *nodeSession, st *pb.UpdateStatus) {
	u.h.handleReport(n, sess, &pb.NodeReport{Msg: &pb.NodeReport_Update{Update: st}})
}

func (u *updHub) info(n *node) *pb.NodeInfo {
	u.h.mu.Lock()
	defer u.h.mu.Unlock()
	return u.h.nodeInfoLocked(n)
}

func (u *updHub) sweep(at time.Time) {
	u.h.mu.Lock()
	defer u.h.mu.Unlock()
	u.h.sweepUpdatesLocked(at)
}

// stored is the update state in the database.
func (u *updHub) stored(n *node) *pb.NodeUpdate {
	r, err := u.h.store.Node(n.rec.ID)
	if err != nil || r == nil || len(r.UpdateState) == 0 {
		return nil
	}
	s := &pb.NodeUpdate{}
	if protojson.Unmarshal(r.UpdateState, s) != nil {
		u.t.Fatalf("stored state: %s", r.UpdateState)
	}
	return s
}

func st(state, phase, from, to string) *pb.UpdateStatus {
	return &pb.UpdateStatus{State: state, Phase: phase, From: from, To: to, Attempt: "a1", Method: "msi", Initiator: "owner",
		Started: timestamppb.Now(), At: timestamppb.Now()}
}

func wantUpdate(t *testing.T, what string, got *pb.NodeUpdate, state, phase string) {
	t.Helper()
	if got.GetState() != state || got.GetPhase() != phase {
		t.Fatalf("%s: state %q phase %q, want %q %q (%v)", what, got.GetState(), got.GetPhase(), state, phase, got)
	}
}

// The happy path: phases with progress, the restart (offline: still
// updating, not "offline"), back on the new version: the state is gone.
// Progress reports are shown, not persisted.
func TestUpdateStateSuccess(t *testing.T) {
	u := newUpdHub(t, "")
	n := u.worker("w1", "alice", "0.1.29", nil)
	s := u.connect(n, "0.1.29", nil)
	if i := u.info(n); i.Update != nil {
		t.Fatalf("no update yet: %v", i.Update)
	}
	u.report(n, s, st("applying", "checking", "0.1.29", "0.1.30"))
	wantUpdate(t, "checking", u.info(n).Update, updUpdating, "checking")
	d := st("applying", "downloading", "0.1.29", "0.1.30")
	d.BytesDone, d.BytesTotal = 12<<20, 42<<20
	u.report(n, s, d)
	i := u.info(n).Update
	wantUpdate(t, "downloading", i, updUpdating, "downloading")
	if i.BytesDone != 12<<20 || i.BytesTotal != 42<<20 || i.Initiator != "owner" || i.Method != "msi" || !i.JobsKeepRunning || !i.Reported {
		t.Fatalf("downloading: %v", i)
	}
	if s := u.stored(n); s.GetPhase() != "downloading" || s.GetBytesDone() != 12<<20 {
		t.Fatalf("stored at the phase change: %v", s)
	}
	d.BytesDone = 30 << 20
	u.report(n, s, d)
	if s := u.stored(n); s.GetBytesDone() != 12<<20 {
		t.Fatalf("progress was persisted: %v", s)
	}
	if i := u.info(n).Update; i.BytesDone != 30<<20 {
		t.Fatalf("progress not shown: %v", i)
	}
	u.report(n, s, st("applying", "verifying", "0.1.29", "0.1.30"))
	u.report(n, s, st("restarting", "installing", "0.1.29", "0.1.30"))
	u.disconnect(n)
	i = u.info(n).Update
	wantUpdate(t, "away", i, updUpdating, "installing")
	if i.OfflineSince == nil || i.Deadline == nil {
		t.Fatalf("offline: %v", i)
	}
	u.connect(n, "0.1.30", &pb.UpdateStatus{State: "ok", From: "0.1.29", To: "0.1.30", Attempt: "a1", At: timestamppb.Now()})
	if i := u.info(n); i.Update != nil || !i.Online || i.Version != "0.1.30" {
		t.Fatalf("back: %v", i)
	}
	if s := u.stored(n); s != nil {
		t.Fatalf("stored after success: %v", s)
	}
}

// Edge case 1: the update fails (bad signature, sha256 mismatch, disk
// full, MSI error, downgrade refused): the worker is back on the old
// version and reports why - "failed" with a code, never stuck updating.
func TestUpdateStateFailedReconnectOldVersion(t *testing.T) {
	for _, c := range []struct{ err, code, want string }{
		{"refusing longarm-windows-amd64.msi 0.1.30: invalid signature", "", "signature"},
		{"longarmd-linux-amd64: sha256 ab, the hub says cd", "", "sha256"},
		{"write C:\\x: There is not enough space on the disk.", "", "disk"},
		{"the installer failed (msiexec exit code 1603; log C:\\ProgramData\\x.log)", "", "msi"},
		{"0.1.30 is older than 0.1.31: refusing a downgrade", "refused", "refused"},
		{"refusing a downgrade", "", "downgrade"},
	} {
		t.Run(c.want, func(t *testing.T) {
			u := newUpdHub(t, "")
			n := u.worker("w1", "alice", "0.1.29", nil)
			s := u.connect(n, "0.1.29", nil)
			u.report(n, s, st("applying", "installing", "0.1.29", "0.1.30"))
			u.disconnect(n)
			// back on the old version, the result (the service's Last) in the hello
			f := st("failed", "", "0.1.29", "0.1.30")
			f.Error, f.Code = c.err, c.code
			u.connect(n, "0.1.29", f)
			i := u.info(n).Update
			wantUpdate(t, "failed", i, updFailed, "installing")
			if i.Code != c.want || i.Error == "" || i.FinishedAt == nil || i.OfflineSince != nil {
				t.Fatalf("failed: %v", i)
			}
			if s := u.stored(n); s.GetState() != updFailed {
				t.Fatalf("stored: %v", s)
			}
			// it stays until a new attempt or the TTL: never "updating"
			u.sweep(time.Now().Add(2 * time.Hour))
			wantUpdate(t, "later", u.info(n).Update, updFailed, "installing")
			u.sweep(time.Now().Add(updateResultTTL + time.Hour))
			if i := u.info(n).Update; i != nil {
				t.Fatalf("after the TTL: %v", i)
			}
		})
	}
	// the failure arrives as a report after the hello on the old version
	// (the service records it a few seconds later): updating until then
	u := newUpdHub(t, "")
	n := u.worker("w1", "alice", "0.1.29", nil)
	s := u.connect(n, "0.1.29", nil)
	u.report(n, s, st("applying", "installing", "0.1.29", "0.1.30"))
	u.disconnect(n)
	s = u.connect(n, "0.1.29", nil)
	wantUpdate(t, "back, no result yet", u.info(n).Update, updUpdating, "installing")
	f := st("failed", "", "0.1.29", "0.1.30")
	f.Error = "the installer failed (msiexec exit code 1603)"
	u.report(n, s, f)
	wantUpdate(t, "reported", u.info(n).Update, updFailed, "installing")
}

// Edge case 2: the worker doesn't come back by the deadline (MSI hung,
// rollback, crash, machine off): timed_out with the last phase, an event
// for the owner; a late hello still resolves it.
func TestUpdateStateDeadlineTimesOut(t *testing.T) {
	u := newUpdHub(t, "")
	n := u.worker("w1", "alice", "0.1.29", nil)
	sub := u.h.bus.subscribeFiltered([]string{"node"}, "", eventFilter(tenancy.Caller{User: "alice"}))
	defer u.h.bus.unsubscribe(sub)
	s := u.connect(n, "0.1.29", nil)
	u.report(n, s, st("restarting", "installing", "0.1.29", "0.1.30"))
	u.disconnect(n)
	u.sweep(time.Now().Add(defaultUpdateRestart - time.Minute))
	wantUpdate(t, "within the deadline", u.info(n).Update, updUpdating, "installing")
	u.sweep(time.Now().Add(defaultUpdateRestart + time.Minute))
	i := u.info(n).Update
	wantUpdate(t, "past it", i, updTimedOut, "installing")
	if i.Code != "timeout" || i.StartedAt == nil || u.stored(n).GetState() != updTimedOut {
		t.Fatalf("timed out: %v", i)
	}
	saw := false
	for len(sub.ch) > 0 {
		ev := <-sub.ch
		saw = saw || strings.Contains(ev.Message, "didn't finish")
	}
	if !saw {
		t.Fatal("no event for the owner")
	}
	// back on the old version without a result: it failed
	u.connect(n, "0.1.29", nil)
	i = u.info(n).Update
	wantUpdate(t, "back old", i, updFailed, "installing")
	if i.Code != "timeout" || !strings.Contains(i.Error, "didn't finish") {
		t.Fatalf("back on the old version: %v", i)
	}
	// another worker: timed out, then back on the new version: done
	m := u.worker("w2", "alice", "0.1.29", nil)
	s = u.connect(m, "0.1.29", nil)
	u.report(m, s, st("restarting", "installing", "0.1.29", "0.1.30"))
	u.disconnect(m)
	u.sweep(time.Now().Add(time.Hour))
	wantUpdate(t, "w2", u.info(m).Update, updTimedOut, "installing")
	u.connect(m, "0.1.30", nil)
	if i := u.info(m).Update; i != nil {
		t.Fatalf("late success: %v", i)
	}
	// a phase repeated (the service reports "installing" every few seconds)
	// does not move the deadline; progress does
	k := u.worker("w3", "alice", "0.1.29", nil)
	s = u.connect(k, "0.1.29", nil)
	u.report(k, s, st("applying", "installing", "0.1.29", "0.1.30"))
	d0 := u.info(k).Update.Deadline.AsTime()
	time.Sleep(20 * time.Millisecond)
	u.report(k, s, st("applying", "installing", "0.1.29", "0.1.30"))
	if d := u.info(k).Update.Deadline.AsTime(); !d.Equal(d0) {
		t.Fatalf("a repeated phase moved the deadline: %v -> %v", d0, d)
	}
	// an update never runs past its max duration
	started := time.Now().Add(-defaultUpdateMax + time.Minute)
	p := st("applying", "downloading", "0.1.29", "0.1.30")
	p.Attempt, p.Started, p.BytesDone, p.BytesTotal = "a2", timestamppb.New(started), 1, 10
	u.report(k, s, p)
	if d := u.info(k).Update.Deadline.AsTime(); d.After(started.Add(defaultUpdateMax)) {
		t.Fatalf("deadline %v past the max %v", d, started.Add(defaultUpdateMax))
	}
}

// Edge case 3: msiexec 3010 (a restart of the machine is needed): not a
// failure - restart_pending, until the machine booted again.
func TestUpdateStateRestartPending(t *testing.T) {
	u := newUpdHub(t, "")
	n := u.worker("w1", "alice", "0.1.29", nil)
	s := u.connect(n, "0.1.29", nil)
	u.h.handleReport(n, s, &pb.NodeReport{Msg: &pb.NodeReport_Metrics{Metrics: &pb.MetricsSnapshot{UptimeSeconds: 3600}}})
	u.report(n, s, st("restarting", "installing", "0.1.29", "0.1.30"))
	u.disconnect(n)
	ok := st("ok", "", "0.1.29", "0.1.30")
	ok.RebootRequired = true
	s = u.connect(n, "0.1.30", ok)
	i := u.info(n).Update
	if i.GetState() != updRestartPending || i.Code != "" || i.Error != "" {
		t.Fatalf("3010: %v", i)
	}
	// the same boot (uptime keeps growing): still pending
	u.h.handleReport(n, s, &pb.NodeReport{Msg: &pb.NodeReport_Metrics{Metrics: &pb.MetricsSnapshot{UptimeSeconds: 3700}}})
	if u.info(n).Update.GetState() != updRestartPending {
		t.Fatal("cleared without a reboot")
	}
	// rebooted (uptime from zero): done
	u.h.mu.Lock()
	n.rec.UpdateBootAt = n.rec.UpdateBootAt.Add(-time.Hour) // the boot before the update, an hour ago
	u.h.mu.Unlock()
	u.h.handleReport(n, s, &pb.NodeReport{Msg: &pb.NodeReport_Metrics{Metrics: &pb.MetricsSnapshot{UptimeSeconds: 30}}})
	if i := u.info(n).Update; i != nil {
		t.Fatalf("after the reboot: %v", i)
	}
	// the result reported again after the reboot (the service keeps it): done
	s = u.connect(n, "0.1.30", ok)
	if i := u.info(n).Update; i != nil {
		t.Fatalf("the 3010 came back after the reboot: %v", i)
	}

	// the order of an MSI update: the new version says hello (done), the
	// service reports msiexec's 3010 a moment later
	m := u.worker("w2", "alice", "0.1.29", nil)
	s = u.connect(m, "0.1.29", nil)
	u.report(m, s, st("restarting", "installing", "0.1.29", "0.1.30"))
	u.disconnect(m)
	s = u.connect(m, "0.1.30", nil)
	if i := u.info(m).Update; i != nil {
		t.Fatalf("hello on the new version: %v", i)
	}
	u.report(m, s, ok)
	if i := u.info(m).Update; i.GetState() != updRestartPending {
		t.Fatalf("3010 after the hello: %v", i)
	}
}

// Edge case 4: running jobs and sessions keep running with both update
// methods (the shims outlive longarmd); a generic update of an older
// worker doesn't claim it.
func TestUpdateStateJobsKeepRunning(t *testing.T) {
	u := newUpdHub(t, "")
	for _, m := range []string{"msi", "self"} {
		n := u.worker("w-"+m, "alice", "0.1.29", nil)
		s := u.connect(n, "0.1.29", nil)
		r := st("applying", "downloading", "0.1.29", "0.1.30")
		r.Method = m
		u.report(n, s, r)
		if i := u.info(n).Update; !i.JobsKeepRunning || i.Method != m {
			t.Fatalf("%s: %v", m, i)
		}
	}
	old := u.worker("old", "alice", "0.1.20", nil)
	s := u.connect(old, "0.1.20", nil)
	u.report(old, s, &pb.UpdateStatus{State: "applying", From: "0.1.20", To: "0.1.30"})
	if i := u.info(old).Update; i.JobsKeepRunning || i.Reported != true || i.Phase != "" {
		t.Fatalf("older worker: %v", i)
	}
}

// Edge case 5: duplicate clicks and concurrent updates: the update that
// runs stays (a rollout's request doesn't replace it; a repeated report
// of the attempt is the same update).
func TestUpdateStateDuplicateUpdate(t *testing.T) {
	u := newUpdHub(t, "")
	n := u.worker("w1", "alice", "0.1.29", func(r *store.Node) { r.UpdateEnabled = true })
	s := u.connect(n, "0.1.29", nil)
	u.report(n, s, st("applying", "downloading", "0.1.29", "0.1.30"))
	started := u.info(n).Update.StartedAt.AsTime()
	u.h.mu.Lock()
	u.h.updateRequestedLocked(n, "0.1.31", time.Now())
	u.h.mu.Unlock()
	i := u.info(n).Update
	if i.To != "0.1.30" || i.Initiator != "owner" || i.State != updUpdating {
		t.Fatalf("a rollout replaced the running update: %v", i)
	}
	u.report(n, s, st("applying", "downloading", "0.1.29", "0.1.30"))
	if i := u.info(n).Update; !i.StartedAt.AsTime().Equal(started) {
		t.Fatalf("the same attempt restarted: %v", i)
	}
}

// Edge case 6: the session worker and the admin worker restart in one
// MSI update: the one that doesn't report shows its companion's update
// (one updating machine, not an offline card); other users see neither
// the admin worker's name nor the worker's words.
func TestUpdateStateCompanionShownAsOne(t *testing.T) {
	u := newUpdHub(t, "")
	sid, aid := nodeid.New(), nodeid.New()
	sw := u.worker("box", "alice", "0.1.29", func(r *store.Node) {
		r.ID, r.Companion, r.Shares = sid, aid, &store.WorkerShares{Everyone: true}
	})
	aw := u.worker("box-admin", "alice", "0.1.29", func(r *store.Node) { r.ID, r.Companion, r.Admin = aid, sid, true })
	u.connect(sw, "0.1.29", nil)
	s := u.connect(aw, "0.1.29", nil)
	r := st("applying", "installing", "0.1.29", "0.1.30")
	r.Message = "installing 0.1.30 from C:\\ProgramData\\Longarm\\updates"
	u.report(aw, s, r)
	u.disconnect(sw)
	u.disconnect(aw)
	si := u.info(sw)
	wantUpdate(t, "session worker", si.Update, updUpdating, "installing")
	if si.Update.Via != "box-admin" || si.Update.OfflineSince == nil {
		t.Fatalf("session worker: %v", si.Update)
	}
	wantUpdate(t, "admin worker", u.info(aw).Update, updUpdating, "installing")
	bob := nodeView(tenancy.Caller{User: "bob"}, u.info(sw))
	if bob.Update.GetState() != updUpdating || bob.Update.Via != "" || bob.Update.Message != "" {
		t.Fatalf("bob's view: %v", bob.Update)
	}
	if a := nodeView(tenancy.Caller{User: "alice"}, u.info(sw)); a.Update.Via != "box-admin" || a.Update.Message == "" {
		t.Fatalf("alice's view: %v", a.Update)
	}
	// both back on the new version: no state anywhere
	u.connect(aw, "0.1.30", nil)
	u.connect(sw, "0.1.30", nil)
	if u.info(sw).Update != nil || u.info(aw).Update != nil {
		t.Fatalf("after: %v / %v", u.info(sw).Update, u.info(aw).Update)
	}
	// a self-update of the admin worker's companion is not inherited
	s = u.connect(sw, "0.1.30", nil)
	r = st("applying", "downloading", "0.1.30", "0.1.31")
	r.Method = "self"
	u.report(sw, s, r)
	if u.info(aw).Update != nil {
		t.Fatal("a self-update was shown on the companion")
	}
}

// Edge case 7: a plain network blip without an update: offline, as ever.
func TestUpdateStateNetworkBlipOffline(t *testing.T) {
	u := newUpdHub(t, "")
	n := u.worker("w1", "alice", "0.1.29", nil)
	u.connect(n, "0.1.29", nil)
	u.disconnect(n)
	if i := u.info(n); i.Online || i.Update != nil {
		t.Fatalf("blip: %v", i)
	}
	u.sweep(time.Now().Add(time.Hour))
	if i := u.info(n); i.Update != nil {
		t.Fatalf("blip, later: %v", i)
	}
	// an old result in a hello (an update long ago) makes no state either
	old := &pb.UpdateStatus{State: "failed", From: "0.1.28", To: "0.1.29", Error: "x", At: timestamppb.New(time.Now().Add(-48 * time.Hour))}
	u.connect(n, "0.1.29", old)
	if i := u.info(n); i.Update != nil {
		t.Fatalf("stale result: %v", i.Update)
	}
}

// Edge case 8: the hub restarts during an update: the state survives
// (and its deadline leaves room for the reconnect); a state the hub lost
// is rebuilt from the worker's hello.
func TestUpdateStateSurvivesHubRestart(t *testing.T) {
	u := newUpdHub(t, "")
	n := u.worker("w1", "alice", "0.1.29", nil)
	s := u.connect(n, "0.1.29", nil)
	u.report(n, s, st("restarting", "installing", "0.1.29", "0.1.30"))
	u.disconnect(n)
	// the deadline was just about to pass when the hub went down
	u.h.mu.Lock()
	n.upd.Deadline = timestamppb.New(time.Now().Add(-time.Second))
	u.h.saveUpdateLocked(n)
	u.h.mu.Unlock()
	u.h.Close()
	u2 := newUpdHub(t, u.dir)
	var m *node
	for _, x := range u2.h.nodes {
		if x.rec.Name == "w1" {
			m = x
		}
	}
	i := u2.info(m).Update
	wantUpdate(t, "after the restart", i, updUpdating, "installing")
	if d := i.Deadline.AsTime(); d.Before(time.Now().Add(updateStartGrace - 5*time.Second)) {
		t.Fatalf("deadline %v leaves no room to reconnect", d)
	}
	u2.connect(m, "0.1.30", nil)
	if i := u2.info(m).Update; i != nil {
		t.Fatalf("back: %v", i)
	}
	// a state the hub lost: the hello of a worker in its health check
	k := u2.worker("w2", "alice", "0.1.29", nil)
	u2.connect(k, "0.1.30", &pb.UpdateStatus{State: "checking", From: "0.1.29", To: "0.1.30", At: timestamppb.Now()})
	wantUpdate(t, "rebuilt", u2.info(k).Update, updUpdating, "health_check")
}

// Edge case 9: an older worker (0.1.15-0.1.29) that reports no phases:
// a rollout makes a generic "requested", its legacy "applying" makes it
// "updating" (no phase); one that never takes it up loses it quietly and
// is not asked again.
func TestUpdateStateOldWorkerGeneric(t *testing.T) {
	u := newUpdHub(t, "")
	n := u.worker("w1", "", "0.1.20", func(r *store.Node) { r.UpdateEnabled = true })
	s := u.connect(n, "0.1.20", nil)
	u.h.mu.Lock()
	u.h.updateRequestedLocked(n, "0.1.30", time.Now())
	u.h.mu.Unlock()
	i := u.info(n).Update
	if i.State != updRequested || i.Reported || i.Initiator != "rollout" || i.StartedAt == nil {
		t.Fatalf("requested: %v", i)
	}
	u.report(n, s, &pb.UpdateStatus{State: "applying", From: "0.1.20", To: "0.1.30"})
	i = u.info(n).Update
	if i.State != updUpdating || i.Phase != "" || i.Initiator != "rollout" || !i.Reported {
		t.Fatalf("applying: %v", i)
	}
	u.report(n, s, &pb.UpdateStatus{State: "restarting", From: "0.1.20", To: "0.1.30"})
	u.disconnect(n)
	wantUpdate(t, "restarting", u.info(n).Update, updUpdating, "restarting")
	u.connect(n, "0.1.30", &pb.UpdateStatus{State: "checking", From: "0.1.20", To: "0.1.30"})
	wantUpdate(t, "health check", u.info(n).Update, updUpdating, "health_check")
	s = u.connect(n, "0.1.30", nil)
	u.report(n, s, &pb.UpdateStatus{State: "ok", From: "0.1.20", To: "0.1.30"})
	if i := u.info(n).Update; i != nil {
		t.Fatalf("done: %v", i)
	}
	// never taken up while online: dropped, not requested again
	k := u.worker("w2", "", "0.1.20", func(r *store.Node) { r.UpdateEnabled = true })
	u.connect(k, "0.1.20", nil)
	u.h.mu.Lock()
	u.h.updateRequestedLocked(k, "0.1.30", time.Now())
	u.h.mu.Unlock()
	u.sweep(time.Now().Add(defaultUpdateRequested + time.Minute))
	if i := u.info(k).Update; i != nil {
		t.Fatalf("not taken up: %v", i)
	}
	u.h.mu.Lock()
	u.h.updateRequestedLocked(k, "0.1.30", time.Now())
	u.h.mu.Unlock()
	if i := u.info(k).Update; i != nil {
		t.Fatalf("requested again: %v", i)
	}
	// requested while it is offline (or goes offline): it may be restarting
	m := u.worker("w3", "", "0.1.20", func(r *store.Node) { r.UpdateEnabled = true })
	u.h.mu.Lock()
	u.h.updateRequestedLocked(m, "0.1.30", time.Now())
	u.h.mu.Unlock()
	u.sweep(time.Now().Add(defaultUpdateRestart + time.Minute))
	if i := u.info(m).Update; i.GetState() != updTimedOut {
		t.Fatalf("offline: %v", i)
	}
}

// Edge case 10: spoofing - only the node's own current session sets its
// state (a replaced session's report is dropped; there is no client
// request for it); other users see a stripped state, or nothing.
func TestUpdateStateSpoofing(t *testing.T) {
	u := newUpdHub(t, "")
	n := u.worker("w1", "alice", "0.1.29", nil)
	other := u.worker("w2", "carol", "0.1.29", nil)
	old := u.connect(n, "0.1.29", nil)
	u.connect(n, "0.1.29", nil) // a newer session replaced it
	u.report(n, old, st("applying", "downloading", "0.1.29", "0.1.30"))
	if i := u.info(n).Update; i != nil {
		t.Fatalf("a replaced session set the state: %v", i)
	}
	// carol's worker reports for itself only
	cs := u.connect(other, "0.1.29", nil)
	u.report(other, cs, st("applying", "downloading", "0.1.29", "0.1.30"))
	if u.info(n).Update != nil || u.info(other).Update == nil {
		t.Fatal("reports reached another node")
	}
	// invalid versions, non-workers, revoked nodes: nothing
	cl := u.worker("c1", "alice", "0.1.29", func(r *store.Node) { r.Role = string(pki.RoleClient) })
	cls := u.connect(cl, "0.1.29", nil)
	u.report(cl, cls, st("applying", "downloading", "0.1.29", "0.1.30"))
	if u.info(cl).Update != nil {
		t.Fatal("a client has an update state")
	}
	s := u.connect(n, "0.1.29", nil)
	u.report(n, s, st("applying", "downloading", "0.1.29", "../../x"))
	if u.info(n).Update != nil {
		t.Fatal("an invalid version made a state")
	}
	// another user sees no error text; a user who can't see the worker
	// doesn't get it at all
	f := st("failed", "", "0.1.29", "0.1.30")
	f.Error = "refusing C:\\Users\\alice\\secret.msi"
	u.report(n, s, f)
	if v := nodeView(tenancy.Caller{User: "bob"}, u.info(n)); v.Update.GetError() != "" || v.Update.GetCode() == "" {
		t.Fatalf("bob: %v", v.Update)
	}
	for _, x := range u.h.ListNodesFor(tenancy.Caller{User: "bob"}, false) {
		if x.Name == "w1" {
			t.Fatal("bob lists alice's unshared worker")
		}
	}
	u.h.mu.Lock()
	n.rec.State = stateRevoked
	u.h.sweepUpdatesLocked(time.Now())
	u.h.mu.Unlock()
	if u.info(n).Update != nil {
		t.Fatal("a revoked worker keeps its state")
	}
}

// Edge case 11 (the hub's side): the update window is visible to clients
// as offline_since + phase, which they show instead of link errors; a
// worker that went away without an update shows none (case 7).
func TestUpdateStateOfflineWindow(t *testing.T) {
	u := newUpdHub(t, "")
	n := u.worker("w1", "alice", "0.1.29", nil)
	s := u.connect(n, "0.1.29", nil)
	u.report(n, s, st("restarting", "restarting", "0.1.29", "0.1.30"))
	u.disconnect(n)
	i := u.info(n)
	if i.Online || i.Update.GetOfflineSince() == nil || i.Update.GetState() != updUpdating {
		t.Fatalf("away: %v", i)
	}
	u.connect(n, "0.1.29", nil) // a blip before the restart: back, still updating
	if i := u.info(n).Update; i.GetOfflineSince() != nil || i.GetState() != updUpdating {
		t.Fatalf("back: %v", i)
	}
}

// Edge case 12: it came back on another version than the target.
func TestUpdateStateVersionMismatch(t *testing.T) {
	u := newUpdHub(t, "")
	n := u.worker("w1", "alice", "0.1.29", nil)
	s := u.connect(n, "0.1.29", nil)
	u.report(n, s, st("restarting", "installing", "0.1.29", "0.1.30"))
	u.disconnect(n)
	u.connect(n, "0.1.31", nil)
	i := u.info(n).Update
	if i.GetState() != updMismatch || i.Code != "version" || !strings.Contains(i.Error, "0.1.31") {
		t.Fatalf("mismatch: %v", i)
	}
}

// A result the hub resolved already (a failure) is not brought back by a
// hello that repeats it; the record older hubs read keeps the old fields.
func TestUpdateStateReplayAndLegacyRecord(t *testing.T) {
	u := newUpdHub(t, "")
	n := u.worker("w1", "alice", "0.1.29", nil)
	s := u.connect(n, "0.1.29", nil)
	u.report(n, s, st("applying", "downloading", "0.1.29", "0.1.30"))
	f := st("failed", "", "0.1.29", "0.1.30")
	f.Error = "sha256 mismatch"
	u.report(n, s, f)
	u.sweep(time.Now().Add(updateResultTTL + time.Hour))
	if u.info(n).Update != nil {
		t.Fatal("not expired")
	}
	u.connect(n, "0.1.29", f)
	if i := u.info(n).Update; i != nil {
		t.Fatalf("a replayed failure came back: %v", i)
	}
	r, _ := u.h.store.Node(n.rec.ID)
	var legacy map[string]any
	if err := protojson.Unmarshal(r.Update, &pb.UpdateStatus{}); err != nil || strings.Contains(string(r.Update), "attempt") ||
		strings.Contains(string(r.Update), "phase") {
		t.Fatalf("legacy record %s (%v %v)", r.Update, err, legacy)
	}
}
