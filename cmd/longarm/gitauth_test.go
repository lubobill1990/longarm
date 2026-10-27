package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestCheckGitAuthFlag(t *testing.T) {
	for _, c := range []struct {
		mode, repo string
		ok         bool
	}{
		{"", "", true}, {"none", ".", true}, {"forward", ".", true}, {"forward", "https://h/r", true},
		{"forward", "", false}, {"yes", ".", false},
	} {
		if err := checkGitAuthFlag(c.mode, c.repo); (err == nil) != c.ok || (err != nil && exitCodeFor(err, true) != exitUsage) {
			t.Errorf("checkGitAuthFlag(%q, %q) = %v", c.mode, c.repo, err)
		}
	}
}

// TestRemoteBases: the commits the worker fetches before the delta is
// shipped are the boundary of what is not on the remote's tracking
// branches (the commit itself once it is pushed), from the upstream's
// remote.
func TestRemoteBases(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	commit := func(name string) string {
		os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644)
		gitT(t, dir, "add", ".")
		gitT(t, dir, "commit", "-qm", name)
		return gitT(t, dir, "rev-parse", "HEAD")
	}
	c1 := commit("a")
	c2 := commit("b")
	gitT(t, dir, "remote", "add", "up", "https://dev.azure.com/o/p/_git/r")
	gitT(t, dir, "remote", "add", "origin", "https://github.com/o/r")
	gitT(t, dir, "update-ref", "refs/remotes/up/main", c2) // "pushed" to up
	gitT(t, dir, "update-ref", "refs/remotes/origin/main", c1)
	gitT(t, dir, "config", "branch.main.remote", "up")
	gitT(t, dir, "config", "branch.main.merge", "refs/heads/main")
	lr := &localRepo{top: dir}
	if name, u := lr.remoteFor("HEAD"); name != "up" || u != "https://dev.azure.com/o/p/_git/r" {
		t.Fatalf("remoteFor = %s %s", name, u)
	}
	if name, _ := lr.remoteFor(c2); name != "origin" {
		t.Fatalf("remoteFor(<sha>) = %s", name)
	}
	if got := lr.remoteBases(c2, "up"); !slices.Equal(got, []string{c2}) {
		t.Fatalf("pushed: %v", got)
	}
	c3 := commit("c")
	c4 := commit("d")
	if got := lr.remoteBases(c4, "up"); !slices.Equal(got, []string{c2}) {
		t.Fatalf("two unpushed on up: %v", got)
	}
	if got := lr.remoteBases(c4, "origin"); !slices.Equal(got, []string{c1}) {
		t.Fatalf("on origin: %v", got)
	}
	if got := lr.remoteBases(c3, "nosuch"); len(got) != 0 {
		t.Fatalf("no tracking branches: %v", got)
	}
}
