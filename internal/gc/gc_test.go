package gc

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/supervisor"
	"github.com/lubobill1990/longarm/internal/workspace"
)

// mkJob writes a job dir in a given state, ended at the given time.
func mkJob(t *testing.T, root, kind, id string, state jobs.State, ended time.Time, workdir string) {
	t.Helper()
	d := jobs.Dir(filepath.Join(root, kind, id))
	os.MkdirAll(string(d), 0o755)
	spec := &jobs.Spec{ID: id, Argv: []string{"x"}, CreatedAt: ended.Add(-time.Minute)}
	if err := d.WriteSpec(spec); err != nil {
		t.Fatal(err)
	}
	code := 0
	st := &jobs.Status{State: state, CreatedAt: spec.CreatedAt, EndedAt: ended, Workdir: workdir}
	if state.Terminal() {
		st.ExitCode = &code
	} else {
		// A live "shim": this test process.
		st.ShimPID = os.Getpid()
	}
	if err := d.WriteStatus(st); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(d.Path(jobs.LogFile), make([]byte, 1000), 0o644)
}

func TestCollect(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	wt := filepath.Join(root, workspace.WorktreesDir, "old")
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(wt, "f"), make([]byte, 500), 0o644)
	mkJob(t, root, "jobs", "old", jobs.Done, now.Add(-10*24*time.Hour), wt)
	mkJob(t, root, "jobs", "mid", jobs.Failed, now.Add(-3*24*time.Hour), "")
	mkJob(t, root, "jobs", "new", jobs.Done, now.Add(-time.Hour), "")
	mkJob(t, root, "sessions", "sold", jobs.Done, now.Add(-9*24*time.Hour), "")
	mkJob(t, root, "jobs", "running", jobs.Running, time.Time{}, "")
	// Stale staging and an orphaned worktree.
	stale := filepath.Join(root, workspace.StagingDir, "stale")
	os.MkdirAll(stale, 0o755)
	old := now.Add(-48 * time.Hour)
	os.Chtimes(stale, old, old)
	orphan := filepath.Join(root, workspace.WorktreesDir, "ORPHAN1")
	os.MkdirAll(orphan, 0o755)
	os.Chtimes(orphan, old, old)

	opts := supervisor.Options{Logger: log, ShimCommand: func(string) *exec.Cmd { return exec.Command("true") }}
	o1, o2 := opts, opts
	o1.JobsDir, o2.JobsDir = filepath.Join(root, "jobs"), filepath.Join(root, "sessions")
	jm, err := supervisor.New(o1)
	if err != nil {
		t.Fatal(err)
	}
	sm, err := supervisor.New(o2)
	if err != nil {
		t.Fatal(err)
	}
	freePct := 50.0
	c := &Collector{DataDir: root, Jobs: jm, Sessions: sm, WS: &workspace.Store{Root: root}, Log: log,
		DiskUsage: func(string) (uint64, uint64, error) { return uint64(freePct), 100, nil }}

	has := func(res Result, name string) bool {
		for _, r := range res.Removed {
			if r == name {
				return true
			}
		}
		return false
	}
	// Dry run removes nothing.
	res := c.Run(context.Background(), Options{Keep: 7 * 24 * time.Hour, MinFreePercent: 10, DryRun: true})
	if !has(res, "jobs/old") || !has(res, "sessions/sold") || has(res, "jobs/mid") || res.Freed < 1500 {
		t.Fatalf("dry run: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", "old")); err != nil {
		t.Fatal("dry run deleted")
	}
	// Age pass.
	res = c.Run(context.Background(), Options{Keep: 7 * 24 * time.Hour, MinFreePercent: 10})
	if !has(res, "jobs/old") || !has(res, "sessions/sold") || !has(res, "staging/stale") || !has(res, "worktrees/ORPHAN1") || has(res, "jobs/mid") {
		t.Fatalf("age pass: %+v", res)
	}
	for _, p := range []string{filepath.Join(root, "jobs", "old"), wt, stale, orphan} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived", p)
		}
	}
	if _, err := jm.Get("old"); err == nil {
		t.Fatal("manager still knows the removed job")
	}
	// Disk pressure: oldest terminal first, never the running one.
	freePct = 5
	res = c.Run(context.Background(), Options{Keep: 7 * 24 * time.Hour, MinFreePercent: 10})
	if !has(res, "jobs/mid") || !has(res, "jobs/new") || has(res, "jobs/running") {
		t.Fatalf("pressure pass: %+v", res)
	}
	if _, err := jm.Get("running"); err != nil {
		t.Fatal("running job removed")
	}
}

// Session worktrees (worktrees/s.<name>) live as long as the session: never
// collected while it runs or while its ended data is kept, removed with
// it, and collected as orphans once the session is gone.
func TestCollectSessionWorktrees(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	wt := func(name string) string {
		p := filepath.Join(root, workspace.WorktreesDir, jobs.SessionWorktreeKey(name, now))
		os.MkdirAll(p, 0o755)
		os.Chtimes(p, old, old)
		return p
	}
	live, kept, ended, gone := wt("live"), wt("kept"), wt("ended"), wt("gone")
	// an older generation of "live" (the session that ended and was replaced)
	oldGen := filepath.Join(root, workspace.WorktreesDir, jobs.SessionWorktreeKey("live", now.Add(-time.Hour)))
	os.MkdirAll(oldGen, 0o755)
	os.Chtimes(oldGen, old, old)
	// A job id equal to a session name must not protect or remove it.
	mkJob(t, root, "jobs", "gone", jobs.Running, time.Time{}, "")
	mkJob(t, root, "sessions", "live", jobs.Running, time.Time{}, live)
	mkJob(t, root, "sessions", "kept", jobs.Done, now.Add(-time.Hour), kept)
	mkJob(t, root, "sessions", "ended", jobs.Done, now.Add(-10*24*time.Hour), ended)
	opts := supervisor.Options{Logger: log, ShimCommand: func(string) *exec.Cmd { return exec.Command("true") }}
	o1, o2 := opts, opts
	o1.JobsDir, o2.JobsDir = filepath.Join(root, "jobs"), filepath.Join(root, "sessions")
	jm, err := supervisor.New(o1)
	if err != nil {
		t.Fatal(err)
	}
	sm, err := supervisor.New(o2)
	if err != nil {
		t.Fatal(err)
	}
	c := &Collector{DataDir: root, Jobs: jm, Sessions: sm, WS: &workspace.Store{Root: root}, Log: log,
		DiskUsage: func(string) (uint64, uint64, error) { return 50, 100, nil }}
	res := c.Run(context.Background(), Options{Keep: 7 * 24 * time.Hour})
	for _, p := range []string{live, kept} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %+v", p, res)
		}
	}
	for _, p := range []string{ended, gone, oldGen} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived: %+v", p, res)
		}
	}
}
