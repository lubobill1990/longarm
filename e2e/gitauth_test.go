package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/lubobill1990/longarm/internal/testutil/gitserver"
)

// credEnv is the environment of a client whose git credential helper
// knows tok for every host (the e2e stand-in for Git Credential Manager;
// the client's `git credential fill` asks it). longarmd never gets it.
func credEnv(tok string) []string {
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=!f() { echo username=e2e; echo password=" + tok + "; }; f"}
}

// longarmWithEnv is env.longarm with extra environment.
func longarmWithEnv(t *testing.T, cfgDir string, extra []string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bins.longarm, args...)
	cmd.Env = append(append(os.Environ(), "LONGARM_CONFIG_DIR="+cfgDir), extra...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("longarm %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return result{out.String(), errb.String(), code}
}

// remoteHistory makes <srv.Root>/proj.git with n commits of 64 KiB of
// random (incompressible) data each, in one git fast-import; it returns
// the tip.
func remoteHistory(t *testing.T, srv *gitserver.Server, n int) string {
	t.Helper()
	bare := filepath.Join(srv.Root, "proj.git")
	git(t, srv.Root, "init", "-q", "--bare", "-b", "main", bare)
	var stream bytes.Buffer
	for i := 0; i < n; i++ {
		blob := make([]byte, 64<<10)
		rand.Read(blob)
		msg := fmt.Sprintf("commit %d", i)
		fmt.Fprintf(&stream, "commit refs/heads/main\ncommitter t <t@t> %d +0000\ndata %d\n%s\n", 1700000000+i, len(msg), msg)
		fmt.Fprintf(&stream, "M 644 inline f%03d.bin\ndata %d\n", i, len(blob))
		stream.Write(blob)
		stream.WriteString("\n")
	}
	cmd := exec.Command("git", "fast-import", "--quiet")
	cmd.Dir = bare
	cmd.Stdin = &stream
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v\n%s", err, out)
	}
	return git(t, bare, "rev-parse", "main")
}

// sizeOf parses longarm's human sizes ("12.3 KiB").
func sizeOf(t *testing.T, s string) float64 {
	t.Helper()
	f := strings.Fields(s)
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		t.Fatalf("size %q", s)
	}
	switch f[1] {
	case "KiB":
		v *= 1 << 10
	case "MiB":
		v *= 1 << 20
	case "GiB":
		v *= 1 << 30
	}
	return v
}

// noToken fails when tok is in any file under the roots.
func noToken(t *testing.T, tok string, roots ...string) {
	t.Helper()
	for _, root := range roots {
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || strings.HasSuffix(p, ".lock") {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(tok)) {
				t.Errorf("credential on disk in %s", p)
			}
			return nil
		})
	}
}

// TestGitAuthForward: --git-auth forward against an https remote that
// requires a token (git http-backend behind TLS). With a URL the worker
// fetches a commit that is no branch tip with the forwarded credential
// (and fails clearly without one); with a local checkout the worker
// fetches what the remote has and only the unpushed delta is shipped;
// when the worker can't use the credential it falls back to shipping
// everything. The token is never in job.json, the logs, the audit log,
// the job's environment or the worktree's git config.
func TestGitAuthForward(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// the test server's certificate (longarmd and its git inherit this)
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	const token = "e2e-FORWARDED-token-5b1d0c"
	srv := gitserver.New(t, t.TempDir(), token)
	tip := remoteHistory(t, srv, 40) // ~2.5 MiB of history
	bare := filepath.Join(srv.Root, "proj.git")
	base := git(t, bare, "rev-parse", "main~3") // no branch tip
	u := srv.RepoURL("proj")
	e := newEnv(t)
	e.startLongarmd()
	cl := func(tok string, args ...string) result { return longarmWithEnv(t, e.client, credEnv(tok), args...) }

	t.Run("url", func(t *testing.T) {
		req := filepath.Join(e.root, "request.json")
		payload := []byte(`{"request": "build"}`)
		os.WriteFile(req, payload, 0o600)
		id := ulid.Make().String()
		r := cl(token, "run", "w1", "--id", id, "--repo", u, "--ref", base, "--git-auth", "forward",
			"--stdin-file", req, "--", bins.helper, "stdin-sha")
		if r.code != 0 || strings.TrimSpace(r.stdout) != id {
			t.Fatalf("run: %+v", r)
		}
		if !strings.Contains(r.stderr, "forwarding a credential for 127.0.0.1") || strings.Contains(r.stderr+r.stdout, token) {
			t.Fatalf("run stderr: %s", r.stderr)
		}
		if w := e.longarm(e.client, "wait", "w1", id); w.code != 0 {
			t.Fatalf("wait: %+v", w)
		}
		logs := e.mustLongarm(e.client, "logs", "w1", id)
		if !strings.Contains(logs, fmt.Sprintf("%d %x\n", len(payload), sha256.Sum256(payload))) ||
			!strings.Contains(logs, "with the forwarded credential") {
			t.Fatalf("logs: %s", logs)
		}
		wt := filepath.Join(e.dataDir, "worktrees", id)
		if got := git(t, wt, "rev-parse", "HEAD"); got != base {
			t.Fatalf("worktree at %s, want %s", got, base)
		}
		if cfg := git(t, wt, "config", "--list"); strings.Contains(cfg, token) || strings.Contains(strings.ToLower(cfg), "extraheader") {
			t.Fatalf("worktree git config:\n%s", cfg)
		}
		if remotes := git(t, wt, "remote", "-v"); remotes != "" {
			t.Fatalf("remotes left behind: %s", remotes)
		}
		// the job's environment
		r = cl(token, "run", "w1", "--repo", u, "--ref", tip, "--git-auth", "forward", "-f", "--", bins.helper, "environ")
		if r.code != 0 || !strings.Contains(r.stdout, "PATH=") || strings.Contains(r.stdout, token) ||
			strings.Contains(strings.ToLower(r.stdout), "extraheader") || strings.Contains(r.stdout, "GIT_CONFIG_KEY") {
			t.Fatalf("job environment: %+v", r)
		}
		if jl := e.mustLongarm(e.client, "jobs", "w1", "--json", "--id", id); strings.Contains(jl, token) || !strings.Contains(jl, `"gitAuth": true`) {
			t.Fatalf("jobs --json: %s", jl)
		}
	})

	t.Run("session", func(t *testing.T) {
		// session start --repo <url> --git-auth forward: the same workspace path
		r := cl(token, "session", "start", "w1", "-s", "ga", "--repo", u, "--ref", base, "--git-auth", "forward", "--json", "--", bins.helper, "repl")
		var out struct {
			Cwd     string `json:"cwd"`
			Created bool   `json:"created"`
		}
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil || !out.Created || strings.Contains(r.stdout+r.stderr, token) {
			t.Fatalf("session start: %+v", r)
		}
		if got := git(t, out.Cwd, "rev-parse", "HEAD"); got != base {
			t.Fatalf("session worktree at %s, want %s", got, base)
		}
		if cfg := git(t, out.Cwd, "config", "--list"); strings.Contains(cfg, token) {
			t.Fatalf("worktree git config:\n%s", cfg)
		}
		logs := e.mustLongarm(e.client, "logs", "w1", "ga", "--session")
		// the session's preparation runs git like a job's (#36): no traces,
		// no credential headers
		if !strings.Contains(logs, "with the forwarded credential") || strings.Contains(logs, token) ||
			strings.Contains(logs, "Recv header") || strings.Contains(logs, "Authorization") {
			t.Fatalf("session transcript: %s", logs)
		}
		if sl := e.mustLongarm(e.client, "sessions", "w1", "-s", "ga", "--json"); strings.Contains(sl, token) || !strings.Contains(sl, `"gitAuth": true`) {
			t.Fatalf("sessions --json: %s", sl)
		}
		e.mustLongarm(e.client, "session", "stop", "w1", "-s", "ga", "--grace", "0")
	})

	t.Run("url_without_credential", func(t *testing.T) {
		// (another repository: a commit the mirror has is not fetched again)
		git(t, srv.Root, "clone", "-q", "--bare", bare, filepath.Join(srv.Root, "proj2.git"))
		r := e.longarm(e.client, "run", "w1", "--repo", srv.RepoURL("proj2"), "--ref", base, "--wait", "--", bins.helper, "print", "1", "0", "0")
		if r.code != 126 {
			t.Fatalf("fetch without a credential: %+v", r)
		}
		id := strings.TrimSpace(r.stdout)
		if jl := e.mustLongarm(e.client, "jobs", "w1", "--json", "--id", id); !strings.Contains(jl, "could not authenticate") {
			t.Fatalf("reason: %s", jl)
		}
		// no credential to be had here: exit 77 before anything is sent
		r = longarmWithEnv(t, e.client, []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0="},
			"run", "w1", "--repo", u, "--git-auth", "forward", "--", "x")
		if r.code != 77 || r.stdout != "" {
			t.Fatalf("no credential: %+v", r)
		}
		// http: refused (exit 2)
		if r := cl(token, "run", "w1", "--repo", strings.Replace(u, "https:", "http:", 1), "--git-auth", "forward", "--", "x"); r.code != 2 {
			t.Fatalf("http url: %+v", r)
		}
		if v := e.mustLongarm(e.client, "version", "--json"); !strings.Contains(v, `"git-auth-forward"`) {
			t.Fatalf("version --json: %s", v)
		}
	})

	// A local checkout of the remote with one unpushed commit.
	local := filepath.Join(e.root, "local")
	git(t, e.root, "clone", "-q", bare, local)
	git(t, local, "remote", "set-url", "origin", u)
	os.WriteFile(filepath.Join(local, "unpushed.txt"), []byte("the delta\n"), 0o644)
	git(t, local, "add", ".")
	git(t, local, "commit", "-qm", "unpushed")
	head := git(t, local, "rev-parse", "HEAD")

	t.Run("local_delta", func(t *testing.T) {
		// everything, as without forwarding (another mirror)
		r := cl(token, "sync", local, "w1:"+filepath.ToSlash(filepath.Join(e.root, "wt-full")), "--repo-name", "full")
		if r.code != 0 {
			t.Fatalf("sync: %+v", r)
		}
		full := sizeOf(t, readSyncBytes(t, r.stderr, "bundle"))
		before := srv.Stats().UploadPackBytes
		wt := filepath.Join(e.root, "wt-delta")
		r = cl(token, "sync", local, "w1:"+filepath.ToSlash(wt), "--repo-name", "delta", "--git-auth", "forward")
		if r.code != 0 || !strings.Contains(r.stderr, "the worker fetched 1 of 1 base commit(s)") {
			t.Fatalf("sync --git-auth forward: %+v", r)
		}
		delta := sizeOf(t, readSyncBytes(t, r.stderr, "bundle"))
		fetched := srv.Stats().UploadPackBytes - before
		t.Logf("history %0.f B shipped without forwarding; with it: delta %0.f B shipped, the worker fetched %d B from the remote", full, delta, fetched)
		if full < 2<<20 || delta > 16<<10 || float64(fetched) < full/2 {
			t.Fatalf("full %0.f B, delta %0.f B, fetched %d B", full, delta, fetched)
		}
		if got := git(t, wt, "rev-parse", "HEAD"); got != head {
			t.Fatalf("worktree at %s, want %s", got, head)
		}
		// run --repo <local checkout>: the same, the job sees the commit
		r = cl(token, "run", "w1", "--repo", local, "--git-auth", "forward", "-f", "--", bins.helper, "cat", "unpushed.txt")
		if r.code != 0 || !strings.Contains(r.stdout, "the delta") || !strings.Contains(r.stderr, "shipping the delta") {
			t.Fatalf("run --repo <local> --git-auth forward: %+v", r)
		}
		// the worker's record of what that run shipped
		audit, _ := os.ReadFile(filepath.Join(e.dataDir, "audit.log"))
		var last string
		for _, l := range strings.Split(string(audit), "\n") {
			if strings.Contains(l, "Workspace/Sync") && strings.Contains(l, `"stage_only":true`) {
				last = l
			}
		}
		m := regexp.MustCompile(`"bundle_bytes":(\d+)`).FindStringSubmatch(last)
		if m == nil {
			t.Fatalf("audit: %q", last)
		}
		if n, _ := strconv.Atoi(m[1]); n > 16<<10 {
			t.Fatalf("run shipped a bundle of %d bytes", n)
		}
	})

	t.Run("local_fallback", func(t *testing.T) {
		// the remote refuses what the client forwards: a note, then
		// everything is shipped as without forwarding
		r := cl("wrong-token-000000", "sync", local, "w1:"+filepath.ToSlash(filepath.Join(e.root, "wt-fb")), "--repo-name", "fb", "--git-auth", "forward")
		if r.code != 0 || !strings.Contains(r.stderr, "could not fetch") || sizeOf(t, readSyncBytes(t, r.stderr, "bundle")) < 2<<20 {
			t.Fatalf("fallback: %+v", r)
		}
		// no https remote: a note
		other := filepath.Join(e.root, "other")
		git(t, e.root, "clone", "-q", bare, other)
		r = cl(token, "sync", other, "w1:"+filepath.ToSlash(filepath.Join(e.root, "wt-o")), "--repo-name", "o", "--git-auth", "forward")
		if r.code != 0 || !strings.Contains(r.stderr, "not an https remote") {
			t.Fatalf("file remote: %+v", r)
		}
	})

	b, _ := os.ReadFile(filepath.Join(e.root, "longarmd.out"))
	if bytes.Contains(b, []byte(token)) {
		t.Error("credential in longarmd's log")
	}
	noToken(t, token, e.root)
}

// TestGitAuthForwardHub: TMIA's call through the hub - run --id <ulid>
// --repo <https url> --ref <full sha> --git-auth forward --require
// pool=devbox --no-queue --stdin-file <request.json> - works, and the
// hub (its database, audit log and logs) never sees the credential.
func TestGitAuthForwardHub(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Parallel()
	const token = "e2e-HUB-forwarded-token-77aa"
	srv := gitserver.New(t, t.TempDir(), token)
	remoteHistory(t, srv, 3)
	base := git(t, filepath.Join(srv.Root, "proj.git"), "rev-parse", "main~1")
	h := newHubEnv(t, "")
	os.Remove(filepath.Join(h.client, "workers.toml"))
	wa := h.newWorkerDir(t, "wa", "")
	wa.env = []string{"GIT_SSL_NO_VERIFY=true"} // the test server's certificate
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) && n["metrics"] != nil })
	h.mustLongarm(h.admin, "admin", "label", "wa", "pool=devbox")
	h.waitNode(t, "wa", "labels", func(n map[string]any) bool {
		l, _ := n["labels"].(map[string]any)
		return l["pool"] == "devbox"
	})

	req := filepath.Join(h.root, "request.json")
	payload := []byte(`{"build": "request"}`)
	os.WriteFile(req, payload, 0o600)
	id := ulid.Make().String()
	r := longarmWithEnv(t, h.client, credEnv(token), "run", "--id", id, "--repo", srv.RepoURL("proj"), "--ref", base,
		"--git-auth", "forward", "--require", "pool=devbox", "--no-queue", "--stdin-file", req, "--", bins.helper, "stdin-sha")
	if r.code != 0 || strings.TrimSpace(r.stdout) != id {
		t.Fatalf("run: %+v", r)
	}
	if w := h.longarm(h.client, "wait", id); w.code != 0 {
		t.Fatalf("wait: %+v", w)
	}
	if logs := h.mustLongarm(h.client, "logs", id); !strings.Contains(logs, fmt.Sprintf("%d %x", len(payload), sha256.Sum256(payload))) {
		t.Fatalf("logs: %s", logs)
	}
	// `longarm logs <id>` right after: an earlier longarm remembered the
	// hub as down (a slow first connection) and no node list is cached,
	// so no worker is known - the hub is asked again before "not found"
	os.Remove(filepath.Join(h.client, "hub-cache.json"))
	state := fmt.Sprintf(`{"address": %q, "down": %q, "error": "context deadline exceeded"}`,
		h.addr(), time.Now().UTC().Format(time.RFC3339Nano))
	os.WriteFile(filepath.Join(h.client, "hub-state.json"), []byte(state), 0o600)
	if r := h.longarm(h.client, "logs", id); r.code != 0 || !strings.Contains(r.stdout, fmt.Sprintf("%d %x", len(payload), sha256.Sum256(payload))) {
		t.Fatalf("logs with the hub remembered down: %+v", r)
	}
	if r := h.longarm(h.client, "logs", "01NOSUCHJOB0000000000000000"); r.code != 3 {
		t.Fatalf("logs of an unknown job: %+v", r)
	}
	h.stopAll() // flush the hub's database
	noToken(t, token, h.hubDir, wa.dataDir, h.root)
}
