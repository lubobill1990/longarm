package longarmd

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fakeEnv(env map[string]string, home string, inUse ...string) dataDirEnv {
	used := map[string]bool{}
	for _, d := range inUse {
		used[d] = true
	}
	return dataDirEnv{
		getenv: func(k string) string { return env[k] },
		home:   home,
		inUse:  func(d string) bool { return used[d] },
	}
}

func TestDefaultDataDir(t *testing.T) {
	home := filepath.Join("h", "root")
	legacy := filepath.Join(home, ".longarm")
	sys := LinuxSystemDataDir
	for _, tc := range []struct {
		name string
		e    dataDirEnv
		want string
	}{
		{"env wins", func() dataDirEnv {
			e := fakeEnv(map[string]string{DataDirEnv: "/x"}, home)
			e.system, e.sysDir = true, sys
			return e
		}(), "/x"},
		{"normal account", func() dataDirEnv { e := fakeEnv(nil, home); e.sysDir = sys; return e }(), legacy},
		{"normal account, system dir in use", func() dataDirEnv { e := fakeEnv(nil, home, sys); e.sysDir = sys; return e }(), legacy},
		{"root, fresh", func() dataDirEnv { e := fakeEnv(nil, home); e.system, e.sysDir = true, sys; return e }(), sys},
		{"root, legacy home enrollment", func() dataDirEnv {
			e := fakeEnv(nil, home, legacy)
			e.system, e.sysDir = true, sys
			return e
		}(), legacy},
		{"root, both in use", func() dataDirEnv {
			e := fakeEnv(nil, home, legacy, sys)
			e.system, e.sysDir = true, sys
			return e
		}(), sys},
		{"root, no home", func() dataDirEnv { e := fakeEnv(nil, ""); e.system, e.sysDir = true, sys; return e }(), sys},
		{"root on macOS (no system dir)", func() dataDirEnv { e := fakeEnv(nil, home); e.system = true; return e }(), legacy},
		{"no home, no system dir", fakeEnv(nil, ""), ".longarm"},
		{"windows elevated, only system dir in use", func() dataDirEnv {
			e := fakeEnv(nil, home, sys)
			e.elevated, e.sysDir = true, sys
			return e
		}(), sys},
		{"windows elevated, home in use", func() dataDirEnv {
			e := fakeEnv(nil, home, sys, legacy)
			e.elevated, e.sysDir = true, sys
			return e
		}(), legacy},
		{"windows elevated, fresh", func() dataDirEnv { e := fakeEnv(nil, home); e.elevated, e.sysDir = true, sys; return e }(), legacy},
	} {
		if got := defaultDataDir(tc.e); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSystemDataDirFor(t *testing.T) {
	none := func(string) string { return "" }
	if got := systemDataDirFor("linux", none, ""); got != "/var/lib/longarmd" {
		t.Errorf("linux: %q", got)
	}
	if got := systemDataDirFor("darwin", none, ""); got != "" {
		t.Errorf("darwin: %q", got)
	}
	if got, want := systemDataDirFor("windows", none, `D:\PD`), filepath.Join(`D:\PD`, "Longarm", "longarmd"); got != want {
		t.Errorf("windows: %q, want %q", got, want)
	}
	if got, want := systemDataDirFor("windows", none, ""), filepath.Join(`C:\ProgramData`, "Longarm", "longarmd"); got != want {
		t.Errorf("windows without ProgramData: %q, want %q", got, want)
	}
	env := func(k string) string {
		if k == SystemDataDirEnv {
			return "/t/sys"
		}
		return ""
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if got := systemDataDirFor(goos, env, ""); got != "/t/sys" {
			t.Errorf("%s with %s: %q", goos, SystemDataDirEnv, got)
		}
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolveServiceDataDir(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "root")
	legacy := filepath.Join(home, ".longarm")
	sys := filepath.Join(base, "sys")
	env := dataDirEnv{getenv: func(string) string { return "" }, home: home, system: true, sysDir: sys, inUse: DataDirInUse}
	noHome := func(string) (string, error) { return "", errors.New("no such user") }
	ok := func(string, string) error { return nil }
	var checked [2]string
	record := func(u, d string) error { checked = [2]string{u, d}; return nil }

	// Per-user, default dir: the system dir; nothing to migrate yet.
	sd, err := resolveServiceDataDir(ServiceDirRequest{DataDir: sys, PerUser: true}, env, "linux", noHome, record)
	if err != nil || sd.Dir != sys || sd.MigrateFrom != "" || sd.UsersDir != filepath.Join(sys, "users") {
		t.Fatalf("fresh per-user: %+v %v", sd, err)
	}
	if checked != [2]string{filepath.Join(sys, "users"), sys} {
		t.Fatalf("reachability checked for %v", checked)
	}

	// The legacy root dir holds an enrollment (DefaultDataDir chose it):
	// per-user moves to the system dir and migrates; its users_dir counts.
	writeFiles(t, legacy, map[string]string{"tls/ca.crt": "ca", "longarmd.toml": "[isolation]\nusers_dir = \"u\"\n"})
	sd, err = resolveServiceDataDir(ServiceDirRequest{DataDir: legacy, PerUser: true}, env, "linux", noHome, ok)
	if err != nil || sd.Dir != sys || sd.MigrateFrom != legacy || sd.UsersDir != filepath.Join(sys, "u") {
		t.Fatalf("legacy per-user: %+v %v", sd, err)
	}
	// ... also when the default already was the system dir (~/.longarm found).
	sd, err = resolveServiceDataDir(ServiceDirRequest{DataDir: sys, PerUser: true}, env, "linux", noHome, ok)
	if err != nil || sd.MigrateFrom != legacy {
		t.Fatalf("per-user, enrollment in ~/.longarm: %+v %v", sd, err)
	}
	// Once the system dir is in use, nothing is migrated.
	writeFiles(t, sys, map[string]string{"hub.toml": "address = \"hub.example.com:7443\"\n"})
	sd, err = resolveServiceDataDir(ServiceDirRequest{DataDir: sys, PerUser: true}, env, "linux", noHome, ok)
	if err != nil || sd.Dir != sys || sd.MigrateFrom != "" {
		t.Fatalf("per-user, system dir in use: %+v %v", sd, err)
	}

	// An explicit dir is kept; an unreachable users dir is refused with a hint.
	unreachable := func(u, d string) error { return errors.New("per-user isolation: the accounts cannot reach " + u) }
	_, err = resolveServiceDataDir(ServiceDirRequest{DataDir: legacy, Explicit: true, PerUser: true}, env, "linux", noHome, unreachable)
	if err == nil || !strings.Contains(err.Error(), "cannot reach") || !strings.Contains(err.Error(), "drop --data-dir") || !strings.Contains(err.Error(), sys) {
		t.Fatalf("explicit unreachable: %v", err)
	}
	_, err = resolveServiceDataDir(ServiceDirRequest{DataDir: sys, PerUser: true}, env, "linux", noHome, unreachable)
	if err == nil || strings.Contains(err.Error(), "drop --data-dir") {
		t.Fatalf("default unreachable: %v", err)
	}

	// Shared mode as root with --user: that account's ~/.longarm.
	alice := filepath.Join(base, "alice")
	aliceHome := func(n string) (string, error) {
		if n != "alice" {
			return "", errors.New("unknown " + n)
		}
		return alice, nil
	}
	_, err = resolveServiceDataDir(ServiceDirRequest{DataDir: sys, User: "alice"}, env, "linux", aliceHome, ok)
	if err == nil || !strings.Contains(err.Error(), "sudo -u alice longarmd join") {
		t.Fatalf("--user alice with root's enrollment only: %v", err)
	}
	writeFiles(t, filepath.Join(alice, ".longarm"), map[string]string{"tls/ca.crt": "ca"})
	sd, err = resolveServiceDataDir(ServiceDirRequest{DataDir: sys, User: "alice"}, env, "darwin", aliceHome, ok)
	if err != nil || sd.Dir != filepath.Join(alice, ".longarm") {
		t.Fatalf("--user alice: %+v %v", sd, err)
	}
	if _, err := resolveServiceDataDir(ServiceDirRequest{DataDir: sys, User: "bob"}, env, "linux", aliceHome, ok); err == nil || !strings.Contains(err.Error(), "--data-dir") {
		t.Fatalf("--user bob (unknown): %v", err)
	}
	// ... but not with --data-dir, not on Windows, not for non-root and not
	// for --user root (root's own default).
	for _, tc := range []struct {
		r    ServiceDirRequest
		goos string
		root bool
	}{
		{ServiceDirRequest{DataDir: sys, User: "alice", Explicit: true}, "linux", true},
		{ServiceDirRequest{DataDir: sys, User: `.\alice`}, "windows", true},
		{ServiceDirRequest{DataDir: sys, User: "alice"}, "linux", false},
		{ServiceDirRequest{DataDir: sys}, "linux", true},
		{ServiceDirRequest{DataDir: sys, User: "root"}, "linux", true},
	} {
		e := env
		e.system = tc.root
		sd, err := resolveServiceDataDir(tc.r, e, tc.goos, aliceHome, ok)
		if err != nil || sd.Dir != sys {
			t.Errorf("%+v on %s (root %v): %+v %v", tc.r, tc.goos, tc.root, sd, err)
		}
	}
}

func TestMigrateEnrollment(t *testing.T) {
	base := t.TempDir()
	src, dst := filepath.Join(base, "old"), filepath.Join(base, "new")
	writeFiles(t, src, map[string]string{
		"tls/ca.crt": "CA", "tls/node.crt": "CERT", "tls/node.key": "KEY",
		"hub.toml": "address = \"hub.example.com:7443\"\n", "longarmd.toml": "listen = []\n",
		"tsnet.authkey": "tskey", "tsnet-ctl.example.com/state": "S",
		"jobs/j1/meta.json": "{}", "longarmd.log": "log",
	})
	if runtime.GOOS != "windows" {
		os.Chmod(filepath.Join(src, "tls", "ca.crt"), 0o644)
		if err := os.Symlink("/etc/passwd", filepath.Join(src, "authorized_keys")); err != nil {
			t.Fatal(err)
		}
		// A file others may write: refused, and nothing is left in dst.
		os.Chmod(filepath.Join(src, "tls", "node.crt"), 0o666)
		err := MigrateEnrollment(src, dst, nil)
		if err == nil || !strings.Contains(err.Error(), "writable by group or others") {
			t.Fatalf("group/world-writable source file: %v", err)
		}
		if DataDirInUse(dst) {
			t.Fatal("a failed migration left a half enrollment")
		}
		if m, _ := filepath.Glob(filepath.Join(dst, migrateTmpPrefix+"*")); len(m) > 0 {
			t.Fatalf("staging dir left: %v", m)
		}
		os.Chmod(filepath.Join(src, "tls", "node.crt"), 0o644)
		// So is a source dir others may write.
		os.Chmod(src, 0o777)
		if err := MigrateEnrollment(src, dst, nil); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
			t.Fatalf("world-writable source dir: %v", err)
		}
		os.Chmod(src, 0o700)
	}
	// A leftover staging dir of an interrupted run is cleaned up.
	if err := os.MkdirAll(filepath.Join(dst, migrateTmpPrefix+"old", "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := MigrateEnrollment(src, dst, &out); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(dst, migrateTmpPrefix+"*")); len(m) > 0 {
		t.Errorf("staging dirs left: %v", m)
	}
	for name, want := range map[string]string{
		"tls/ca.crt": "CA", "tls/node.key": "KEY", "tls/node.crt": "CERT", "hub.toml": "address = \"hub.example.com:7443\"\n",
		"longarmd.toml": "listen = []\n", "tsnet.authkey": "tskey", "tsnet-ctl.example.com/state": "S",
	} {
		b, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(name)))
		if err != nil || string(b) != want {
			t.Errorf("%s: %q %v", name, b, err)
		}
	}
	for _, name := range []string{"jobs", "longarmd.log", "authorized_keys"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); err == nil {
			t.Errorf("%s copied", name)
		}
	}
	if _, err := os.Stat(filepath.Join(src, "tls", "node.key")); err != nil {
		t.Errorf("source changed: %v", err)
	}
	if !strings.Contains(out.String(), "copied "+filepath.Join(src, "tls")) {
		t.Errorf("output: %s", out.String())
	}
	if runtime.GOOS != "windows" {
		if !strings.Contains(out.String(), "skipped "+filepath.Join(src, "authorized_keys")+" (a symlink)") {
			t.Errorf("output: %s", out.String())
		}
		for name, want := range map[string]os.FileMode{"tls": 0o700, "tls/node.key": 0o600, "tls/ca.crt": 0o600, "hub.toml": 0o600,
			"tsnet-ctl.example.com": 0o700, "tsnet-ctl.example.com/state": 0o600, "tsnet.authkey": 0o600} {
			fi, err := os.Stat(filepath.Join(dst, filepath.FromSlash(name)))
			if err != nil {
				t.Error(err)
			} else if fi.Mode().Perm() != want {
				t.Errorf("%s: mode %v, want %v", name, fi.Mode().Perm(), want)
			}
		}
	}
	// Never over an existing enrollment.
	if err := MigrateEnrollment(src, dst, nil); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("second migration: %v", err)
	}
	// Nothing to copy.
	empty := filepath.Join(base, "empty")
	os.Mkdir(empty, 0o700)
	if err := MigrateEnrollment(empty, filepath.Join(base, "new2"), nil); err == nil {
		t.Error("migration of an empty dir succeeded")
	}
}

func TestDataDirInUse(t *testing.T) {
	d := t.TempDir()
	if DataDirInUse(d) || DataDirInUse(filepath.Join(d, "missing")) {
		t.Fatal("empty dir in use")
	}
	writeFiles(t, d, map[string]string{"jobs/x": ""})
	if DataDirInUse(d) {
		t.Fatal("jobs only: in use")
	}
	for _, n := range []string{"tls/ca.crt", "hub.toml", "longarmd.toml"} {
		d := t.TempDir()
		writeFiles(t, d, map[string]string{n: "x"})
		if !DataDirInUse(d) {
			t.Errorf("%s: not in use", n)
		}
	}
}
