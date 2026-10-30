package supervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/platform"
)

// TestReconcileReplacedEntry: a reconcile of a job that was removed and
// replaced under the same id (a session started again) - its shim's exit
// handler, or a poll that listed it before the removal - must not read the
// newer job's status from the shared directory and mark it lost (seen as
// TestSessionRaces "the new session was hit": "job lost ... supervisor
// (shim) exited unexpectedly").
func TestReconcileReplacedEntry(t *testing.T) {
	root := t.TempDir()
	code := 0
	mkJob(t, root, "gen", jobs.Status{State: jobs.Done, CreatedAt: time.Now(), ExitCode: &code})
	m, err := New(Options{JobsDir: root, Logger: quiet(), OrphanGrace: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	old := m.jobs["gen"]
	m.mu.Unlock()
	old.shimDone = make(chan struct{}) // spawned by this longarmd; its shim has exited
	close(old.shimDone)
	if err := m.Remove("gen"); err != nil {
		t.Fatal(err)
	}
	// the newer generation: running, its shim alive (this process)
	self := os.Getpid()
	selfStart, _ := platform.ProcessStartTime(self)
	deadProc, deadProcStart := deadPID(t)
	st := jobs.Status{State: jobs.Running, CreatedAt: time.Now(), ShimPID: self, ShimStart: selfStart, PID: deadProc, PIDStart: deadProcStart}
	d := mkJob(t, root, "gen", st)
	spec, _ := d.ReadSpec()
	m.mu.Lock()
	m.jobs["gen"] = &entry{dir: d, spec: *spec, st: st, loadedAt: time.Now()}
	m.mu.Unlock()

	m.reconcile(old)
	if b, _ := os.ReadFile(filepath.Join(root, "gen", jobs.StateFile)); string(b) != string(jobs.Running) {
		t.Fatalf("the newer job's state file: %q", b)
	}
	if j, _ := m.Get("gen"); j.Status.State != jobs.Running {
		t.Fatalf("the newer job: %+v", j.Status)
	}
}
