package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// lfsEnv configures git-lfs's filter as `git lfs install` does (globally,
// for every git these tests run - the worker's situation that made
// checkouts of shipped repositories fail).
func lfsEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		t.Skip("git-lfs not installed")
	}
	kv := [][2]string{
		{"filter.lfs.clean", "git-lfs clean -- %f"},
		{"filter.lfs.smudge", "git-lfs smudge -- %f"},
		{"filter.lfs.process", "git-lfs filter-process"},
		{"filter.lfs.required", "true"},
		{"core.autocrlf", "false"},
	}
	t.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(len(kv)))
	for i, p := range kv {
		n := strconv.Itoa(i)
		t.Setenv("GIT_CONFIG_KEY_"+n, p[0])
		t.Setenv("GIT_CONFIG_VALUE_"+n, p[1])
	}
}

// TestLFSWorktree: a shipped repository with Git LFS files checks out
// although the mirror has no LFS server (pointer files, noted in the log);
// with the objects shipped into the mirror's LFS store the files have
// their content and the worktree is clean.
func TestLFSWorktree(t *testing.T) {
	lfsEnv(t)
	root := t.TempDir()
	s := &Store{Root: filepath.Join(root, "data")}
	ctx := context.Background()
	src := filepath.Join(root, "src")
	os.MkdirAll(src, 0o755)
	run(t, src, "init", "-q", "-b", "main")
	run(t, src, "lfs", "track", "*.bin")
	content := []byte("large binary content\x00\x01\x02\n")
	os.WriteFile(filepath.Join(src, "big.bin"), content, 0o644)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("text\n"), 0o644)
	run(t, src, "add", ".")
	run(t, src, "commit", "-qm", "c1")
	c1 := run(t, src, "rev-parse", "HEAD")
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	if got := run(t, src, "lfs", "ls-files", "--long"); !strings.Contains(got, oid) {
		t.Fatalf("lfs ls-files: %s", got)
	}
	bundle := filepath.Join(root, "b1")
	run(t, src, "bundle", "create", "-q", bundle, "HEAD")
	if err := s.ImportBundle(ctx, "proj", bundle, "main", c1); err != nil {
		t.Fatal(err)
	}

	// Without the objects: pointer files, a note.
	var log bytes.Buffer
	wt := filepath.Join(root, "wt1")
	if err := s.Checkout(ctx, "proj", c1, wt, "", "", &log); err != nil {
		t.Fatalf("checkout of an LFS repository without its objects: %v\n%s", err, log.String())
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "big.bin")); !bytes.HasPrefix(b, []byte("version https://git-lfs")) {
		t.Fatalf("big.bin = %q, want a pointer file", b)
	}
	if !strings.Contains(log.String(), "pointer files") || !strings.Contains(log.String(), "--lfs ship") {
		t.Fatalf("no LFS note in the log:\n%s", log.String())
	}
	if got := s.LFSPresent("proj", []string{oid}); len(got) != 0 {
		t.Fatalf("present before import: %v", got)
	}

	// Ship the object (a tar named by oid, as longarm sends it).
	obj, err := os.ReadFile(filepath.Join(src, ".git", "lfs", "objects", oid[:2], oid[2:4], oid))
	if err != nil {
		t.Fatal(err)
	}
	tarFile := writeLFSTar(t, filepath.Join(root, "lfs.tar"), map[string][]byte{oid: obj})
	if n, err := s.ImportLFS(ctx, "proj", tarFile); err != nil || n != 1 {
		t.Fatalf("ImportLFS = %d %v", n, err)
	}
	if got := s.LFSPresent("proj", []string{oid, strings.Repeat("0", 64), "../x"}); !slices.Equal(got, []string{oid}) {
		t.Fatalf("present = %v", got)
	}
	if n, err := s.ImportLFS(ctx, "proj", tarFile); err != nil || n != 0 {
		t.Fatalf("second ImportLFS = %d %v", n, err)
	}
	log.Reset()
	if err := s.Checkout(ctx, "proj", c1, filepath.Join(root, "wt2"), "", "", &log); err != nil {
		t.Fatalf("checkout with the objects: %v\n%s", err, log.String())
	}
	if b, _ := os.ReadFile(filepath.Join(root, "wt2", "big.bin")); !bytes.Equal(b, content) {
		t.Fatalf("big.bin = %q, want its content\n%s", b, log.String())
	}
	if st := run(t, filepath.Join(root, "wt2"), "status", "--porcelain"); st != "" {
		t.Fatalf("worktree not clean after git lfs checkout:\n%s", st)
	}
	// The existing worktree, reset, gets the content too.
	if err := s.Checkout(ctx, "proj", c1, wt, "", "", &log); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "big.bin")); !bytes.Equal(b, content) {
		t.Fatalf("reset worktree: big.bin = %q", b)
	}

	// Content that does not match its oid, and foreign entries, are refused.
	bad := strings.Repeat("ab", 32)
	if _, err := s.ImportLFS(ctx, "proj", writeLFSTar(t, filepath.Join(root, "bad.tar"), map[string][]byte{bad: []byte("x")})); err == nil {
		t.Fatal("mismatched object accepted")
	}
	if _, err := os.Stat(s.lfsObject("proj", bad)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("mismatched object stored")
	}
	if _, err := s.ImportLFS(ctx, "proj", writeLFSTar(t, filepath.Join(root, "evil.tar"), map[string][]byte{"../../x": []byte("x")})); err == nil {
		t.Fatal("foreign entry accepted")
	}
}

func writeLFSTar(t *testing.T, p string, files map[string][]byte) string {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for name, b := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFetchError: a fetch the remote refused for lack of credentials
// names the cause and the two ways out; other failures stay as they are.
func TestFetchError(t *testing.T) {
	ado := errors.New("git fetch --no-tags --force -- https://user:secret@dev.azure.com/org/p/_git/r +main:refs/longarm/remote/main: exit status 128: " +
		"fatal: unable to access 'https://dev.azure.com/org/p/_git/r/': The requested URL returned error: 403")
	err := fetchError("https://user:secret@dev.azure.com/org/p/_git/r", false, ado)
	msg := err.Error()
	for _, want := range []string{"could not authenticate", "returned error: 403", "--repo .", "dev.azure.com", "Git Credential Manager",
		"https://user:xxxxx@dev.azure.com/org/p/_git/r"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if !errors.Is(err, ado) {
		t.Error("the git error is not wrapped")
	}
	prompt := errors.New("fatal: could not read Username for 'https://github.com': terminal prompts disabled")
	if msg := fetchError("https://github.com/o/r", true, prompt).Error(); !strings.Contains(msg, "GH_TOKEN") || !strings.Contains(msg, "could not authenticate") {
		t.Errorf("with a token: %s", msg)
	}
	other := errors.New("fatal: couldn't find remote ref no-such-branch")
	if got := fetchError("https://github.com/o/r", false, other); got != other {
		t.Errorf("unrelated failure rewritten: %v", got)
	}
}
