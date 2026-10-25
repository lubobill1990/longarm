//go:build windows

package e2e

import (
	"regexp"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// checkLocalACL verifies that the named pipe admits only the current user
// (and SYSTEM): a protected DACL with no other trustee.
func checkLocalACL(t *testing.T, pipe string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(pipe, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("security info of %s: %v", pipe, err)
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := tu.User.Sid.String()
	sddl := sd.String()
	t.Logf("%s: %s", pipe, sddl)
	if !strings.HasPrefix(sddl, "D:P") {
		t.Fatalf("DACL of %s is not protected: %s", pipe, sddl)
	}
	aces := regexp.MustCompile(`\(([^)]*)\)`).FindAllStringSubmatch(sddl, -1)
	if len(aces) == 0 {
		t.Fatalf("no ACEs: %s", sddl)
	}
	for _, a := range aces {
		f := strings.Split(a[1], ";")
		if len(f) < 6 || f[0] != "A" {
			t.Fatalf("unexpected ACE %s in %s", a[1], sddl)
		}
		// SDDL writes well-known accounts as aliases: a runner service
		// running as LocalService is "LS", not S-1-5-19.
		who := f[5]
		sid, err := windows.StringToSid(who)
		if err != nil {
			t.Fatalf("ACE trustee %q in %s: %v", who, sddl, err)
		}
		if !sid.Equals(tu.User.Sid) && who != "SY" {
			t.Fatalf("pipe %s admits %s (only %s and SYSTEM expected): %s", pipe, who, me, sddl)
		}
	}
}
