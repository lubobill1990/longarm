package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestRemoveUpdatesDir: the uninstall removes the update service's dir; a
// link planted in its place is removed itself, its target stays.
func TestRemoveUpdatesDir(t *testing.T) {
	base := t.TempDir()
	up := filepath.Join(base, "updates")
	os.MkdirAll(filepath.Join(up, "0.1.9"), 0o700)
	os.WriteFile(filepath.Join(up, "state.json"), []byte("{}"), 0o600)
	removeUpdatesDir(base, io.Discard)
	if _, err := os.Stat(up); !os.IsNotExist(err) {
		t.Fatalf("updates dir left: %v", err)
	}
	target := t.TempDir()
	os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o600)
	if err := os.Symlink(target, up); err != nil {
		t.Skipf("symlink: %v", err)
	}
	removeUpdatesDir(base, io.Discard)
	if _, err := os.Lstat(up); !os.IsNotExist(err) {
		t.Fatalf("link left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "keep")); err != nil {
		t.Fatalf("the link's target was emptied: %v", err)
	}
	removeUpdatesDir("", io.Discard) // no base: nothing
}
