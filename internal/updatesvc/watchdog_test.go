package updatesvc

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"longarm/internal/update"
)

// A failure after the version is known (here: a package that fails
// verification) is recorded as the last result with its code, attempt and
// initiator - longarmd reports it to the hub; a busy refusal is not.
func TestPreLaunchFailureRecorded(t *testing.T) {
	r := newRig(t, "0.1.8")
	data := []byte("MSI package 0.1.9")
	r.publish("0.1.9", "amd64", data, r.sign([]byte("other"), update.MSIName("amd64"), "0.1.9"), true)
	resp := r.install(user, Request{Version: "0.1.9", Requester: "longarm worker update by alice via w1"})
	if resp.OK || resp.Code != CodeInvalid {
		t.Fatalf("%+v", resp)
	}
	l := r.svc.Status().Last
	if l == nil || l.OK || l.To != "0.1.9" || l.Reason != CodeInvalid || l.Initiator != InitiatorOwner || l.Attempt == "" || r.store.st.Last == nil {
		t.Fatalf("last %+v", l)
	}
	// a downgrade asked for by hand is recorded; one of automatic updates isn't
	r2 := newRig(t, "0.1.9")
	r2.good("0.1.8")
	if resp := r2.install(user, Request{Version: "0.1.8"}); resp.Code != CodeRefused {
		t.Fatalf("%+v", resp)
	}
	if l := r2.svc.Status().Last; l == nil || l.Reason != CodeRefused || l.Initiator != InitiatorLocal {
		t.Fatalf("downgrade: %+v", l)
	}
	r3 := newRig(t, "0.1.9")
	r3.good("0.1.8")
	r3.svc.install(context.Background(), &Request{Op: OpInstall, Latest: true}, "auto-update", InitiatorAuto)
	if l := r3.svc.Status().Last; l != nil {
		t.Fatalf("auto-update recorded %+v", l)
	}
}

// The status of an install in progress carries its attempt, initiator and
// the download's progress.
func TestStatusProgress(t *testing.T) {
	r := newRig(t, "0.1.8")
	r.good("0.1.9")
	r.src.hold = make(chan struct{})
	done := make(chan *Response)
	go func() { done <- r.install(user, Request{Version: "0.1.9", Requester: "longarm worker update by x"}) }()
	waitFor(t, func() bool { return r.svc.Status().State == PhaseDownloading })
	st := r.svc.Status()
	if st.Attempt == "" || st.Initiator != InitiatorOwner || st.BytesTotal <= 0 || st.BytesDone != 0 {
		t.Fatalf("downloading %+v", st)
	}
	close(r.src.hold)
	if resp := <-done; !resp.OK {
		t.Fatalf("%+v", resp)
	}
	if st := r.svc.Status(); st.State != PhaseInstalling || st.Attempt == "" {
		t.Fatalf("installing %+v", st)
	}
}

// The new service reconciles an install the MSI started it in the middle
// of: it waits for msiexec's exit code (3010 = a restart is pending).
func TestReconcileWaitsForExitCode(t *testing.T) {
	n := newRig(t, "0.1.9")
	n.store.st = &State{Pending: &Pending{From: "0.1.8", To: "0.1.9", Log: "l", Started: time.Now(), Attempt: "svc-1", Initiator: "auto"}}
	n.renew()
	if st := n.svc.Status(); st.State != PhaseInstalling || st.Last != nil || st.Attempt != "svc-1" {
		t.Fatalf("before msiexec ended: %+v", st)
	}
	n.launcher.mu.Lock()
	n.launcher.outcomes["l"] = 3010
	n.launcher.mu.Unlock()
	waitFor(t, func() bool { return n.svc.Status().Last != nil })
	if l := n.svc.Status().Last; !l.OK || !l.RebootRequired() || l.Attempt != "svc-1" || l.Initiator != "auto" {
		t.Fatalf("%+v", l)
	}
	// no exit code in the log at all: done (the new version runs) after ReconcileWait
	m := newRig(t, "0.1.9")
	m.cfg.ReconcileWait = 50 * time.Millisecond
	m.store.st = &State{Pending: &Pending{From: "0.1.8", To: "0.1.9", Log: "l", Started: time.Now()}}
	m.renew()
	waitFor(t, func() bool { return m.svc.Status().Last != nil })
	if l := m.svc.Status().Last; !l.OK {
		t.Fatalf("%+v", l)
	}
}

type fakeSCM struct {
	mu      sync.Mutex
	running map[string]bool
	started []string
	fail    error
}

func (f *fakeSCM) Running(n string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[n], nil
}

func (f *fakeSCM) Start(n string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, n)
	if f.fail != nil {
		return f.fail
	}
	f.running[n] = true
	return nil
}

func (f *fakeSCM) set(n string, run bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[n] = run
}

func (f *fakeSCM) startedList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.started)
}

func (r *rig) finishInstaller(code int) {
	r.launcher.mu.Lock()
	r.launcher.outcomes[r.launcher.launched[0][1]] = code
	r.launcher.mu.Unlock()
}

// The watchdog: after the installer ended (exit code known), the worker
// services that ran before the install and are stopped now are started
// again; one stopped before stays stopped; LongarmUpdate itself is not its
// business; an install that timed out (msiexec may still run) gets no
// recovery.
func TestWatchdogRestartsStoppedWorkers(t *testing.T) {
	scm := &fakeSCM{running: map[string]bool{"longarmd": true, "longarmd-admin": false, ServiceName: true}}
	r := newRig(t, "0.1.8")
	r.cfg.Services, r.cfg.WorkerServices = scm, func() []string { return []string{"longarmd", "longarmd-admin", ServiceName} }
	r.cfg.RecoverAfter, r.cfg.RecoverEvery = 20*time.Millisecond, 10*time.Millisecond
	r.renew()
	r.good("0.1.9")
	if resp := r.install(user, Request{Version: "0.1.9"}); !resp.OK {
		t.Fatalf("%+v", resp)
	}
	if p := r.store.st.Pending; p == nil || !slices.Equal(p.Running, []string{"longarmd"}) {
		t.Fatalf("pending %+v", p)
	}
	// msiexec stops the services; its rollback (exit 1603) leaves longarmd stopped
	scm.set("longarmd", false)
	r.finishInstaller(1603)
	waitFor(t, func() bool { l := r.svc.Status().Last; return l != nil && len(l.Recovered) > 0 })
	if got := scm.startedList(); !slices.Equal(got, []string{"longarmd"}) {
		t.Fatalf("started %v", got)
	}
	if l := r.svc.Status().Last; l.OK || !slices.Equal(l.Recovered, []string{"longarmd"}) || l.RecoverError != "" {
		t.Fatalf("last %+v", l)
	}

	// a service that won't start: tried RecoverTries times, the error recorded
	scm2 := &fakeSCM{running: map[string]bool{"longarmd": true}, fail: errors.New("logon failure")}
	f := newRig(t, "0.1.8")
	f.cfg.Services, f.cfg.WorkerServices = scm2, func() []string { return []string{"longarmd"} }
	f.cfg.RecoverAfter, f.cfg.RecoverEvery = 10*time.Millisecond, 5*time.Millisecond
	f.renew()
	f.good("0.1.9")
	f.install(user, Request{Version: "0.1.9"})
	scm2.set("longarmd", false)
	f.finishInstaller(0)
	waitFor(t, func() bool { l := f.svc.Status().Last; return l != nil && l.RecoverError != "" })
	if n := len(scm2.startedList()); n != DefaultRecoverTries {
		t.Fatalf("tried %d times", n)
	}

	// running again by itself (the installer restarted it): nothing to do
	scm4 := &fakeSCM{running: map[string]bool{"longarmd": true}}
	g := newRig(t, "0.1.8")
	g.cfg.Services, g.cfg.WorkerServices = scm4, func() []string { return []string{"longarmd"} }
	g.cfg.RecoverAfter = 10 * time.Millisecond
	g.renew()
	g.good("0.1.9")
	g.install(user, Request{Version: "0.1.9"})
	g.finishInstaller(0)
	time.Sleep(80 * time.Millisecond)
	if got := scm4.startedList(); len(got) != 0 {
		t.Fatalf("started a running service: %v", got)
	}

	// timed out (no exit code): no recovery
	scm3 := &fakeSCM{running: map[string]bool{}}
	x := newRig(t, "0.1.8")
	x.cfg.Services, x.cfg.WorkerServices = scm3, func() []string { return []string{"longarmd"} }
	x.cfg.RecoverAfter = time.Millisecond
	x.store.st = &State{Pending: &Pending{From: "0.1.8", To: "0.1.9", Log: "l", Started: time.Now().Add(-3 * time.Hour), Running: []string{"longarmd"}}}
	x.renew()
	time.Sleep(50 * time.Millisecond)
	if got := scm3.startedList(); len(got) != 0 {
		t.Fatalf("recovered after a timeout: %v", got)
	}
}
