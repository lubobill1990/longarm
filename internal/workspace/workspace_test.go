package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
)

func TestRepoKey(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/example/longarm":     "github.com_example_longarm",
		"https://github.com/example/longarm.git": "github.com_example_longarm",
		"git@github.com:o/r.git":                 "github.com_o_r",
		"ssh://git@host:2222/a/b/":               "host_2222_a_b",
		"my repo":                                "my_repo",
		"..":                                     "repo",
	} {
		got := RepoKey(in)
		if got != want {
			t.Errorf("RepoKey(%q) = %q, want %q", in, got, want)
		}
		if !ValidRepo(got) {
			t.Errorf("RepoKey(%q) = %q is not valid", in, got)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", `a\b`, "x.git", ".hidden", "a..b"} {
		if ValidRepo(bad) {
			t.Errorf("ValidRepo(%q) = true", bad)
		}
	}
}

func writeTar(t *testing.T, entries map[string]byte) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, typ := range entries {
		h := &tar.Header{Name: name, Typeflag: typ, Mode: 0o644, Size: 0}
		if typ == tar.TypeReg {
			h.Size = 2
		}
		if typ == tar.TypeSymlink {
			h.Linkname = "/etc/passwd"
		}
		tw.WriteHeader(h)
		if typ == tar.TypeReg {
			tw.Write([]byte("ok"))
		}
	}
	tw.Close()
	p := filepath.Join(t.TempDir(), "x.tar")
	os.WriteFile(p, buf.Bytes(), 0o644)
	return p
}

func TestExtractTarRejectsEscapes(t *testing.T) {
	dst := t.TempDir()
	if err := ExtractTar(writeTar(t, map[string]byte{"a/b.txt": tar.TypeReg, "dir": tar.TypeDir}), dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a", "b.txt")); string(b) != "ok" {
		t.Fatalf("extracted %q", b)
	}
	for _, bad := range []map[string]byte{
		{"../evil": tar.TypeReg}, {"/abs": tar.TypeReg}, {"a/../../evil": tar.TypeReg},
		{"link": tar.TypeSymlink}, {".git/config": tar.TypeReg},
	} {
		if err := ExtractTar(writeTar(t, bad), dst); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "evil")); err == nil {
		t.Fatal("escaped the destination")
	}
}

func run(t *testing.T, dir string, args ...string) string {
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

// TestMirrorBundleWorktree exercises the store end to end with real git:
// bundle import, Refs, Checkout (create + reset), FetchURL and PrepareJob.
func TestMirrorBundleWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// Git for Windows' default (system gitconfig): the worktree must still
	// get the commit's LF bytes.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.autocrlf")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	root := t.TempDir()
	s := &Store{Root: filepath.Join(root, "data")}
	ctx := context.Background()
	src := filepath.Join(root, "src")
	os.MkdirAll(src, 0o755)
	run(t, src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\n"), 0o644)
	run(t, src, "add", ".")
	run(t, src, "commit", "-qm", "c1")
	c1 := run(t, src, "rev-parse", "HEAD")

	if exists, _, err := s.Refs(ctx, "proj"); err != nil || exists {
		t.Fatalf("refs of a missing mirror: %v %v", exists, err)
	}
	bundle := filepath.Join(root, "b1")
	run(t, src, "bundle", "create", "-q", bundle, "HEAD")
	if err := s.ImportBundle(ctx, "proj", bundle, "main", c1); err != nil {
		t.Fatal(err)
	}
	if _, commits, _ := s.Refs(ctx, "proj"); len(commits) == 0 || commits[0] != c1 {
		t.Fatalf("refs %v", commits)
	}
	if err := s.ImportBundle(ctx, "proj", "", "", strings.Repeat("ab", 20)); err == nil {
		t.Fatal("unknown commit accepted")
	}

	wt := filepath.Join(root, "wt")
	patch := filepath.Join(root, "patch")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\ntwo\n"), 0o644)
	os.WriteFile(patch, []byte(run(t, src, "diff", "--binary", "HEAD")+"\n"), 0o644)
	if err := s.Checkout(ctx, "proj", c1, wt, patch, "", nil); err != nil {
		t.Fatal(err)
	}
	if !s.MirrorInUse("proj") {
		t.Fatal("mirror with a live worktree not reported in use")
	}
	// The job's own git in the worktree sees the mirror's setting.
	if b, err := exec.Command("git", "-C", wt, "config", "--local", "core.autocrlf").Output(); err != nil || strings.TrimSpace(string(b)) != "false" {
		t.Fatalf("core.autocrlf in the worktree: %q %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "a.txt")); string(b) != "one\ntwo\n" {
		t.Fatalf("a.txt = %q", b)
	}
	// Reset of an existing worktree; foreign dirs are refused.
	if err := s.Checkout(ctx, "proj", c1, wt, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "a.txt")); string(b) != "one\n" {
		t.Fatalf("after reset a.txt = %q", b)
	}
	foreign := filepath.Join(root, "foreign")
	os.MkdirAll(foreign, 0o755)
	os.WriteFile(filepath.Join(foreign, "x"), nil, 0o644)
	if err := s.Checkout(ctx, "proj", c1, foreign, "", "", nil); err == nil {
		t.Fatal("checkout into a foreign directory succeeded")
	}

	// URL fetch (a local path stands in for a remote) + PrepareJob.
	run(t, src, "commit", "-qam", "c2")
	c2 := run(t, src, "rev-parse", "HEAD")
	var log bytes.Buffer
	got, err := s.FetchURL(ctx, "proj", src, "main", "", &log)
	if err != nil || got != c2 {
		t.Fatalf("FetchURL = %s %v", got, err)
	}
	if _, err := s.FetchURL(ctx, "proj", src, "main", "tok", &log); err == nil {
		t.Fatal("token accepted for a non-http url")
	}
	if err := s.Stage("01STAGE", "", ""); err != nil {
		t.Fatal(err)
	}
	spec := jobs.Spec{ID: "job1", Workspace: &Workspace{Repo: "proj", Commit: c1, StagingID: "01STAGE"}}
	dir, err := s.PrepareJob(ctx, jobs.Dir(filepath.Join(root, "jobdir")), spec, nil, &log)
	if err != nil || dir != s.JobWorktree("job1") {
		t.Fatalf("PrepareJob = %s %v\n%s", dir, err, log.String())
	}
	if _, err := os.Stat(s.StagingPath("01STAGE")); err == nil {
		t.Fatal("staging not removed after use")
	}
	if err := s.RemoveWorktree(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if out := run(t, s.Mirror("proj"), "worktree", "list"); strings.Contains(out, "job1") {
		t.Fatalf("worktree still registered:\n%s", out)
	}
	if time.Since(s.MirrorLastUsed("proj")) > time.Minute {
		t.Fatal("last-used not maintained")
	}
}

// Workspace is jobs.Workspace (alias for brevity in tests).
type Workspace = jobs.Workspace

func TestExtractTarRefusesSymlinkedParents(t *testing.T) {
	dst, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dst, "link")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if err := ExtractTar(writeTar(t, map[string]byte{"link/x": tar.TypeReg}), dst); err == nil {
		t.Fatal("extracted through a symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Fatal("file written outside the destination")
	}
}
