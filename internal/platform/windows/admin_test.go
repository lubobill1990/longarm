//go:build windows

package windows

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestTrustedSystemPath: Program Files-like dirs (only SYSTEM,
// Administrators, TrustedInstaller can write) pass; a dir a user created
// (owned by them) does not - the check the installer makes before it hands
// a binary or a data dir to SYSTEM.
func TestTrustedSystemPath(t *testing.T) {
	sys := filepath.Join(os.Getenv("SystemRoot"), "System32")
	if err := TrustedSystemPath(sys); err != nil {
		t.Errorf("%s: %v", sys, err)
	}
	if err := TrustedSystemPath(t.TempDir()); err == nil {
		t.Error("a user's temp dir passed")
	}
	if err := TrustedSystemPath(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("a missing dir passed")
	}
}

func TestUserSIDAndAdmin(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := u.User.Sid.String()
	if err := UserSID(me); err != nil {
		t.Errorf("own SID %s: %v", me, err)
	}
	for _, bad := range []string{"S-1-5-32-544", "S-1-5-18", "S-1-5-21-1-2-3-999999", "x"} {
		if err := UserSID(bad); err == nil {
			t.Errorf("%s passed as a user", bad)
		}
	}
	if ok, err := IsLocalAdmin("S-1-5-18"); !ok || err != nil {
		t.Errorf("LocalSystem: %v %v", ok, err)
	}
	ok, err := IsLocalAdmin(me)
	t.Logf("this account is an administrator: %v (%v)", ok, err)
	if os.Getenv("GITHUB_ACTIONS") == "true" && (!ok || err != nil) {
		t.Errorf("the runner's account (an administrator): %v %v", ok, err)
	}
}
