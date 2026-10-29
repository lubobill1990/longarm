package hub

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStartupMarkerHeartbeat(t *testing.T) {
	p := filepath.Join(t.TempDir(), StartupMarkerFile)
	m := NewStartupMarker(p, 20*time.Millisecond)
	if _, _, ok, err := StartupInProgress(p, time.Minute, time.Now()); ok || err != nil {
		t.Fatalf("no marker yet: ok=%v err=%v", ok, err)
	}
	if err := m.Begin("restore", 0); err != nil {
		t.Fatal(err)
	}
	// backdate it: the heartbeat must bring the mtime forward again
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		phase, _, ok, err := StartupInProgress(p, 10*time.Second, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if phase != "restore" {
				t.Fatalf("phase %q", phase)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("heartbeat did not refresh the marker")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// the next phase relabels it
	if err := m.Begin("bootstrap", 0); err != nil {
		t.Fatal(err)
	}
	if phase, _, ok, _ := StartupInProgress(p, time.Minute, time.Now()); !ok || phase != "bootstrap" {
		t.Fatalf("after Begin(bootstrap): phase %q ok %v", phase, ok)
	}
	// End stops refreshing but keeps the file (the hand-over)
	m.End()
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	phase, age, ok, err := StartupInProgress(p, time.Minute, time.Now())
	if err != nil || ok || phase != "bootstrap" || age < 59*time.Minute {
		t.Fatalf("after End: phase %q age %s ok %v err %v", phase, age, ok, err)
	}
	if err := m.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("Remove left the marker: %v", err)
	}
	if err := m.Remove(); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

// A phase that runs past its limit stops refreshing: a hung restore must
// fail the liveness probe eventually.
func TestStartupMarkerMax(t *testing.T) {
	p := filepath.Join(t.TempDir(), StartupMarkerFile)
	m := NewStartupMarker(p, 10*time.Millisecond)
	if err := m.Begin("restore", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	defer m.End()
	time.Sleep(200 * time.Millisecond) // past the limit
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, _, ok, _ := StartupInProgress(p, time.Minute, time.Now()); ok {
		t.Fatal("marker still refreshed after the phase's limit")
	}
}

func TestStartupInProgressStale(t *testing.T) {
	p := filepath.Join(t.TempDir(), StartupMarkerFile)
	if err := os.WriteFile(p, []byte("phase=restore\npid=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, tc := range []struct {
		mtime time.Time
		ok    bool
	}{
		{now.Add(-5 * time.Second), true},
		{now.Add(-59 * time.Second), true},
		{now.Add(-61 * time.Second), false},
		{now.Add(-10 * time.Minute), false},
		{now.Add(10 * time.Minute), false}, // far future: a clock step, not fresh
	} {
		if err := os.Chtimes(p, tc.mtime, tc.mtime); err != nil {
			t.Fatal(err)
		}
		phase, _, ok, err := StartupInProgress(p, StartupMarkerStale, now)
		if err != nil || ok != tc.ok || phase != "restore" {
			t.Errorf("mtime %s: phase %q ok %v err %v, want ok %v", now.Sub(tc.mtime), phase, ok, err, tc.ok)
		}
	}
}

func TestStartupMarkerPath(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir(), DBPath: filepath.Join("x", "y", "hub.db")}
	if got, want := StartupMarkerPath(cfg), filepath.Join("x", "y", StartupMarkerFile); got != want {
		t.Fatalf("StartupMarkerPath = %q, want %q", got, want)
	}
}
