package localapi

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReleaseKeepsAnotherProcesssFile: a daemon that exits removes the
// address file only while it is still its own. The channel's name is the
// same for every daemon of a config dir, so when a new daemon took over
// (stop, then start at once) the old one must leave the new one's file:
// it used to delete it, and the running daemon was "not running".
func TestReleaseKeepsAnotherProcesssFile(t *testing.T) {
	dir := t.TempDir()
	addrFile := filepath.Join(dir, "daemon.addr")
	old, err := ListenChannel("latest", dir, addrFile)
	if err != nil {
		t.Fatal(err)
	}
	old.Close() // the old daemon stops serving...
	cur, err := ListenChannel("latest", dir, addrFile)
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	old.Release() // ...and exits after the new one wrote its file
	if a, err := ReadAddr(addrFile); err != nil || a != cur.Addr {
		t.Fatalf("the new daemon's address file: %q, %v", a, err)
	}
	// rewritten in place (no rename): not the same file content-wise either
	if fi, _ := os.Stat(addrFile); !OwnsAddrFile(addrFile, cur.Addr, fi) || OwnsAddrFile(addrFile, cur.Addr, nil) {
		t.Fatal("OwnsAddrFile")
	}
	cur.Release()
	if _, err := os.Stat(addrFile); !os.IsNotExist(err) {
		t.Fatalf("its own file stays: %v", err)
	}
	cur.Release() // idempotent
}

// TestReleaseSameTimestamp: the new daemon's file has the very modification
// time of the old one's (both written within one timestamp tick) and the
// same content (the same channel): only the file's identity, pinned when
// the old one was written, tells them apart.
func TestReleaseSameTimestamp(t *testing.T) {
	dir := t.TempDir()
	addrFile := filepath.Join(dir, "daemon.addr")
	old, err := ListenChannel("latest", dir, addrFile)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	cur, err := ListenChannel("latest", dir, addrFile)
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	if err := os.Chtimes(addrFile, old.written.ModTime(), old.written.ModTime()); err != nil {
		t.Fatal(err)
	}
	old.Release()
	if a, err := ReadAddr(addrFile); err != nil || a != cur.Addr {
		t.Fatalf("the new daemon's address file: %q, %v", a, err)
	}
}
