package workspace

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/testutil/gitserver"
)

// The test binary is git's credential helper too (longarmd's role).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == CredHelperArg {
		os.Exit(CredentialHelperMain(os.Args[2:], os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

const testToken = "forwarded-credential-for-tests"

func bearer(tok string) *jobs.GitAuth {
	return &jobs.GitAuth{Scheme: jobs.GitAuthBearer, Secret: []byte(tok)}
}

// askHelper runs the credential helper as git would.
func askHelper(t *testing.T, env []string, req string) string {
	t.Helper()
	cmd := exec.Command(HelperExe(), CredHelperArg, "get")
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(req)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	return string(out)
}

// TestFetchAuthEnv: the credential is not in git's environment at all;
// the environment names a credential helper (credential helpers of the
// worker off, the URL's path counts), refuses redirects, pins the LFS
// endpoint, turns traces off, and the helper answers only for the exact
// protocol, host and repository path. https only.
func TestFetchAuthEnv(t *testing.T) {
	fa, err := newFetchAuth("https://org@dev.azure.com/org/p/_git/r?x=1", bearer(testToken), "")
	if err != nil {
		t.Fatal(err)
	}
	defer fa.close()
	env := fa.env([]string{"PATH=x", "GIT_CONFIG_COUNT=2"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, testToken) {
		t.Fatalf("credential in the environment:\n%s", joined)
	}
	for _, want := range []string{"GIT_CONFIG_COUNT=7", "GIT_CONFIG_KEY_2=credential.helper\nGIT_CONFIG_VALUE_2=\n",
		"GIT_CONFIG_KEY_3=credential.useHttpPath\nGIT_CONFIG_VALUE_3=true",
		"GIT_CONFIG_KEY_4=credential.https://dev.azure.com.helper\nGIT_CONFIG_VALUE_4=!\"",
		"GIT_CONFIG_KEY_5=http.followRedirects\nGIT_CONFIG_VALUE_5=false",
		"GIT_CONFIG_KEY_6=lfs.url\nGIT_CONFIG_VALUE_6=https://dev.azure.com/org/p/_git/r.git/info/lfs",
		"LONGARM_GITCRED_KEY="} {
		if !strings.Contains(joined, want) {
			t.Errorf("env lacks %q:\n%s", want, joined)
		}
	}
	for req, ok := range map[string]bool{
		"protocol=https\nhost=dev.azure.com\npath=org/p/_git/r\n":              true,
		"protocol=https\nhost=dev.azure.com\npath=org/p/_git/r.git/info/lfs\n": true,
		"protocol=https\nhost=DEV.azure.com\npath=org/p/_git/r/info/refs\n":    true,
		"protocol=http\nhost=dev.azure.com\npath=org/p/_git/r\n":               false,
		"protocol=https\nhost=evil.example\npath=org/p/_git/r\n":               false,
		"protocol=https\nhost=dev.azure.com\npath=org/p/_git/other\n":          false,
		"protocol=https\nhost=dev.azure.com\npath=org/p/_git/r2\n":             false,
		"protocol=https\nhost=dev.azure.com\n":                                 false,
		"protocol=https\nhost=dev.azure.com:8443\npath=org/p/_git/r\n":         false,
	} {
		out := askHelper(t, env, req)
		if ok != (out == "username=x-access-token\npassword="+testToken+"\n") || (!ok && out != "") {
			t.Errorf("helper for %q: %q", req, out)
		}
	}
	// a wrong key gets nothing
	bad := append(append([]string(nil), env...), credKeyEnv+"=00")
	if out := askHelper(t, bad, "protocol=https\nhost=dev.azure.com\npath=org/p/_git/r\n"); out != "" {
		t.Errorf("wrong key: %q", out)
	}
	// closed: nothing
	fa.close()
	if out := askHelper(t, env, "protocol=https\nhost=dev.azure.com\npath=org/p/_git/r\n"); out != "" {
		t.Errorf("after the fetch: %q", out)
	}
	if got := fa.redact("x " + testToken + " z"); got != "x *** z" {
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

// recorder is a server that records the Authorization headers it gets.
type recorder struct {
	mu    sync.Mutex
	auths []string
	hits  int
}

func (r *recorder) handler(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.hits++
	if a := req.Header.Get("Authorization"); a != "" {
		r.auths = append(r.auths, a)
	}
	r.mu.Unlock()
	w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
	http.Error(w, "nope", http.StatusUnauthorized)
}

// TestFetchRedirectRefused: a remote that redirects - to another host, or
// from https to plain http - never gets the forwarded credential replayed
// to the target; the fetch fails saying why.
func TestFetchRedirectRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	srv := gitserver.New(t, t.TempDir(), testToken)
	c1, _, _ := gitRemote(t, srv)
	for name, mk := range map[string]func(h http.Handler) *httptest.Server{
		"cross-host":    httptest.NewTLSServer,
		"https-to-http": httptest.NewServer,
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			target := mk(http.HandlerFunc(rec.handler))
			defer target.Close()
			// the remote: redirects everything (also authenticated requests)
			redir := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.RequestURI(), http.StatusFound)
			}))
			defer redir.Close()
			s := &Store{Root: t.TempDir()}
			var log bytes.Buffer
			_, err := s.FetchURLAuth(context.Background(), "proj", redir.URL+"/proj.git", c1, bearer(testToken), "", &log)
			if err == nil || !strings.Contains(err.Error(), "redirect") {
				t.Fatalf("fetch through a redirect: %v\n%s", err, log.String())
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			for _, a := range rec.auths {
				t.Errorf("the redirect target got Authorization %q", a)
			}
			if strings.Contains(err.Error()+log.String(), testToken) {
				t.Fatal("credential in the error or log")
			}
		})
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
	// In the mirror already, but the mirror is shared between users: the
	// caller proves its own access (one ls-remote, no objects) - without a
	// credential, or with a wrong one, it is refused.
	if _, err := s.FetchURLAuth(ctx, "proj", u, c1, nil, "", &log); err == nil || !strings.Contains(err.Error(), "could not authenticate") {
		t.Fatalf("cached commit, no credential: %v", err)
	}
	if _, err := s.FetchURLAuth(ctx, "proj", u, c1, bearer("other-users-wrong-cred"), "", &log); err == nil ||
		!strings.Contains(err.Error(), "forwarded credential was refused") {
		t.Fatalf("cached commit, wrong credential: %v", err)
	}
	before := srv.Stats()
	if got, err := s.FetchURLAuth(ctx, "proj", u, c1, bearer(testToken), "", &log); err != nil || got != c1 {
		t.Fatalf("cached commit: %s %v", got, err)
	}
	if after := srv.Stats(); after.Authorized > before.Authorized+2 || after.UploadPackBytes-before.UploadPackBytes > 2048 {
		t.Fatalf("cached commit: more than an ls-remote: %+v -> %+v", before, after)
	}
	// per-user mirrors: no request at all
	private := &Store{Root: root, PrivateMirrors: true}
	before = srv.Stats()
	if got, err := private.FetchURLAuth(ctx, "proj", u, c1, nil, "", &log); err != nil || got != c1 || srv.Stats() != before {
		t.Fatalf("cached commit, private mirror: %s %v %+v", got, err, srv.Stats())
	}
	// FetchCommits of present commits proves access the same way
	if _, err := s.FetchCommits(ctx, "proj", u, []string{c1}, bearer("other-users-wrong-cred"), &log); err == nil {
		t.Fatal("FetchCommits of a present commit with a wrong credential worked")
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
	for _, needle := range []string{testToken} {
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

// TestPushHandoffExact: a handoff push writes exactly its branch - not the
// unpublished annotated tags that follow the commit with push.followTags
// on - and deletes it again; other namespaces and forced updates are
// refused.
func TestPushHandoffExact(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_SSL_NO_VERIFY", "true") // the test server's certificate
	srv := gitserver.New(t, t.TempDir(), testToken)
	c1, _, _ := gitRemote(t, srv)
	bare := filepath.Join(srv.Root, "proj.git")
	run(t, bare, "config", "http.receivepack", "true")
	src := filepath.Join(t.TempDir(), "work")
	noLFS := []string{"-c", "filter.lfs.process=", "-c", "filter.lfs.smudge=cat", "-c", "filter.lfs.clean=cat", "-c", "filter.lfs.required=false"}
	run(t, filepath.Dir(src), append(noLFS, "clone", "-q", bare, src)...)
	for i := 0; i < len(noLFS); i += 2 {
		k, v, _ := strings.Cut(noLFS[i+1], "=")
		run(t, src, "config", k, v)
	}
	run(t, src, "config", "push.followTags", "true")
	run(t, src, "tag", "-a", "-m", "unpublished", "v9-unpublished", c1)
	os.WriteFile(filepath.Join(src, "new.txt"), []byte("snapshot\n"), 0o644)
	run(t, src, "add", ".")
	run(t, src, "commit", "-qm", "snapshot")
	snap := run(t, src, "rev-parse", "HEAD")
	s := &Store{Root: t.TempDir()}
	ctx := context.Background()
	u := srv.RepoURL("proj")
	branch := "longarm/handoff/alice/abc12345-202610290000"
	if err := s.PushHandoff(ctx, src, u, snap, branch, bearer(testToken)); err != nil {
		t.Fatal(err)
	}
	refs := run(t, bare, "for-each-ref", "--format=%(refname)")
	if strings.Contains(refs, "refs/tags/") || !strings.Contains(refs, "refs/heads/"+branch) {
		t.Fatalf("the remote's refs after the push:\n%s", refs)
	}
	if err := s.PushHandoff(ctx, src, u, snap, "main", bearer(testToken)); err == nil {
		t.Error("a push outside longarm/handoff/ was accepted")
	}
	if err := s.PushHandoff(ctx, src, u, c1, branch, bearer(testToken)); err == nil {
		t.Error("a non-fast-forward update of the branch was accepted")
	}
	if err := s.PushHandoff(ctx, src, u, "", branch, bearer(testToken)); err != nil {
		t.Fatal(err)
	}
	if refs := run(t, bare, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, "longarm/") || strings.Contains(refs, "refs/tags/") {
		t.Fatalf("after the delete:\n%s", refs)
	}
}
