package supervisor

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/platform"
)

func TestMain(m *testing.M) {
	if os.Getenv("LONGARM_TEST_SUP_HELPER") == "sleep" {
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func sleeper(t *testing.T) (*exec.Cmd, uint64) {
	t.Helper()
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), "LONGARM_TEST_SUP_HELPER=sleep")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	st, err := platform.ProcessStartTime(c.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return c, st
}

// deadPID returns the pid of a process that has exited.
func deadPID(t *testing.T) (int, uint64) {
	c, st := sleeper(t)
	c.Process.Kill()
	c.Wait()
	return c.Process.Pid, st
}

func mkJob(t *testing.T, root, id string, st jobs.Status) jobs.Dir {
	t.Helper()
	d := jobs.Dir(filepath.Join(root, id))
	if err := os.MkdirAll(string(d), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteSpec(&jobs.Spec{ID: id, Argv: []string{"x"}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteStatus(&st); err != nil {
		t.Fatal(err)
	}
	return d
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestReconcileOnStartup(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	self, selfStart := os.Getpid(), uint64(0)
	selfStart, _ = platform.ProcessStartTime(self)
	deadShim, deadShimStart := deadPID(t)
	deadProc, deadProcStart := deadPID(t)
	orphan, orphanStart := sleeper(t)
	code := 0

	// Shim alive (this test process plays the shim) => re-adopted as running.
	mkJob(t, root, "alive", jobs.Status{State: jobs.Running, CreatedAt: now, ShimPID: self, ShimStart: selfStart, PID: self, PIDStart: selfStart})
	// Shim and process dead => lost.
	mkJob(t, root, "dead", jobs.Status{State: jobs.Running, CreatedAt: now, ShimPID: deadShim, ShimStart: deadShimStart, PID: deadProc, PIDStart: deadProcStart})
	// Shim dead, process alive => orphaned (read-only adoption).
	mkJob(t, root, "orphan", jobs.Status{State: jobs.Running, CreatedAt: now, ShimPID: deadShim, ShimStart: deadShimStart, PID: orphan.Process.Pid, PIDStart: orphanStart})
	// pid reused by another process (start time differs) => lost, not adopted.
	mkJob(t, root, "reused", jobs.Status{State: jobs.Running, CreatedAt: now, ShimPID: self, ShimStart: selfStart + 1, PID: self, PIDStart: selfStart + 1})
	// Terminal jobs are left alone.
	mkJob(t, root, "finished", jobs.Status{State: jobs.Done, CreatedAt: now, ExitCode: &code})
	// Preparing without a shim: grace period, then lost.
	mkJob(t, root, "prep", jobs.Status{State: jobs.Preparing, CreatedAt: now})
	// Kill was requested, shim died before acting => killed.
	kd := mkJob(t, root, "killreq", jobs.Status{State: jobs.Running, CreatedAt: now, ShimPID: deadShim, ShimStart: deadShimStart, PID: deadProc, PIDStart: deadProcStart})
	kd.RequestKill("test")

	m, err := New(Options{JobsDir: root, Logger: quiet(), OrphanGrace: 2 * time.Second}) // generous: loaded CI machines
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]jobs.State{
		"alive": jobs.Running, "dead": jobs.Lost, "orphan": jobs.Running, "reused": jobs.Lost,
		"finished": jobs.Done, "prep": jobs.Preparing, "killreq": jobs.Killed,
	}
	for id, s := range want {
		j, err := m.Get(id)
		if err != nil || j.Status.State != s {
			t.Errorf("%s: state %s (%v), want %s", id, j.Status.State, err, s)
		}
		// longarmd's decision is persisted for other readers of the job dir too.
		if b, _ := os.ReadFile(filepath.Join(root, id, jobs.StateFile)); string(b) != string(s) {
			t.Errorf("%s: state file %q, want %s", id, b, s)
		}
	}
	if j, _ := m.Get("orphan"); !j.Status.Orphaned {
		t.Error("orphan not flagged")
	}
	if m.RunningCount() != 3 {
		t.Errorf("running count %d", m.RunningCount())
	}

	time.Sleep(2100 * time.Millisecond)
	m.pollOnce()
	if j, _ := m.Get("prep"); j.Status.State != jobs.Lost {
		t.Errorf("prep after grace: %s", j.Status.State)
	}
	// The orphaned process exits => lost with unknown exit code.
	ch := m.Changed()
	orphan.Process.Kill()
	orphan.Wait()
	m.pollOnce()
	select {
	case <-ch:
	default:
		t.Error("no change notification")
	}
	if j, _ := m.Get("orphan"); j.Status.State != jobs.Lost || j.Status.ExitCode != nil {
		t.Errorf("orphan after exit: %+v", j.Status)
	}
	// List: newest first, filter by state.
	if l := m.List([]jobs.State{jobs.Lost}, 0); len(l) != 4 {
		t.Errorf("lost jobs: %d", len(l))
	}
}

func TestSubmitValidationAndIdempotency(t *testing.T) {
	root := t.TempDir()
	// A fake shim that exits immediately without touching anything: the job
	// must end up lost, proving longarmd notices a vanished shim it spawned.
	m, err := New(Options{JobsDir: root, Logger: quiet(), ShimCommand: func(string) *exec.Cmd {
		c := exec.Command(os.Args[0], "-test.run=^$")
		return c
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := m.Submit(ctx, jobs.Spec{ID: "../evil", Argv: []string{"x"}}, nil); err == nil {
		t.Fatal("path traversal id accepted")
	}
	if _, err := m.Submit(ctx, jobs.Spec{ID: "j1"}, nil); err == nil {
		t.Fatal("empty argv accepted")
	}
	spec := jobs.Spec{ID: "j1", Argv: []string{"a", "b"}, Env: map[string]string{"K": "V"}}
	j, err := m.Submit(ctx, spec, map[string]string{"TOKEN": "secret-value"})
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Spec.SecretEnvNames) != 1 || j.Spec.SecretEnvNames[0] != "TOKEN" {
		t.Fatalf("secret names %v", j.Spec.SecretEnvNames)
	}
	if _, err := m.Submit(ctx, spec, map[string]string{"TOKEN": "secret-value"}); err != nil {
		t.Fatalf("idempotent resubmit: %v", err)
	}
	spec2 := spec
	spec2.Argv = []string{"other"}
	if _, err := m.Submit(ctx, spec2, nil); err != ErrAlreadyExists {
		t.Fatalf("conflicting resubmit: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		m.pollOnce()
		if j, _ := m.Get("j1"); j.Status.State == jobs.Lost {
			break
		}
		if time.Now().After(deadline) {
			j, _ := m.Get("j1")
			t.Fatalf("state %s, want lost", j.Status.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(filepath.Join(root, "j1", jobs.SpecFile))
	if len(b) == 0 || strings.Contains(string(b), "secret-value") {
		t.Fatalf("job.json: %s", b)
	}
}

func TestReconcileOrphanKillAndMissingStatus(t *testing.T) {
	root := t.TempDir()
	deadShim, deadShimStart := deadPID(t)
	orphan, orphanStart := sleeper(t)
	mkJob(t, root, "orphan", jobs.Status{State: jobs.Running, CreatedAt: time.Now(),
		ShimPID: deadShim, ShimStart: deadShimStart, PID: orphan.Process.Pid, PIDStart: orphanStart})
	// job.json without status.json: longarmd died in the middle of Submit.
	nd := jobs.Dir(filepath.Join(root, "nostatus"))
	os.MkdirAll(string(nd), 0o755)
	nd.WriteSpec(&jobs.Spec{ID: "nostatus", Argv: []string{"x"}, CreatedAt: time.Now()})

	m, err := New(Options{JobsDir: root, Logger: quiet(), KillGrace: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if j, _ := m.Get("nostatus"); j.Status.State != jobs.Lost {
		t.Fatalf("nostatus: %+v", j.Status)
	}
	if j, _ := m.Get("orphan"); !j.Status.Orphaned || j.Status.State != jobs.Running {
		t.Fatalf("orphan: %+v", j.Status)
	}
	go m.Run(t.Context())
	j, err := m.Kill(t.Context(), "orphan")
	if err != nil || j.Status.State != jobs.Killed || *j.Status.ExitCode != platform.KilledExitCode {
		t.Fatalf("kill orphan: %+v %v", j.Status, err)
	}
	if platform.ProcessAlive(orphan.Process.Pid, orphanStart) {
		// reap so the zombie check on Unix is accurate
		orphan.Wait()
		if platform.ProcessAlive(orphan.Process.Pid, orphanStart) {
			t.Fatal("orphan survived kill")
		}
	}
}
