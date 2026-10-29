package update

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRenameRetrySharingViolation: a file another process holds open
// without FILE_SHARE_DELETE (an antivirus scan of a fresh binary) can't be
// renamed; the update waits for it instead of failing ("The process cannot
// access the file because it is being used by another process").
func TestRenameRetrySharingViolation(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "longarmd.exe"), filepath.Join(dir, "longarmd.exe.old")
	os.WriteFile(src, []byte("x"), 0o755)
	p, _ := syscall.UTF16PtrFromString(src)
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, dst); err == nil || !transientRenameErr(err) {
		t.Fatalf("rename of a locked file: %v", err)
	}
	go func() { time.Sleep(700 * time.Millisecond); syscall.CloseHandle(h) }()
	if err := renameRetry(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	}
}
