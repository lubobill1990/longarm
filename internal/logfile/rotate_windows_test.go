package logfile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A reader that opened out.log without delete sharing makes rotation fail;
// the writer must keep working and rotate later.
func TestRotationFailureKeepsWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.log")
	w, err := OpenWriter(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var rotErr error
	w.OnRotateError = func(err error) { rotErr = err }
	holder, err := os.Open(path) // Go's os.Open does not share delete on Windows
	if err != nil {
		t.Fatal(err)
	}
	all := stream(250)
	if n, err := w.Write(all); err != nil || n != 250 {
		t.Fatalf("write during failed rotation: %d %v", n, err)
	}
	if rotErr == nil {
		t.Fatal("rotation unexpectedly succeeded while the file was held")
	}
	if fi, _ := os.Stat(path); fi.Size() != 250 {
		t.Fatalf("segment size %d, want 250 (kept appending)", fi.Size())
	}
	holder.Close()
	w.retryAt = time.Time{}
	w.Write([]byte("x"))
	data, start, end, err := Read(path, 0, 1000) // previous segment
	rest, _, _, _ := Read(path, int64(len(data)), 1000)
	if err != nil || start != 0 || end != 251 || !bytes.Equal(append(data, rest...), append(all, 'x')) {
		t.Fatalf("after retry: start %d end %d len %d %v", start, end, len(data), err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatal("no rotation after the holder went away")
	}
}
