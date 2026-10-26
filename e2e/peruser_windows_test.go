//go:build windows

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
	win "github.com/lubobill1990/longarm/internal/platform/windows"
)

// TestPerUserServiceDataDirWindows: `longarmd service install --isolation
// per-user` without --data-dir uses the system data dir (C:\ProgramData\
// Longarm\longarmd; here a temp dir via LONGARM_TEST_SYSTEM_DATA_DIR),
// locked down to SYSTEM and Administrators, and copies the enrollment that
// an elevated `longarmd join` left in the admin's ~\.longarm. Shared mode
// with --user keeps ~\.longarm. Opt-in (LONGARM_PERUSER_TEST=1, elevated):
// it installs a temporary service (not started) and removes it.
func TestPerUserServiceDataDirWindows(t *testing.T) {
	requirePerUser(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	legacy := filepath.Join(home, ".longarm")
	sys := filepath.Join(base, "sys", "longarmd")
	if err := os.MkdirAll(filepath.Join(legacy, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	for f, s := range map[string]string{`tls\ca.crt`: "CA", `tls\pending.key`: "KEY", "hub.toml": "address = \"hub.example.com:7443\"\n"} {
		if err := os.WriteFile(filepath.Join(legacy, f), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) result {
		t.Helper()
		cmd := exec.Command(bins.longarmd, args...)
		for _, kv := range os.Environ() {
			k := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
			if k != "LONGARM_DATA_DIR" && k != "USERPROFILE" && k != "LONGARM_TEST_SYSTEM_DATA_DIR" {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, "USERPROFILE="+home, "LONGARM_TEST_SYSTEM_DATA_DIR="+sys)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("longarmd %v: %v", args, err)
		}
		return result{out.String(), errb.String(), code}
	}
	name := "longarmd-ddtest-" + puRand(3)
	if r := run("service", "install", "--dry-run", "--name", name, "--user", `.\nobody`); r.code != 0 || !strings.Contains(r.stdout, "data dir "+legacy+"\n") {
		t.Fatalf("shared --user --dry-run: %+v", r)
	}
	r := run("service", "install", "--isolation", "per-user", "--dry-run", "--name", name)
	if r.code != 0 || !strings.Contains(r.stdout, "data dir "+sys+"\n") || !strings.Contains(r.stdout, "would copy the enrollment from "+legacy+" to "+sys) {
		t.Fatalf("per-user --dry-run: %+v", r)
	}
	if _, err := os.Stat(sys); err == nil {
		t.Fatalf("--dry-run created %s", sys)
	}
	t.Cleanup(func() { run("service", "uninstall", "--name", name) })
	r = run("service", "install", "--isolation", "per-user", "--name", name)
	if r.code != 0 || !strings.Contains(r.stdout, "copying the enrollment from "+legacy) {
		t.Fatalf("per-user install: %+v", r)
	}
	for f, s := range map[string]string{`tls\ca.crt`: "CA", `tls\pending.key`: "KEY"} {
		if b, err := os.ReadFile(filepath.Join(sys, f)); err != nil || string(b) != s {
			t.Errorf("%s: %q %v", f, b, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(sys, "longarmd.toml")); err != nil || !strings.Contains(string(b), `mode = "per-user"`) {
		t.Errorf("longarmd.toml: %q %v", b, err)
	}
	if !win.HasSDDL(sys, "O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)") {
		t.Errorf("%s is not locked down to SYSTEM and Administrators", sys)
	}
	// Both in use: the elevated admin's own ~\.longarm stays the default.
	if r := run("service", "install", "--dry-run", "--name", name); r.code != 0 || !strings.Contains(r.stdout, "data dir "+legacy+"\n") {
		t.Errorf("shared --dry-run with both in use: %+v", r)
	}
}

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
