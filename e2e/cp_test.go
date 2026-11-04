package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkerCopy: longarm cp copies a directory tree from w1 to w2, worker
// to worker (docs/usage.md "Copy between workers"): into an existing
// directory by its name, files with their contents and empty directories;
// a partial file on w2 is resumed, a second copy finds everything up to
// date; one file goes to an exact path; with one side on this machine it is
// put / get.
func TestWorkerCopy(t *testing.T) {
	h := newHandoffEnv(t)
	src := filepath.Join(h.w1home, "out")
	big := bytes.Repeat([]byte("0123456789abcdef"), 192<<10) // 3 MiB
	files := map[string][]byte{"a.txt": []byte("hello"), "deep/b/big.bin": big, "deep/c.txt": []byte("c")}
	for rel, b := range files {
		p := filepath.Join(src, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(src, "empty"), 0o755)
	in := filepath.Join(h.w2home, "in")
	os.MkdirAll(filepath.Join(in, "out", "deep", "b"), 0o755)
	// an earlier copy that broke off: resumed from its prefix
	os.WriteFile(filepath.Join(in, "out", "deep", "b", "big.bin.lapart"), big[:1<<20], 0o644)

	r := h.client(h.root, "cp", "w1:"+src, "w2:"+in)
	if r.code != 0 {
		t.Fatalf("cp: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, "3 file(s)") || !strings.Contains(r.stderr, "1.0 MiB resumed") {
		t.Errorf("summary: %s", r.stderr)
	}
	for rel, b := range files {
		got, err := os.ReadFile(filepath.Join(in, "out", filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("w2 %s: %d bytes, %v", rel, len(got), err)
		}
	}
	if _, err := os.Stat(filepath.Join(in, "out", "deep", "b", "big.bin.lapart")); err == nil {
		t.Error("the partial file is still there")
	}
	if fi, err := os.Stat(filepath.Join(in, "out", "empty")); err != nil || !fi.IsDir() {
		t.Errorf("the empty directory: %v", err)
	}
	// again: nothing to send
	r = h.client(h.root, "cp", "w1:"+src, "w2:"+in)
	if r.code != 0 || !strings.Contains(r.stderr, "0 B sent") || !strings.Contains(r.stderr, "3 up to date") {
		t.Errorf("again: exit %d\n%s", r.code, r.stderr)
	}
	// one file, to an exact path
	one := filepath.Join(h.w2home, "single", "copy.txt")
	if r := h.client(h.root, "cp", "w1:"+filepath.Join(src, "a.txt"), "w2:"+one); r.code != 0 {
		t.Fatalf("cp a file: exit %d\n%s", r.code, r.stderr)
	}
	if got, _ := os.ReadFile(one); string(got) != "hello" {
		t.Errorf("the file: %q", got)
	}
	// a missing source: the source worker says so, nothing is written
	if r := h.client(h.root, "cp", "w1:"+filepath.Join(src, "missing"), "w2:"+filepath.Join(h.w2home, "nothing")); r.code == 0 || !strings.Contains(r.stderr, "does not exist") {
		t.Errorf("a missing source: exit %d\n%s", r.code, r.stderr)
	}
	// one side here: put
	local := filepath.Join(h.root, "local.txt")
	os.WriteFile(local, []byte("local"), 0o644)
	if r := h.client(h.root, "cp", local, "w2:"+filepath.Join(h.w2home, "put.txt")); r.code != 0 {
		t.Fatalf("cp from here: exit %d\n%s", r.code, r.stderr)
	}
	if got, _ := os.ReadFile(filepath.Join(h.w2home, "put.txt")); string(got) != "local" {
		t.Errorf("put: %q", got)
	}
	// the same worker on both sides
	if r := h.client(h.root, "cp", "w1:"+src, "w1:"+filepath.Join(h.w1home, "x")); r.code == 0 || !strings.Contains(r.stderr, "both paths are on w1") {
		t.Errorf("w1 to w1: exit %d\n%s", r.code, r.stderr)
	}
}
