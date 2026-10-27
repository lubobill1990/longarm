package platform

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestPeerIsAccount(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	testPeerIsAccount(t, &Account{SID: u.User.Sid.String()}, &Account{SID: "S-1-5-18"})
}
