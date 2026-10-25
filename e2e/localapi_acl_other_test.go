//go:build !windows

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// checkLocalACL verifies the unix socket is 0600 in a private 0700 directory.
func checkLocalACL(t *testing.T, sock string) {
	t.Helper()
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket %s mode %v", sock, fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(sock))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode %v", di.Mode().Perm())
	}
}
