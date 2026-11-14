//go:build windows

package windows

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestSecureUserDir: a new and a pre-created dir get the descriptor (owner:
// this account, so no elevation needed); a junction in place of the dir is
// refused and its target left alone.
func TestSecureUserDir(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := u.User.Sid.String()
	want := "O:" + sid + "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + sid + ")"
	base := t.TempDir()

	fresh := filepath.Join(base, "a", "b")
	if err := SecureUserDir(fresh, sid); err != nil {
		t.Fatal(err)
	}
	if !HasSDDL(fresh, want) {
		sd, _ := PathSD(fresh)
		t.Errorf("new dir: %s, want %s", sd, want)
	}
	old := filepath.Join(base, "old")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SecureUserDir(old, sid); err != nil {
		t.Fatal(err)
	}
	if !HasSDDL(old, want) {
		t.Errorf("existing dir: not %s", want)
	}
	if err := SecureUserDir(old, sid); err != nil { // already: kept
		t.Fatal(err)
	}
	// set through the handle, the DACL still reaches what is below
	if err := secureDirSDDL(old, "", "O:"+sid+"D:PAI(A;OICI;FA;;;"+sid+")(A;OICI;FR;;;WD)"); err != nil {
		t.Fatal(err)
	}
	if sd, err := PathSD(filepath.Join(old, "f")); err != nil || !strings.Contains(sd.String(), "(A;ID;FR;;;WD)") {
		t.Errorf("not propagated: %v %v", sd, err)
	}

	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SecureUserDir(file, sid); err == nil {
		t.Error("a file accepted")
	}

	target := t.TempDir()
	link := filepath.Join(base, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	if err := SecureUserDir(link, sid); err == nil {
		t.Fatal("junction accepted")
	}
	if HasSDDL(target, want) {
		t.Error("the junction's target got the descriptor")
	}
}
