//go:build windows

package longarmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/platform"
	win "github.com/lubobill1990/longarm/internal/platform/windows"
)

const lockedSDDL = "O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// TestEnsureSystemDataDirWindows: a pre-created ...\Longarm\longarmd with
// an inherited ACL is locked down (and so is what it holds); one owned by
// another account is refused; a locked one (per-user's traverse ACL) is
// kept. Needs an elevated test process (owner Administrators).
func TestEnsureSystemDataDirWindows(t *testing.T) {
	if !platform.Elevated() {
		t.Skip("needs an elevated (administrator) test process")
	}
	base := t.TempDir()
	parent := filepath.Join(base, "Longarm")
	sys := filepath.Join(parent, "longarmd")
	t.Setenv(SystemDataDirEnv, sys)

	// Pre-created with the temp dir's inherited ACL, holding a file.
	writeFiles(t, sys, map[string]string{"tsnet.authkey": "x"})
	if err := win.SecureTraverseDir(parent, "BU"); err != nil { // per-user's parent: kept
		t.Fatal(err)
	}
	if err := EnsureSystemDataDir(sys); err != nil {
		t.Fatal(err)
	}
	if !win.HasSDDL(sys, lockedSDDL) {
		sd, _ := win.PathSD(sys)
		t.Fatalf("%s not locked down: %v", sys, sd)
	}
	if !win.HasSDDL(parent, win.TraverseSDDL("BU")) {
		t.Errorf("the parent's traverse ACL was replaced")
	}
	sd, err := win.PathSD(filepath.Join(sys, "tsnet.authkey"))
	if err != nil {
		t.Fatal(err)
	}
	if locked, _ := win.LockedDACL(sd); locked {
		t.Errorf("the file kept a protected DACL")
	}
	if !strings.Contains(sd.String(), "(A;ID;FA;;;SY)") || strings.Contains(sd.String(), ";BU)") {
		t.Errorf("file ACL %s, want only the inherited SYSTEM/Administrators ACEs", sd)
	}

	// Per-user isolation's traverse ACL on the dir is kept as it is.
	if err := win.SecureTraverseDir(sys, "BU"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSystemDataDir(sys); err != nil || !win.HasSDDL(sys, win.TraverseSDDL("BU")) {
		t.Fatalf("traverse ACL: %v", err)
	}

	// Owned by somebody else (here: the Users group): refused.
	if err := win.SecureUserDir(sys, "S-1-5-32-545"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSystemDataDir(sys); err == nil || !strings.Contains(err.Error(), "not SYSTEM or Administrators") {
		t.Fatalf("foreign owner: %v", err)
	}
	// A file below owned by somebody else, in a dir that was open: refused.
	if err := platform.SecureDir(sys, ""); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(sys, "tls")
	if err := win.SecureUserDir(sub, "S-1-5-32-545"); err != nil {
		t.Fatal(err)
	}
	// the dir inherits its ACL again (as if pre-created in ProgramData)
	if err := win.ResetInherited(base, func(p string) bool { return p == sub }); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSystemDataDir(sys); err == nil || !strings.Contains(err.Error(), sub+" is owned by") {
		t.Fatalf("foreign-owned entry: %v", err)
	}
}

func TestMigrateEnrollmentWindowsACL(t *testing.T) {
	if !platform.Elevated() {
		t.Skip("needs an elevated (administrator) test process")
	}
	base := t.TempDir()
	src := filepath.Join(base, "home", ".longarm")
	sys := filepath.Join(base, "Longarm", "longarmd")
	t.Setenv(SystemDataDirEnv, sys)
	writeFiles(t, src, map[string]string{"tls/node.key": "KEY", "hub.toml": "address = \"h:7443\"\n"})
	if err := MigrateEnrollment(src, sys, nil); err != nil {
		t.Fatal(err)
	}
	if !win.HasSDDL(sys, lockedSDDL) {
		t.Fatalf("%s not locked down", sys)
	}
	for _, f := range []string{`tls\node.key`, "hub.toml", "tls"} {
		sd, err := win.PathSD(filepath.Join(sys, f))
		if err != nil {
			t.Fatal(err)
		}
		if s := sd.String(); strings.Contains(s, ";BU)") || strings.Contains(s, ";WD)") || strings.Contains(s, ";AU)") || !strings.Contains(s, "ID;FA;;;SY)") || strings.Contains(s, "D:P") {
			t.Errorf("%s: ACL %s", f, s)
		}
	}
}
