package platform

import (
	"os"
	"testing"
)

func TestPeerIsAccount(t *testing.T) {
	testPeerIsAccount(t, &Account{UID: os.Getuid()}, &Account{UID: os.Getuid() + 4242})
}
