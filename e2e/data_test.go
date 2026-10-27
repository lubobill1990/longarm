package e2e

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func randBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// startEchoServer runs the helper's TCP echo server (on the worker = this
// machine) and returns its port.
func startEchoServer(t *testing.T, dir string) int {
	t.Helper()
	pf := filepath.Join(dir, fmt.Sprintf("echo-%d.port", time.Now().UnixNano()))
	cmd := exec.Command(bins.helper, "echo-server", pf)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	var port int
	waitFor(t, "echo server", 30*time.Second, func() bool {
		b, _ := os.ReadFile(pf)
		port, _ = strconv.Atoi(string(b))
		return port > 0
	})
	return port
}

// TestForward: longarm forward with many concurrent connections, both
// half-close directions, --list and --stop.
func TestForward(t *testing.T) {
	e := newEnv(t)
	e.startLongarmd()
	port := startEchoServer(t, e.root)
	local := freePort(t)
	b := e.longarmBG(t, e.client, "forward", "w1", strconv.Itoa(port), strconv.Itoa(local))
	waitOutput(t, b.stderr.String, "forwarding", 30*time.Second)

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := randBytes(t, 50_000+i*1000)
			c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", local))
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			go func() {
				c.Write(payload)
				c.(*net.TCPConn).CloseWrite() // half-close: server answers after EOF
			}()
			c.SetReadDeadline(time.Now().Add(30 * time.Second))
			got, err := io.ReadAll(c)
			if err != nil {
				errs <- err
				return
			}
			want := append(append([]byte(nil), payload...), []byte(fmt.Sprintf("%d bytes\n", len(payload)))...)
			if !bytes.Equal(got, want) {
				errs <- fmt.Errorf("conn %d: got %d bytes, want %d", i, len(got), len(want))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Connecting to a closed port on the worker fails cleanly.
	dead := freePort(t)
	b2 := e.longarmBG(t, e.client, "forward", "w1", strconv.Itoa(dead), strconv.Itoa(freePort(t)))
	waitOutput(t, b2.stderr.String, "forwarding", 30*time.Second)

	list := e.mustLongarm(e.client, "forward", "--list")
	if !strings.Contains(list, fmt.Sprintf("127.0.0.1:%d", local)) || !strings.Contains(list, strconv.Itoa(port)) {
		t.Fatalf("--list:\n%s", list)
	}
	out := e.mustLongarm(e.client, "forward", "--stop", strconv.Itoa(local))
	if !strings.Contains(out, "stopped") {
		t.Fatalf("--stop: %s", out)
	}
	b.wait(t, 10*time.Second)
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", local), time.Second); err == nil {
		t.Fatal("forward still listening after --stop")
	}
	e.mustLongarm(e.client, "forward", "--stop", "all")
	b2.wait(t, 10*time.Second)
	if r := e.longarm(e.client, "forward", "--stop", "all"); r.code == 0 {
		t.Fatal("--stop with nothing running succeeded")
	}
}

// TestFiles: longarm put / get of trees, skip-if-identical, resume of partial
// transfers in both directions, corrupted partials.
func TestFiles(t *testing.T) {
	e := newEnv(t)
	e.startLongarmd()
	src := filepath.Join(e.root, "src")
	os.MkdirAll(filepath.Join(src, "sub", "deeper"), 0o755)
	os.MkdirAll(filepath.Join(src, "empty"), 0o755)
	big := randBytes(t, 3_000_000)
	os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644)
	os.WriteFile(filepath.Join(src, "sub", "a.txt"), []byte("alpha\n"), 0o644)
	os.WriteFile(filepath.Join(src, "sub", "deeper", "b.txt"), []byte("beta\n"), 0o644)
	remote := filepath.ToSlash(filepath.Join(e.root, "remote"))

	r := e.longarm(e.client, "put", src, "w1:"+remote+"/")
	if r.code != 0 || !strings.Contains(r.stderr, "3 file(s) uploaded") {
		t.Fatalf("put: %+v", r)
	}
	for _, p := range []string{"big.bin", "sub/a.txt", "sub/deeper/b.txt", "empty"} {
		if _, err := os.Stat(filepath.Join(e.root, "remote", "src", p)); err != nil {
			t.Fatalf("missing %s after put", p)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(e.root, "remote", "src", "big.bin")); !bytes.Equal(got, big) {
		t.Fatal("big.bin differs after put")
	}
	if r := e.longarm(e.client, "put", src, "w1:"+remote+"/"); !strings.Contains(r.stderr, "3 up to date") {
		t.Fatalf("second put: %+v", r)
	}

	// get the tree back into an existing dir.
	back := filepath.Join(e.root, "back")
	os.MkdirAll(back, 0o755)
	if r := e.longarm(e.client, "get", "w1:"+remote+"/src", back); r.code != 0 || !strings.Contains(r.stderr, "3 file(s) downloaded") {
		t.Fatalf("get: %+v", r)
	}
	if got, _ := os.ReadFile(filepath.Join(back, "src", "big.bin")); !bytes.Equal(got, big) {
		t.Fatal("big.bin differs after get")
	}
	if got, _ := os.ReadFile(filepath.Join(back, "src", "sub", "deeper", "b.txt")); string(got) != "beta\n" {
		t.Fatalf("b.txt = %q", got)
	}

	// Resume an interrupted upload: a valid prefix is kept ...
	dst := filepath.Join(e.root, "remote", "resume.bin")
	os.WriteFile(dst+".lapart", big[:1_000_000], 0o644)
	r = e.longarm(e.client, "put", filepath.Join(src, "big.bin"), "w1:"+filepath.ToSlash(dst))
	if r.code != 0 || !strings.Contains(r.stderr, "resuming") || !strings.Contains(r.stderr, "uploaded (1.9 MiB") {
		t.Fatalf("resumed put: %+v", r)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, big) {
		t.Fatal("resumed upload differs")
	}
	if _, err := os.Stat(dst + ".lapart"); err == nil {
		t.Fatal(".lapart left behind")
	}
	// ... a corrupted one is discarded.
	dst2 := filepath.Join(e.root, "remote", "corrupt.bin")
	bad := append([]byte(nil), big[:1_000_000]...)
	bad[500] ^= 0xff
	os.WriteFile(dst2+".lapart", bad, 0o644)
	r = e.longarm(e.client, "put", filepath.Join(src, "big.bin"), "w1:"+filepath.ToSlash(dst2))
	if r.code != 0 || strings.Contains(r.stderr, "resuming") {
		t.Fatalf("put over corrupt partial: %+v", r)
	}
	if got, _ := os.ReadFile(dst2); !bytes.Equal(got, big) {
		t.Fatal("upload over corrupt partial differs")
	}

	// Resume an interrupted download.
	local := filepath.Join(e.root, "dl.bin")
	os.WriteFile(local+".lapart", big[:2_000_000], 0o644)
	r = e.longarm(e.client, "get", "w1:"+filepath.ToSlash(dst), local)
	if r.code != 0 || !strings.Contains(r.stderr, "resuming") {
		t.Fatalf("resumed get: %+v", r)
	}
	if got, _ := os.ReadFile(local); !bytes.Equal(got, big) {
		t.Fatal("resumed download differs")
	}
	if r := e.longarm(e.client, "get", "w1:"+filepath.ToSlash(dst), local); !strings.Contains(r.stderr, "1 up to date") {
		t.Fatalf("second get: %+v", r)
	}
	// Errors.
	if r := e.longarm(e.client, "get", "w1:"+remote+"/nope", local); r.code == 0 {
		t.Fatal("get of a missing file succeeded")
	}
	// Audit records writes.
	audit, _ := os.ReadFile(filepath.Join(e.dataDir, "audit.log"))
	if !strings.Contains(string(audit), "/longarm.v1.Files/Write") {
		t.Fatal("no Files.Write audit record")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func readSyncBytes(t *testing.T, stderr, what string) string {
	t.Helper()
	i := strings.Index(stderr, what+" ")
	if i < 0 {
		t.Fatalf("no %q in %q", what, stderr)
	}
	f := strings.Fields(stderr[i+len(what)+1:])
	return f[0] + " " + strings.TrimSuffix(f[1], ",")
}

// TestSync: longarm sync (bundle negotiation, uncommitted + untracked files,
// worktree reset keeping ignored files) and longarm run --repo (local checkout
// and clone URL; job state "preparing" -> worktree as cwd).
func TestSync(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := newEnv(t)
	e.startLongarmd()
	repo := filepath.Join(e.root, "repo")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("v1\n"), 0o644)
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored/\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "bin.dat"), []byte{0, 1, 2, 3, 255}, 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "c1")
	os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("v1\nv2 uncommitted\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "bin.dat"), []byte{9, 9, 0, 255}, 0o644) // binary diff
	os.WriteFile(filepath.Join(repo, "new.txt"), []byte("untracked\n"), 0o644)
	os.MkdirAll(filepath.Join(repo, "ignored"), 0o755)
	os.WriteFile(filepath.Join(repo, "ignored", "x"), []byte("never sent"), 0o644)

	wt := filepath.Join(e.root, "wt")
	r := e.longarm(e.client, "sync", repo, "w1:"+filepath.ToSlash(wt))
	if r.code != 0 {
		t.Fatalf("sync: %+v", r)
	}
	check := func(name, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(wt, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q (%v), want %q", name, got, err, want)
		}
	}
	check("tracked.txt", "v1\nv2 uncommitted\n")
	check("bin.dat", string([]byte{9, 9, 0, 255}))
	check("new.txt", "untracked\n")
	if _, err := os.Stat(filepath.Join(wt, "ignored")); err == nil {
		t.Fatal("ignored files were sent")
	}
	head1 := git(t, repo, "rev-parse", "HEAD")
	if got := git(t, wt, "rev-parse", "HEAD"); got != head1 {
		t.Fatalf("worktree HEAD %s, want %s", got, head1)
	}
	firstBundle := readSyncBytes(t, r.stderr, "bundle")

	// New commit: only the new objects travel; worker-side ignored files
	// (e.g. node_modules) survive, stray untracked ones are cleaned.
	os.MkdirAll(filepath.Join(wt, "ignored"), 0o755)
	os.WriteFile(filepath.Join(wt, "ignored", "keep"), []byte("deps"), 0o644)
	os.WriteFile(filepath.Join(wt, "stray.txt"), []byte("x"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "c2")
	r = e.longarm(e.client, "sync", repo, "w1:"+filepath.ToSlash(wt))
	if r.code != 0 {
		t.Fatalf("sync 2: %+v", r)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != git(t, repo, "rev-parse", "HEAD") {
		t.Fatal("worktree not at the new HEAD")
	}
	if !strings.Contains(r.stderr, "patch 0 B") || !strings.Contains(r.stderr, "untracked 0 B") {
		t.Fatalf("clean tree sent changes: %s", r.stderr)
	}
	t.Logf("bundles: first %s, incremental %s", firstBundle, readSyncBytes(t, r.stderr, "bundle"))
	check("ignored/keep", "deps")
	if _, err := os.Stat(filepath.Join(wt, "stray.txt")); err == nil {
		t.Fatal("stray untracked file survived the reset")
	}
	// Nothing new: no bundle at all.
	r = e.longarm(e.client, "sync", repo, "w1:"+filepath.ToSlash(wt))
	if r.code != 0 || !strings.Contains(r.stderr, "bundle 0 B") {
		t.Fatalf("sync 3: %+v", r)
	}
	// A non-worktree target is refused.
	other := filepath.Join(e.root, "notawt")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "f"), []byte("x"), 0o644)
	if r := e.longarm(e.client, "sync", repo, "w1:"+filepath.ToSlash(other)); r.code == 0 || !strings.Contains(r.stderr, "not a worktree") {
		t.Fatalf("sync into a foreign dir: %+v", r)
	}

	// longarm run --repo <local checkout>: commits + uncommitted changes.
	os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("job-state\n"), 0o644)
	out := e.longarm(e.client, "run", "w1", "--repo", repo, "-f", "--", bins.helper, "cat", "tracked.txt")
	if out.code != 0 || !strings.Contains(out.stdout, "job-state") || !strings.Contains(out.stdout, "longarmd: worktree") {
		t.Fatalf("run --repo: %+v", out)
	}
	id := strings.SplitN(out.stdout, "\n", 2)[0]
	st := e.status(id)
	if st.Workdir == "" || !strings.HasPrefix(st.Workdir, filepath.Join(e.dataDir, "worktrees")) {
		t.Fatalf("workdir %q", st.Workdir)
	}
	// longarm run --repo <url>: the worker fetches (file:// stands in for GitHub).
	bare := filepath.Join(e.root, "bare.git")
	git(t, e.root, "clone", "-q", "--bare", repo, bare)
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(bare), "/")
	out = e.longarm(e.client, "run", "w1", "--repo", url, "--ref", "main", "-f", "--", bins.helper, "cat", "tracked.txt")
	if out.code != 0 || !strings.Contains(out.stdout, "v1") || !strings.Contains(out.stdout, "longarmd: fetching") {
		t.Fatalf("run --repo url: %+v", out)
	}
	// A bad ref fails the job in "preparing" with exit 126.
	out = e.longarm(e.client, "run", "w1", "--repo", url, "--ref", "no-such-branch", "--wait", "--", bins.helper, "cat", "x")
	if out.code != 126 {
		t.Fatalf("bad ref: %+v", out)
	}

	// --ref <full commit id> of a local checkout: a commit on no branch is
	// shipped (through a temporary ref, gone afterwards) and checked out
	// detached; a short id is refused.
	side := filepath.Join(e.root, "side")
	git(t, repo, "worktree", "add", "-q", "-b", "side", side)
	os.WriteFile(filepath.Join(side, "side.txt"), []byte("side commit\n"), 0o644)
	git(t, side, "add", "side.txt")
	git(t, side, "commit", "-qm", "side")
	sideCommit := git(t, side, "rev-parse", "HEAD")
	git(t, repo, "worktree", "remove", "--force", side)
	git(t, repo, "branch", "-D", "side")
	out = e.longarm(e.client, "run", "w1", "--repo", repo, "--ref", sideCommit, "-f", "--", bins.helper, "cat", "side.txt")
	if out.code != 0 || !strings.Contains(out.stdout, "side commit") {
		t.Fatalf("run --ref <commit id>: %+v", out)
	}
	if refs := git(t, repo, "for-each-ref", "refs/longarm/"); refs != "" {
		t.Fatalf("temporary refs left behind: %s", refs)
	}
	out = e.longarm(e.client, "run", "w1", "--repo", repo, "--ref", sideCommit[:8], "--", bins.helper, "cat", "side.txt")
	if out.code == 0 || !strings.Contains(out.stderr, "short commit id") || !strings.Contains(out.stderr, sideCommit) {
		t.Fatalf("run --ref <short id>: %+v", out)
	}
}

// TestSyncLFS: longarm run --repo <checkout with Git LFS files>: by default
// the job's worktree has the pointer files (and the client says so); with
// --lfs ship the commit's LFS objects travel and the files have their
// content.
func TestSyncLFS(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		t.Skip("git-lfs not installed")
	}
	e := newEnv(t)
	e.startLongarmd()
	repo := filepath.Join(e.root, "lfsrepo")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "lfs", "install", "--local")
	git(t, repo, "lfs", "track", "*.bin")
	os.WriteFile(filepath.Join(repo, "big.bin"), []byte("lfs content\n"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "c1")

	out := e.longarm(e.client, "run", "w1", "--repo", repo, "-f", "--", bins.helper, "cat", "big.bin")
	if out.code != 0 || !strings.Contains(out.stdout, "version https://git-lfs") || !strings.Contains(out.stderr, "--lfs ship") {
		t.Fatalf("run --repo (lfs skip): %+v", out)
	}
	out = e.longarm(e.client, "run", "w1", "--repo", repo, "--lfs", "ship", "-f", "--", bins.helper, "cat", "big.bin")
	if out.code != 0 || !strings.Contains(out.stdout, "lfs content") {
		t.Fatalf("run --repo --lfs ship: %+v", out)
	}
	// A url is fetched by the worker: nothing to ship.
	if out := e.longarm(e.client, "run", "w1", "--repo", "https://example.com/r.git", "--lfs", "ship", "--", bins.helper, "cat", "x"); out.code == 0 ||
		!strings.Contains(out.stderr, "local checkout") {
		t.Fatalf("--lfs ship with a url: %+v", out)
	}
}

// TestGitHubFetch fetches this repository from GitHub with GH_TOKEN as a
// job secret (opt-in: LONGARM_GITHUB_TEST=1 and GH_TOKEN set).
func TestGitHubFetch(t *testing.T) {
	if os.Getenv("LONGARM_GITHUB_TEST") == "" || os.Getenv("GH_TOKEN") == "" {
		t.Skip("set LONGARM_GITHUB_TEST=1 and GH_TOKEN")
	}
	e := newEnv(t)
	e.startLongarmd()
	out := e.longarm(e.client, "run", "w1", "--repo", "https://github.com/lubobill1990/longarm", "--ref", "master",
		"--secret-env", "GH_TOKEN", "-f", "--", "git", "log", "--oneline", "-1")
	if out.code != 0 || !strings.Contains(out.stdout, "longarmd: fetching") {
		t.Fatalf("%+v", out)
	}
	if strings.Contains(out.stdout, os.Getenv("GH_TOKEN")) {
		t.Fatal("token in output")
	}
}
