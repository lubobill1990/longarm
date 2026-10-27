package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/testutil/gitserver"
)

const testToken = "tok-FORWARDED-0123456789abcdef"

func bearer(tok string) *jobs.GitAuth {
	return &jobs.GitAuth{Scheme: jobs.GitAuthBearer, Secret: []byte(tok)}
}

// TestFetchAuthEnv: the credential goes into git's environment as
// http.<url>.extraHeader for the remote (and its .git twin, git-lfs's
// endpoint) only, credential helpers off, appended after the worker's own
// GIT_CONFIG_* entries; https only.
func TestFetchAuthEnv(t *testing.T) {
	fa, err := newFetchAuth("https://org@dev.azure.com/org/p/_git/r?x=1", bearer(testToken), "")
	if err != nil {
		t.Fatal(err)
	}
	env := fa.env([]string{"PATH=x", "GIT_CONFIG_COUNT=2"})
	want := []string{
		"GIT_CONFIG_COUNT=5",
		"GIT_CONFIG_KEY_2=credential.helper", "GIT_CONFIG_VALUE_2=",
		"GIT_CONFIG_KEY_3=http.https://dev.azure.com/org/p/_git/r.extraHeader", "GIT_CONFIG_VALUE_3=Authorization: Bearer " + testToken,
		"GIT_CONFIG_KEY_4=http.https://dev.azure.com/org/p/_git/r.git.extraHeader", "GIT_CONFIG_VALUE_4=Authorization: Bearer " + testToken,
	}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("env:\n%s\nwant:\n%s", strings.Join(env, "\n"), strings.Join(want, "\n"))
	}
	basic := &jobs.GitAuth{Scheme: jobs.GitAuthBasic, Username: "me", Secret: []byte(testToken)}
	fa, err = newFetchAuth("https://example.com/r.git/", basic, "")
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.StdEncoding.EncodeToString([]byte("me:" + testToken))
	env = fa.env(nil)
	if len(env) != 5 || env[3] != "GIT_CONFIG_KEY_1=http.https://example.com/r.git.extraHeader" ||
		env[4] != "GIT_CONFIG_VALUE_1=Authorization: Basic "+enc {
		t.Fatalf("basic env: %q", env)
	}
	if got := fa.redact("x " + testToken + " y " + enc + " z"); got != "x *** y *** z" {
		t.Fatalf("redact: %q", got)
	}
	for _, u := range []string{"http://example.com/r", "git@github.com:o/r", "file:///tmp/r"} {
		if _, err := newFetchAuth(u, bearer(testToken), ""); err == nil {
			t.Errorf("%s: forwarded credential accepted", u)
		}
	}
	if fa, err := newFetchAuth("https://example.com/r", nil, ""); err != nil || fa.env(nil) != nil {
		t.Fatalf("no credential: %v %v", fa, err)
	}
}

// TestRedactAuthHeader: Authorization header values are masked in any
// output (git's curl traces, git-lfs errors).
func TestRedactAuthHeader(t *testing.T) {
	for in, want := range map[string]string{
		"> Authorization: Bearer eyJ0eXAi.abc.def":   "> Authorization: Bearer xxxxx",
		`extraheader "AUTHORIZATION: basic eDp5"`:    `extraheader "AUTHORIZATION: basic xxxxx"`,
		"authorization:token ghp_123 rest":           "authorization:token xxxxx rest",
		"Authorization: Bearer x https://u:p@h/r ok": "Authorization: Bearer xxxxx https://xxxxx@h/r ok",
	} {
		if got := RedactURLs(in); got != want {
			t.Errorf("RedactURLs(%q) = %q, want %q", in, got, want)
		}
	}
}

// gitRemote makes a bare repository proj.git under srvRoot with two
// commits on main (the first is no branch tip) and an LFS file in the
// second; it returns the commits and the LFS content.
func gitRemote(t *testing.T, srv *gitserver.Server) (c1, c2 string, lfsContent []byte) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	os.MkdirAll(src, 0o755)
	run(t, src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\n"), 0o644)
	run(t, src, "add", ".")
	run(t, src, "commit", "-qm", "c1")
	c1 = run(t, src, "rev-parse", "HEAD")
	lfsContent = []byte("large binary content\x00\x01\x02 of an LFS file\n")
	srv.AddLFS(lfsContent)
	os.WriteFile(filepath.Join(src, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644)
	os.WriteFile(filepath.Join(src, "big.bin"), gitserver.LFSPointer(lfsContent), 0o644)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("two\n"), 0o644)
	// the pointer is committed as is (no clean filter in this repository)
	run(t, src, "-c", "filter.lfs.clean=", "-c", "filter.lfs.process=", "-c", "filter.lfs.required=false", "add", ".")
	run(t, src, "commit", "-qm", "c2")
	c2 = run(t, src, "rev-parse", "HEAD")
	run(t, srv.Root, "clone", "-q", "--bare", src, filepath.Join(srv.Root, "proj.git"))
	return c1, c2, lfsContent
}

// TestFetchForwarded: against an https remote that requires a token, a
// fetch without it fails with an explanation, with it works (a branch, a
// commit that is no branch tip, Workspace.Fetch's FetchCommits, Git LFS
// content through git-lfs), and the token is nowhere in the mirror, the
// worktree's git config, the log or the errors.
func TestFetchForwarded(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_SSL_NO_VERIFY", "true") // the test server's certificate
	srv := gitserver.New(t, t.TempDir(), testToken)
	c1, c2, lfsContent := gitRemote(t, srv)
	u := srv.RepoURL("proj")
	ctx := context.Background()
	root := t.TempDir()
	s := &Store{Root: root}
	var log bytes.Buffer

	// none: the worker can't prompt
	if _, err := s.FetchURL(ctx, "proj", u, "main", "", &log); err == nil || !strings.Contains(err.Error(), "could not authenticate") {
		t.Fatalf("fetch without a credential: %v", err)
	}
	// a wrong one: refused, explained, masked
	wrong := "wrong-token-0123456789"
	_, err := s.FetchURLAuth(ctx, "proj", u, "main", bearer(wrong), "", &log)
	if err == nil || !strings.Contains(err.Error(), "forwarded credential was refused") || strings.Contains(err.Error(), wrong) {
		t.Fatalf("fetch with a wrong credential: %v", err)
	}
	// a commit that is no branch tip, by id
	got, err := s.FetchURLAuth(ctx, "proj", u, c1, bearer(testToken), "", &log)
	if err != nil || got != c1 {
		t.Fatalf("fetch %s = %s %v\n%s", c1, got, err, log.String())
	}
	// in the mirror already: no request at all
	before := srv.Stats()
	if got, err := s.FetchURLAuth(ctx, "proj", u, c1, nil, "", &log); err != nil || got != c1 || srv.Stats() != before {
		t.Fatalf("cached commit: %s %v %+v", got, err, srv.Stats())
	}
	// a branch, basic auth (git credential fill's kind)
	basic := &jobs.GitAuth{Scheme: jobs.GitAuthBasic, Username: "u", Secret: []byte(testToken)}
	if got, err := s.FetchURLAuth(ctx, "proj", u, "main", basic, "", &log); err != nil || got != c2 {
		t.Fatalf("fetch main = %s %v", got, err)
	}
	// FetchCommits (remote-first shipping)
	present, err := s.FetchCommits(ctx, "other", u, []string{c1}, bearer(testToken), &log)
	if err != nil || len(present) != 1 || present[0] != c1 {
		t.Fatalf("FetchCommits = %v %v", present, err)
	}
	if _, err := s.FetchCommits(ctx, "other", u, []string{c2}, bearer("nope-nope-nope"), &log); err == nil {
		t.Fatal("FetchCommits with a wrong credential worked")
	}

	// PrepareJob with --lfs fetch: git-lfs gets the header too
	lfs := exec.Command("git", "lfs", "version").Run() == nil
	spec := jobs.Spec{ID: "J1", Workspace: &jobs.Workspace{Repo: "lfsrepo", URL: u, Ref: c2, LFSFetch: lfs}}
	auth := bearer(testToken)
	dir, err := s.PrepareJob(ctx, "", spec, nil, auth, &log)
	if err != nil {
		t.Fatalf("PrepareJob: %v\n%s", err, log.String())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "big.bin"))
	if lfs {
		if !bytes.Equal(b, lfsContent) || srv.Stats().LFSDownloads == 0 {
			t.Fatalf("big.bin = %q (LFS stats %+v)\n%s", b, srv.Stats(), log.String())
		}
		if st := run(t, dir, "status", "--porcelain"); st != "" {
			t.Fatalf("worktree not clean: %s", st)
		}
	} else if !bytes.Equal(b, gitserver.LFSPointer(lfsContent)) {
		t.Fatalf("big.bin = %q", b)
	}

	// the token is nowhere on disk, in the log or the worktree's git config
	enc := base64.StdEncoding.EncodeToString([]byte("u:" + testToken))
	for _, needle := range []string{testToken, enc} {
		if strings.Contains(log.String(), needle) {
			t.Fatalf("credential in the log:\n%s", log.String())
		}
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(needle)) {
					t.Errorf("credential on disk in %s", p)
				}
			}
			return nil
		})
	}
	if cfg := run(t, dir, "config", "--list", "--show-origin"); strings.Contains(cfg, testToken) || strings.Contains(strings.ToLower(cfg), "extraheader") {
		t.Fatalf("worktree git config:\n%s", cfg)
	}
}
