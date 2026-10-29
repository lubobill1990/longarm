package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/hub"
)

func TestLiveCheck(t *testing.T) {
	down := errors.New("connection refused")
	dir := t.TempDir()
	marker := filepath.Join(dir, hub.StartupMarkerFile)
	now := time.Now()

	// /healthz answers: alive, marker or not
	if _, err := liveCheck(nil, marker, time.Minute, now); err != nil {
		t.Fatalf("healthz ok: %v", err)
	}
	// down, no marker: the hub is dead
	if _, err := liveCheck(down, marker, time.Minute, now); !errors.Is(err, down) {
		t.Fatalf("no marker: %v", err)
	}
	if err := os.WriteFile(marker, []byte("phase=restore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// down, fresh marker: still restoring
	msg, err := liveCheck(down, marker, time.Minute, now)
	if err != nil || !strings.Contains(msg, "restore") {
		t.Fatalf("fresh marker: msg %q err %v", msg, err)
	}
	// down, stale marker: the restore died or hangs
	old := now.Add(-2 * time.Minute)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := liveCheck(down, marker, time.Minute, now); !errors.Is(err, down) || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("stale marker: %v", err)
	}
}

func TestHealthcheckCmd(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	marker := filepath.Join(t.TempDir(), hub.StartupMarkerFile)

	run := func(args ...string) error {
		c := healthcheckCmd()
		c.SetArgs(args)
		c.SilenceUsage, c.SilenceErrors = true, true
		return c.Execute()
	}
	if err := run("--url", ok.URL); err != nil {
		t.Fatalf("readiness, healthz ok: %v", err)
	}
	if err := run("--url", bad.URL); err == nil {
		t.Fatal("readiness, healthz 503: passed")
	}
	if err := run("--url", bad.URL, "--live", "--marker", marker); err == nil {
		t.Fatal("liveness, healthz 503, no marker: passed")
	}
	if err := os.WriteFile(marker, []byte("phase=restore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run("--url", bad.URL, "--live", "--marker", marker); err != nil {
		t.Fatalf("liveness, restoring: %v", err)
	}
	// readiness ignores the marker: a restoring hub is not ready
	if err := run("--url", bad.URL, "--marker", marker); err == nil {
		t.Fatal("readiness, restoring: passed")
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	if err := run("--url", bad.URL, "--live", "--marker", marker); err == nil {
		t.Fatal("liveness, stale marker: passed")
	}
}

func TestHealthURL(t *testing.T) {
	for in, want := range map[string]string{
		":7480":          "http://127.0.0.1:7480/healthz",
		"0.0.0.0:7480":   "http://127.0.0.1:7480/healthz",
		"127.0.0.1:7480": "http://127.0.0.1:7480/healthz",
	} {
		if got := healthURL(in); got != want {
			t.Errorf("healthURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStartupTimeout(t *testing.T) {
	t.Setenv(envStartupTimeout, "")
	if d, err := startupTimeout(); err != nil || d != time.Hour {
		t.Fatalf("default: %s %v", d, err)
	}
	t.Setenv(envStartupTimeout, "90m")
	if d, err := startupTimeout(); err != nil || d != 90*time.Minute {
		t.Fatalf("90m: %s %v", d, err)
	}
	for _, v := range []string{"soon", "0s", "-1m"} {
		t.Setenv(envStartupTimeout, v)
		if _, err := startupTimeout(); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

// The test binary is the fake litestream of TestRunRestore.
func TestMain(m *testing.M) {
	if v := os.Getenv("LONGARM_TEST_FAKE_LITESTREAM"); v != "" {
		time.Sleep(300 * time.Millisecond)
		if v == "fail" {
			os.Exit(3)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRunRestore(t *testing.T) {
	restoreProgressEvery = 50 * time.Millisecond
	defer func() { restoreProgressEvery = time.Minute }()
	dir := t.TempDir()
	mk := hub.NewStartupMarker(filepath.Join(dir, hub.StartupMarkerFile), 20*time.Millisecond)
	defer mk.Remove()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LONGARM_TEST_FAKE_LITESTREAM", "ok")
	done := make(chan error, 1)
	go func() {
		done <- runRestore(t.Context(), mk, "restore", time.Minute, self, "cfg.yml", filepath.Join(dir, "hub.db"))
	}()
	time.Sleep(100 * time.Millisecond)
	if phase, _, ok, err := hub.StartupInProgress(mk.Path(), time.Minute, time.Now()); !ok || phase != "restore" || err != nil {
		t.Fatalf("during the restore: phase %q ok %v err %v", phase, ok, err)
	}
	if err := <-done; err != nil {
		t.Fatalf("restore: %v", err)
	}
	t.Setenv("LONGARM_TEST_FAKE_LITESTREAM", "fail")
	if err := runRestore(t.Context(), mk, "restore", time.Minute, self, "cfg.yml", filepath.Join(dir, "hub.db")); err == nil {
		t.Fatal("a failed restore succeeded")
	}
}
