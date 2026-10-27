package main

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefSHA(t *testing.T) {
	for _, c := range []struct {
		ref         string
		full, short bool
	}{
		{strings.Repeat("a1", 20), true, false},
		{strings.Repeat("b2", 32), true, false},
		{"abc1234", false, true},
		{strings.Repeat("c", 41), false, true},
		{"main", false, false},
		{"HEAD", false, false},
		{"v1.2.3", false, false},
		{strings.Repeat("A1", 20), false, false}, // git prints lower case; an upper-case "sha" is a ref name
		{"abc", false, false},
	} {
		if got := fullSHA(c.ref); got != c.full {
			t.Errorf("fullSHA(%q) = %v", c.ref, got)
		}
		if got := shortSHA(c.ref); got != c.short {
			t.Errorf("shortSHA(%q) = %v", c.ref, got)
		}
	}
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.autocrlf=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestLFSObjects: --lfs ship finds the commit's LFS objects in the local
// store and tars those the worker lacks, named by oid.
func TestLFSObjects(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		t.Skip("git-lfs not installed")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "lfs", "install", "--local")
	gitT(t, dir, "lfs", "track", "*.bin")
	a, b := []byte("object a\n"), []byte("object b\n")
	os.WriteFile(filepath.Join(dir, "a.bin"), a, 0o644)
	os.WriteFile(filepath.Join(dir, "b.bin"), b, 0o644)
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-qm", "c1")
	lr, err := openLocalRepo(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	commit := gitT(t, dir, "rev-parse", "HEAD")
	if !lr.usesLFS(commit) {
		t.Fatal("usesLFS = false")
	}
	oids, store, err := lr.lfsObjects(commit)
	if err != nil {
		t.Fatal(err)
	}
	oidOf := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	if len(oids) != 2 || !strings.Contains(strings.Join(oids, " "), oidOf(a)) || !strings.Contains(strings.Join(oids, " "), oidOf(b)) {
		t.Fatalf("oids %v", oids)
	}
	// the worker has a; b goes, plus one that is missing here
	missing := strings.Repeat("0", 64)
	p, err := lfsObjectsTar(filepath.Join(t.TempDir(), "lfs.tar"), store, append(oids, missing), []string{oidOf(a)})
	if err != nil || p == "" {
		t.Fatalf("tar %q %v", p, err)
	}
	f, _ := os.Open(p)
	defer f.Close()
	tr := tar.NewReader(f)
	h, err := tr.Next()
	if err != nil || h.Name != oidOf(b) {
		t.Fatalf("entry %v %v", h, err)
	}
	if got, _ := io.ReadAll(tr); string(got) != string(b) {
		t.Fatalf("content %q", got)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("more entries: %v", err)
	}
	// nothing to send
	if p, err := lfsObjectsTar(filepath.Join(t.TempDir(), "x.tar"), store, oids, oids); err != nil || p != "" {
		t.Fatalf("all present: %q %v", p, err)
	}
	// LFS attributes in a nested .gitattributes only
	nested := t.TempDir()
	gitT(t, nested, "init", "-q")
	os.MkdirAll(filepath.Join(nested, "assets"), 0o755)
	os.WriteFile(filepath.Join(nested, "assets", ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644)
	gitT(t, nested, "add", ".")
	gitT(t, nested, "commit", "-qm", "c")
	if lr, _ := openLocalRepo(nested, ""); !lr.usesLFS(gitT(t, nested, "rev-parse", "HEAD")) {
		t.Fatal("usesLFS misses a nested .gitattributes")
	}
	// a repository without LFS
	plain := t.TempDir()
	gitT(t, plain, "init", "-q")
	os.WriteFile(filepath.Join(plain, "x"), []byte("x"), 0o644)
	gitT(t, plain, "add", ".")
	gitT(t, plain, "commit", "-qm", "c")
	if lr, _ := openLocalRepo(plain, ""); lr.usesLFS(gitT(t, plain, "rev-parse", "HEAD")) {
		t.Fatal("usesLFS of a plain repository")
	}
}
