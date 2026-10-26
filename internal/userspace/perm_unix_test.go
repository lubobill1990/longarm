//go:build !windows

package userspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// traversableBase is a fresh dir below /tmp the checks can pass for (go
// test's temp dirs are 0700).
func traversableBase(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "longarm-reach-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkTraversable(filepath.Join(base, "x")); err != nil {
		t.Skipf("/tmp's parents are not traversable here: %v", err)
	}
	return base
}

func TestCheckReachable(t *testing.T) {
	base := traversableBase(t)
	home := filepath.Join(base, "root")
	data := filepath.Join(home, ".longarm")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(home, 0o700)

	// A data dir below a 0700 home (like /root/.longarm): refused, naming
	// the blocking dir and the fixes.
	err := CheckReachable(filepath.Join(data, UsersDir), data)
	if err == nil || !strings.Contains(err.Error(), home+" is not traversable") ||
		!strings.Contains(err.Error(), "/var/lib/longarmd") || !strings.Contains(err.Error(), "users_dir") {
		t.Fatalf("below a 0700 home: %v", err)
	}
	// The data dir itself may be 0700 (longarmd makes it 0711), and the
	// users dir need not exist yet.
	os.Chmod(home, 0o711)
	if err := CheckReachable(filepath.Join(data, UsersDir), data); err != nil {
		t.Fatalf("o+x home: %v", err)
	}
	// Parents that do not exist yet are created by longarmd: fine.
	if err := CheckReachable(filepath.Join(base, "sys", "longarmd", UsersDir), filepath.Join(base, "sys", "longarmd")); err != nil {
		t.Fatalf("missing parents: %v", err)
	}
	// users_dir outside the data dir: its parents count, the data dir not.
	os.Chmod(home, 0o700)
	if err := CheckReachable(filepath.Join(base, "users"), data); err != nil {
		t.Fatalf("users_dir elsewhere: %v", err)
	}
	if err := CheckReachable(filepath.Join(home, "users"), filepath.Join(base, "other")); err == nil {
		t.Fatal("users_dir below a 0700 dir accepted")
	}
	// checkTraversable (longarmd's start) still wants everything to exist.
	if err := checkTraversable(filepath.Join(base, "nope", "users")); err == nil {
		t.Fatal("checkTraversable accepted a missing parent")
	}
}
