package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/testutil/gitserver"
)

// TestAgentHandoffRemote: --via remote (and auto above the threshold)
// pushes the snapshot to a temporary branch of the repository's https
// remote - a TLS smart-HTTP server that requires a token - the target
// fetches it with the forwarded credential and the branch is deleted
// afterwards; a worker source pushes with the forwarded credential. The
// secret check and a refused push fall back to the relay; --keep-branch
// keeps the branch and handoff --cleanup removes stale ones. No credential
// is left on disk or in a git config.
func TestAgentHandoffRemote(t *testing.T) {
	const token = "e2e-HANDOFF-remote-token-91c2"
	// the test server's certificate; push.followTags on the workers: their
	// pushes must still carry the handoff branch only
	h := newHandoffEnv(t, "GIT_SSL_NO_VERIFY=true", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=push.followTags", "GIT_CONFIG_VALUE_0=true")
	srv := gitserver.New(t, t.TempDir(), token)
	// GIT_CONFIG_NOSYSTEM: no system credential helper (Git Credential Manager) before the test's
	h.extra = append(credEnv(token), "GIT_SSL_NO_VERIFY=true", "GIT_CONFIG_NOSYSTEM=1")
	bare := filepath.Join(srv.Root, "app.git")
	git(t, srv.Root, "init", "-q", "--bare", "-b", "main", bare)
	git(t, bare, "config", "http.receivepack", "true")
	u := srv.RepoURL("app")

	repo := filepath.Join(h.root, "src", "app")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "core.autocrlf", "false")
	mustWrite(t, filepath.Join(repo, "tracked.txt"), "v1\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "c1")
	git(t, repo, "push", "-q", bare, "main:main")
	git(t, repo, "remote", "add", "origin", u)
	git(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD") // as if fetched
	git(t, repo, "config", "branch.main.remote", "origin")
	// an unpublished annotated tag in the snapshot's history, and a user
	// who pushes the tags that follow a commit: never with a handoff
	git(t, repo, "config", "push.followTags", "true")
	git(t, repo, "tag", "-a", "-m", "not for the remote", "v9.9.9-unpublished")
	mustWrite(t, filepath.Join(repo, "tracked.txt"), "v2 uncommitted\n")
	mustWrite(t, filepath.Join(repo, "notes", "untracked.txt"), "untracked\n")

	id := "7e57a7c1-0000-4000-8000-00000000c1a0"
	mustWrite(t, filepath.Join(h.chome, ".claude", "projects", claudeDir(repo), id+".jsonl"),
		fmt.Sprintf(`{"type":"user","cwd":%s,"sessionId":%q,"version":"1.0.0","message":{"role":"user","content":"go"}}`+"\n", jsonQ(repo), id))
	tempBranches := func() string { return git(t, bare, "for-each-ref", "--format=%(refname)", "refs/heads/longarm") }
	onlyBranches := func() {
		t.Helper()
		if tags := git(t, bare, "for-each-ref", "--format=%(refname)", "refs/tags"); tags != "" {
			t.Errorf("a handoff pushed tags to the remote: %s", tags)
		}
		for _, r := range strings.Fields(git(t, bare, "for-each-ref", "--format=%(refname)")) {
			if r != "refs/heads/main" && !strings.HasPrefix(r, "refs/heads/longarm/handoff/") {
				t.Errorf("a handoff pushed %s to the remote", r)
			}
		}
	}
	t.Cleanup(onlyBranches)
	assertCode := func(dir string) {
		t.Helper()
		if got := readText(t, filepath.Join(dir, "tracked.txt")); got != "v2 uncommitted\n" {
			t.Errorf("tracked.txt in %s: %q", dir, got)
		}
		if got := readText(t, filepath.Join(dir, "notes", "untracked.txt")); got != "untracked\n" {
			t.Errorf("untracked file in %s: %q", dir, got)
		}
		if cfg := git(t, dir, "config", "--list"); strings.Contains(cfg, token) || strings.Contains(strings.ToLower(cfg), "extraheader") {
			t.Errorf("a credential in the git config of %s", dir)
		}
	}

	var w1, w2 handoffOut
	t.Run("auto_above_threshold_local_to_w1", func(t *testing.T) {
		var r result
		w1, r = h.handoff(repo, "--to", "w1", "--session", id, "--no-attach", "--git-auth", "forward", "--via", "auto", "--via-threshold", "1")
		if w1.Transfer != "remote" || !strings.HasPrefix(w1.Temp, "longarm/handoff/alice/7e57a7c1-") {
			t.Fatalf("handoff: %+v\n%s", w1, r.stderr)
		}
		if !strings.Contains(r.stderr, "deleted the temporary branch "+w1.Temp) || tempBranches() != "" {
			t.Errorf("the temporary branch was not deleted: %q\n%s", tempBranches(), r.stderr)
		}
		assertCode(w1.Worktree)
		if strings.Contains(r.stdout+r.stderr, token) {
			t.Error("the credential in longarm's output")
		}
	})

	t.Run("worker_pushes_w1_to_w2", func(t *testing.T) {
		onlyBranches()
		// an annotated tag in the worker's history too (its push.followTags is on)
		git(t, w1.Worktree, "tag", "-a", "-m", "not for the remote", "w1-unpublished")
		var r result
		w2, r = h.handoff(repo, "--from", "w1", "--to", "w2", "--session", id, "--no-attach", "--idle", "1s", "--grace", "5s",
			"--git-auth", "forward", "--via", "remote")
		onlyBranches()
		if w2.Transfer != "remote" || w2.Relayed != 0 || tempBranches() != "" {
			t.Fatalf("handoff: %+v branches %q\n%s", w2, tempBranches(), r.stderr)
		}
		assertCode(w2.Worktree)
	})

	t.Run("secret_check_falls_back_to_relay", func(t *testing.T) {
		mustWrite(t, filepath.Join(w2.Worktree, "deploy.env"), "AWS_KEY="+fakeAWSKey+"\n")
		out, r := h.handoff(repo, "--from", "w2", "--to", "local", "--session", id, "--idle", "1s", "--grace", "5s",
			"--git-auth", "forward", "--via", "remote")
		if out.Transfer != "bundle" || !strings.Contains(r.stderr, "the secret check found deploy.env") || tempBranches() != "" {
			t.Fatalf("secret check: %+v\n%s", out, r.stderr)
		}
		if strings.Contains(r.stderr, fakeAWSKey) {
			t.Error("the secret's value in the note")
		}
		assertCode(out.Worktree)
		w1 = out // the session is local now, in this worktree
	})

	t.Run("refused_push_falls_back", func(t *testing.T) {
		os.Remove(filepath.Join(w1.Worktree, "deploy.env"))
		git(t, bare, "config", "http.receivepack", "false")
		defer git(t, bare, "config", "http.receivepack", "true")
		out, r := h.handoff(w1.Worktree, "--to", "w1", "--session", id, "--no-attach", "--git-auth", "forward", "--via", "remote", "--name", "back1")
		if out.Transfer != "upload" || !strings.Contains(r.stderr, "the remote refused the push") {
			t.Fatalf("refused push: %+v\n%s", out, r.stderr)
		}
		assertCode(out.Worktree)
		w1 = out
	})

	t.Run("keep_branch_and_cleanup", func(t *testing.T) {
		out, r := h.handoff(repo, "--from", "w1", "--to", "w2", "--session", id, "--no-attach", "--idle", "1s", "--grace", "5s",
			"--git-auth", "forward", "--via", "remote", "--keep-branch", "--name", "kept")
		if out.Transfer != "remote" || tempBranches() != "refs/heads/"+out.Temp {
			t.Fatalf("--keep-branch: %+v %q\n%s", out, tempBranches(), r.stderr)
		}
		// a stale one of an earlier handoff
		git(t, bare, "update-ref", "refs/heads/longarm/handoff/alice/deadbeef-202001010000", git(t, bare, "rev-parse", "main"))
		c := h.client(repo, "handoff", "--cleanup", "--json")
		if c.code != 0 || !strings.Contains(c.stdout, `"branch": "longarm/handoff/alice/deadbeef-202001010000"`) {
			t.Fatalf("cleanup: %+v", c)
		}
		if got := tempBranches(); got != "refs/heads/"+out.Temp {
			t.Errorf("after cleanup: %q (the stale one gone, the fresh one kept)", got)
		}
		w2 = out
	})

	t.Run("worker_to_local_via_remote_allow_secrets", func(t *testing.T) {
		// the session is on w2 (kept); a secret in its changes: --allow-secrets
		// pushes anyway, with a warning; the new temporary branch goes, the
		// kept one stays
		kept := tempBranches()
		mustWrite(t, filepath.Join(w2.Worktree, "deploy.env"), "AWS_KEY="+fakeAWSKey+"\n")
		out, r := h.handoff(repo, "--from", "w2", "--to", "local", "--session", id, "--idle", "1s", "--grace", "5s",
			"--git-auth", "forward", "--via", "remote", "--allow-secrets")
		if out.Transfer != "remote" || !strings.Contains(r.stderr, "pushing despite the secret check") || strings.Contains(r.stderr, fakeAWSKey) {
			t.Fatalf("handoff: %+v\n%s", out, r.stderr)
		}
		if got := tempBranches(); got != kept {
			t.Errorf("temporary branches after the handoff: %q (want only the kept %q)", got, kept)
		}
		if got := readText(t, filepath.Join(out.Worktree, "deploy.env")); !strings.Contains(got, "AWS_KEY=") {
			t.Errorf("deploy.env locally: %q", got)
		}
		if cfg := git(t, out.Worktree, "config", "--list"); strings.Contains(cfg, token) {
			t.Error("a credential in the local worktree's git config")
		}
	})

	noToken(t, token, h.e.dataDir, h.e2.dataDir, h.w1home, h.w2home)
}

// fakeAWSKey looks like an AWS access key id to the secret check (built at
// run time so that no secret scanner of this repository flags the fixture).
var fakeAWSKey = "AKIA" + strings.Repeat("ABCD", 4)
