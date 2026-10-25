//go:build windows

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
)

// perUserBase is a fresh dir outside any user profile (the per-user
// accounts run binaries from it; Users may read C:\ subdirectories).
func perUserBase(t *testing.T) string {
	t.Helper()
	base := filepath.Join(`C:\longarm-e2e`, "peruser-"+puRand(3))
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	return base
}

// startPerUserWorker installs and starts longarmd as a temporary LocalSystem
// service (S4U logons need SeTcbPrivilege); the returned stop uninstalls it.
func startPerUserWorker(t *testing.T, w *puWorker, exe string) func() {
	t.Helper()
	name := "longarmd-putest-" + puRand(3)
	w.logPath = filepath.Join(w.dataDir, "longarmd.log")
	cfg := platform.ServiceConfig{Name: name, DisplayName: "Longarm per-user test (" + name + ")",
		Description: "temporary: longarm e2e test", Exe: exe,
		Args: []string{"run", "--data-dir", w.dataDir, "--service-name", name}}
	if err := platform.InstallService(cfg); err != nil {
		t.Fatalf("install service: %v", err)
	}
	stop := func() {
		platform.StopService(name)
		if err := platform.UninstallService(name); err != nil {
			t.Logf("uninstall service %s: %v", name, err)
		}
		// jobs live in shims (LocalSystem); end them with the worker
		killShims(w.dataDir)
		users, _ := os.ReadDir(filepath.Join(w.dataDir, "users"))
		for _, u := range users {
			killShims(filepath.Join(w.dataDir, "users", u.Name()))
		}
		time.Sleep(500 * time.Millisecond)
		if t.Failed() {
			b, _ := os.ReadFile(w.logPath)
			t.Logf("per-user longarmd log:\n%s", tail(string(b), 20000))
		}
	}
	if err := platform.StartService(name); err != nil {
		stop()
		b, _ := os.ReadFile(w.logPath)
		t.Fatalf("start service: %v\n%s", err, b)
	}
	return stop
}
