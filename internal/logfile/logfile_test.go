package logfile

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func stream(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

func TestRotationKeepsAbsoluteOffsets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.log")
	w, err := OpenWriter(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	all := stream(5500)
	rnd := rand.New(rand.NewSource(1))
	for p := all; len(p) > 0; {
		n := 1 + rnd.Intn(700)
		if n > len(p) {
			n = len(p)
		}
		if _, err := w.Write(p[:n]); err != nil {
			t.Fatal(err)
		}
		p = p[n:]
	}
	if w.End() != 5500 || Size(path) != 5500 {
		t.Fatalf("end %d size %d", w.End(), Size(path))
	}
	w.Close()
	for _, p := range []string{path, path + ".1"} {
		fi, err := os.Stat(p)
		if err != nil || fi.Size() > 1000 {
			t.Fatalf("%s: %v %d", p, err, fi.Size())
		}
	}
	// Current segment [5000,5500), previous [4000,5000).
	data, start, end, err := Read(path, 4500, 10000)
	if err != nil || start != 4500 || end != 5500 || !bytes.Equal(data, all[4500:5000]) {
		t.Fatalf("prev read: start %d end %d len %d err %v", start, end, len(data), err)
	}
	data, start, _, _ = Read(path, 5000, 10000)
	if start != 5000 || !bytes.Equal(data, all[5000:]) {
		t.Fatalf("cur read: start %d len %d", start, len(data))
	}
	data, start, _, _ = Read(path, 10, 100) // rotated away
	if start != 4000 || !bytes.Equal(data, all[4000:4100]) {
		t.Fatalf("old read: start %d len %d", start, len(data))
	}
	data, start, _, _ = Read(path, -7, 100)
	if start != 5493 || !bytes.Equal(data, all[5493:]) {
		t.Fatalf("tail read: start %d %q", start, data)
	}
	if data, start, _, _ = Read(path, 9999, 100); data != nil || start != 9999 {
		t.Fatalf("past end: %d %q", start, data)
	}
	// Reopening continues at the same absolute offset.
	w, _ = OpenWriter(path, 1000)
	if w.End() != 5500 {
		t.Fatalf("reopened end %d", w.End())
	}
	w.Close()
	if err := Remove(path); err != nil || Size(path) != 0 {
		t.Fatal(err)
	}
}

// A follower reading concurrently with a rotating writer sees each byte of
// the stream at its true offset (or skips ahead when it fell behind by more
// than a segment), never wrong bytes.
func TestConcurrentFollower(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.log")
	w, err := OpenWriter(path, 2048)
	if err != nil {
		t.Fatal(err)
	}
	all := stream(40000)
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		rnd := rand.New(rand.NewSource(2))
		for p := all; len(p) > 0; {
			n := 1 + rnd.Intn(900)
			if n > len(p) {
				n = len(p)
			}
			if _, err := w.Write(p[:n]); err != nil {
				t.Error(err)
				return
			}
			p = p[n:]
		}
	}()
	var off int64
	finished := false
	for !finished {
		select {
		case <-done:
			finished = true
		default:
		}
		for {
			data, start, _, err := Read(path, off, 3000)
			if err != nil {
				t.Fatal(err)
			}
			if len(data) == 0 {
				break
			}
			if !bytes.Equal(data, all[start:start+int64(len(data))]) {
				t.Fatalf("wrong bytes at %d", start)
			}
			off = start + int64(len(data))
		}
	}
	wg.Wait()
	w.Close()
	if off != int64(len(all)) {
		t.Fatalf("follower ended at %d", off)
	}
}
