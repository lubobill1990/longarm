package update

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
)

// A child process that just waits (a stand-in for a watchdog).
func TestMain(m *testing.M) {
	if os.Getenv("LONGARM_UPDATE_TEST_SLEEP") == "1" {
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func sleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "LONGARM_UPDATE_TEST_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

// TestWatchdogSuperseded: the watchdog of a finished update that is still
// polling when the next update is applied leaves that update to its own
// watchdog. Two watchdogs rolling back the same files at once left
// longarm missing (e2e TestSelfUpdate: "fork/exec .../longarm: no such
// file or directory").
func TestWatchdogSuperseded(t *testing.T) {
	f := newFakeInstall(t)
	if err := Apply(context.Background(), f.cfg, "2.0.0", f.files, f.fetch); err != nil {
		t.Fatal(err)
	}
	other := sleeper(t) // the newer update's watchdog, alive
	st, _ := LoadState(f.data)
	st.Phase, st.Error, st.WatchdogPID = PhaseUnhealthy, "fatal: broken", other.Process.Pid
	st.WatchdogStart, _ = platform.ProcessStartTime(other.Process.Pid)
	st.Save(f.data)
	done := make(chan error, 1)
	go func() { done <- Watch(context.Background(), f.data, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the superseded watchdog did not leave")
	}
	if f.read(t, "longarmd") != "longarmd v2" || f.read(t, "longarm") != "longarm v2" {
		t.Fatal("a superseded watchdog rolled back")
	}
	if st, _ := LoadState(f.data); st.Phase != PhaseUnhealthy {
		t.Fatalf("phase %s", st.Phase)
	}
}

// TestApplyStopsFinishedWatchdog: Apply stops the previous update's
// watchdog when that update is over (watchdogs of older versions don't
// know about WatchdogPID).
func TestApplyStopsFinishedWatchdog(t *testing.T) {
	f := newFakeInstall(t)
	old := sleeper(t)
	pid := old.Process.Pid
	start, _ := platform.ProcessStartTime(pid)
	(&State{From: "0.9.0", To: "1.0.0", Phase: PhaseOK, WatchdogPID: pid, WatchdogStart: start}).Save(f.data)
	exited := make(chan struct{})
	go func() { old.Process.Wait(); close(exited) }()
	if err := Apply(context.Background(), f.cfg, "2.0.0", f.files, f.fetch); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatal("the previous update's watchdog still runs")
	}
}

// An update in progress keeps its watchdog.
func TestApplyKeepsRunningWatchdog(t *testing.T) {
	f := newFakeInstall(t)
	w := sleeper(t)
	pid := w.Process.Pid
	start, _ := platform.ProcessStartTime(pid)
	(&State{From: "0.9.0", To: "1.0.0", Phase: PhaseChecking, WatchdogPID: pid, WatchdogStart: start}).Save(f.data)
	Apply(context.Background(), f.cfg, "2.0.0", f.files, f.fetch)
	if !platform.ProcessAlive(pid, start) {
		t.Fatal("the watchdog of an update in progress was stopped")
	}
}

// TestWatchdogReplacesDeadOne: a replacement watchdog (ensureWatchdog after
// a reboot or a watchdog crash) reads update.json before its own PID is
// saved there: the recorded PID is its dead predecessor's. It must not take
// that for a newer watchdog and leave the update unprotected: it rolls back.
func TestWatchdogReplacesDeadOne(t *testing.T) {
	f := newFakeInstall(t)
	f.cfg.Args = nil
	if err := Apply(context.Background(), f.cfg, "2.0.0", f.files, f.fetch); err != nil {
		t.Fatal(err)
	}
	dead := sleeper(t)
	deadStart, _ := platform.ProcessStartTime(dead.Process.Pid)
	dead.Process.Kill()
	dead.Wait()
	st, _ := LoadState(f.data)
	st.Phase, st.Error = PhaseUnhealthy, "fatal: broken"
	st.WatchdogPID, st.WatchdogStart = dead.Process.Pid, deadStart
	st.Save(f.data)
	Watch(context.Background(), f.data, nil) // the exec-mode restart of the fake binary fails; the files are restored regardless
	if f.read(t, "longarmd") != "longarmd v1" || f.read(t, "longarm") != "longarm v1" {
		t.Fatalf("not rolled back: %q %q", f.read(t, "longarmd"), f.read(t, "longarm"))
	}
	if st, _ := LoadState(f.data); st.Phase != PhaseRolledBack {
		t.Fatalf("phase %s (%s)", st.Phase, st.Error)
	}
}
