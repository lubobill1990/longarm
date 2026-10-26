//go:build !windows

package longarmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureSystemDataDirUnix: the system data dir is created 0700; an
// existing one must be a root-owned (here: our) plain dir nobody else may
// write.
func TestEnsureSystemDataDirUnix(t *testing.T) {
	base := t.TempDir()
	sys := filepath.Join(base, "longarmd")
	t.Setenv(SystemDataDirEnv, sys)
	if err := EnsureSystemDataDir(sys); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(sys); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("created: %v %v", fi, err)
	}
	// Per-user isolation's 0711 is fine.
	os.Chmod(sys, 0o711)
	if err := EnsureSystemDataDir(sys); err != nil {
		t.Fatalf("0711: %v", err)
	}
	os.Chmod(sys, 0o777)
	if err := EnsureSystemDataDir(sys); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("world-writable: %v", err)
	}
	os.Chmod(sys, 0o700)
	if os.Geteuid() == 0 {
		if err := os.Chown(sys, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		if err := EnsureSystemDataDir(sys); err == nil || !strings.Contains(err.Error(), "owned by uid 65534") {
			t.Fatalf("owned by nobody: %v", err)
		}
		os.Chown(sys, 0, 0)
	}
	os.Remove(sys)
	if err := os.Symlink(base, sys); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSystemDataDir(sys); err == nil || !strings.Contains(err.Error(), "not a plain directory") {
		t.Fatalf("symlink: %v", err)
	}
	// Other dirs are not checked.
	if err := EnsureSystemDataDir(base); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateEnrollmentForeignOwner: as root, a source entry owned by
// another account is refused.
func TestMigrateEnrollmentForeignOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (chown)")
	}
	base := t.TempDir()
	src, dst := filepath.Join(base, "old"), filepath.Join(base, "new")
	writeFiles(t, src, map[string]string{"tls/ca.crt": "CA", "hub.toml": "x"})
	if err := os.Chown(filepath.Join(src, "hub.toml"), 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if err := MigrateEnrollment(src, dst, nil); err == nil || !strings.Contains(err.Error(), "owned by uid 65534") {
		t.Fatalf("foreign-owned hub.toml: %v", err)
	}
	if DataDirInUse(dst) {
		t.Fatal("half enrollment left")
	}
	os.Chown(filepath.Join(src, "hub.toml"), 0, 0)
	if err := os.Chown(src, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if err := MigrateEnrollment(src, dst, nil); err == nil || !strings.Contains(err.Error(), "owned by uid 65534") {
		t.Fatalf("foreign-owned source dir: %v", err)
	}
}

// TestRootHomeIgnoresHOME: as root, the default (and migration source) is
// below root's passwd home, whatever $HOME says (sudo -E).
func TestRootHomeIgnoresHOME(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	t.Setenv(SystemDataDirEnv, "")
	os.Unsetenv(SystemDataDirEnv)
	t.Setenv("HOME", t.TempDir())
	e := currentDataDirEnv()
	if e.home == os.Getenv("HOME") || e.home == "" {
		t.Fatalf("root's home %q follows $HOME", e.home)
	}
}
