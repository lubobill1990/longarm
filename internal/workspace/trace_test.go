package workspace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/testutil/gitserver"
)

func TestGitEnv(t *testing.T) {
	base := []string{"PATH=/bin", "GIT_TRACE=1", "git_curl_verbose=0", "Git_Trace_Curl=1", "GIT_TRACE_PACKET=2",
		"GIT_TRACE2_EVENT=/tmp/e", "GIT_TRANSFER_TRACE=1", "GIT_TRACE_REDACT=0", "GIT_TRACE_SHALLOW=1",
		"GIT_TRACE_PACKFILE=/tmp/p", "GIT_DIR_X=keep", "GITTRACE=keep"}
	got := strings.Join(gitEnv(base), "\n")
	want := "PATH=/bin\nGIT_DIR_X=keep\nGITTRACE=keep\nGIT_TRACE2=0\nGIT_TRACE2_EVENT=0\nGIT_TRACE2_PERF=0\nGIT_TRACE_REDACT=1"
	if got != want {
		t.Fatalf("gitEnv:\n%s\nwant:\n%s", got, want)
	}
	GitTrace = true
	defer func() { GitTrace = false }()
	got = strings.Join(gitEnv(base), "\n")
	if !strings.Contains(got, "git_curl_verbose=0") || !strings.Contains(got, "GIT_TRACE=1") ||
		strings.Contains(got, "GIT_TRACE_REDACT=0") || !strings.HasSuffix(got, "GIT_TRACE_REDACT=1") {
		t.Fatalf("gitEnv with git_trace:\n%s", got)
	}
}

func TestScrubOutput(t *testing.T) {
	in := "remote: ok\n" +
		"02:00:00.1 http.c:845 => Send header: Authorization: Basic dXNlcjpzZWNyZXQtdmFsdWU=\n" +
		"02:00:00.1 http.c:845 => Send header: authorization: <redacted>\n" +
		"02:00:00.2 http.c:992 <= Recv header: Set-Cookie: session=abcdef\n" +
		"02:00:00.2 http.c:992 <= Recv header: proxy-authorization: x\n" +
		"02:00:00.2 http.c:992 <= Recv header: Cookie: a=b\n" +
		"02:00:00.3 http.c:992 <= Recv header: WWW-Authenticate: Basic realm=\"x\"\n" +
		"error: token Bearer eyJ0eXAiOiJKV1QiLCJhbGciOi.payload.sig rejected\n" +
		"fatal: https://user:pw@host/r\n"
	got := scrubOutput(in, nil)
	want := "remote: ok\n" +
		"02:00:00.3 http.c:992 <= Recv header: WWW-Authenticate: Basic realm=\"x\"\n" +
		"error: token Bearer xxxxx rejected\n" +
		"fatal: https://xxxxx@host/r\n"
	if got != want {
		t.Fatalf("scrubOutput:\n%s\nwant:\n%s", got, want)
	}
	var b bytes.Buffer
	w := &redactWriter{w: &b}
	w.Write([]byte(in))
	w.flush()
	if b.String() != want {
		t.Fatalf("redactWriter:\n%s", b.String())
	}
}

// traceLine matches git / curl / git-lfs trace output.
var traceLine = regexp.MustCompile(`(?m)(http\.c:\d+|=> Send header|<= Recv header|== Info:|^\S+ trace\S* |\btrace: |packet: |trace git-lfs)`)

// TestFetchIgnoresInheritedTrace: longarmd started with git trace variables
// in its environment (any case) - and git's own trap, GIT_CURL_VERBOSE=0,
// which turns curl tracing on because it is set - runs git and git-lfs
// without traces: the job's log and the errors have no trace lines, no
// response headers and no credential.
func TestFetchIgnoresInheritedTrace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	srv := gitserver.New(t, t.TempDir(), testToken)
	c1, c2, _ := gitRemote(t, srv)
	u := srv.RepoURL("proj")

	// what made the trace: a set GIT_CURL_VERBOSE, whatever its value
	t.Setenv("GIT_CURL_VERBOSE", "0")
	raw, _ := exec.Command("git", "ls-remote", u, "HEAD").CombinedOutput()
	t.Logf("git ls-remote with GIT_CURL_VERBOSE=0 printed %d trace lines", len(traceLine.FindAllString(string(raw), -1)))

	for k, v := range map[string]string{"GIT_TRACE": "1", "GIT_TRACE_CURL": "1", "GIT_TRACE_PACKET": "1",
		"GIT_TRACE_SETUP": "1", "GIT_TRACE_PERFORMANCE": "1", "GIT_TRANSFER_TRACE": "1", "GIT_TRACE_REDACT": "0"} {
		t.Setenv(k, v)
	}
	ctx := context.Background()
	s := &Store{Root: t.TempDir()}
	var log bytes.Buffer
	// fetch (by id), ls-remote (a cached commit), a refused credential,
	// and the job's preparation (worktree, checkout, LFS)
	if _, err := s.FetchURLAuth(ctx, "proj", u, c1, bearer(testToken), "", &log); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FetchURLAuth(ctx, "proj", u, c1, bearer(testToken), "", &log); err != nil {
		t.Fatal(err)
	}
	_, err := s.FetchURLAuth(ctx, "proj2", u, "main", bearer("refused-credential-xyz"), "", &log)
	if err == nil {
		t.Fatal("wrong credential accepted")
	}
	lfs := exec.Command("git", "lfs", "version").Run() == nil
	spec := jobs.Spec{ID: "T1", Workspace: &jobs.Workspace{Repo: "proj", URL: u, Ref: c2, LFSFetch: lfs}}
	if _, err := s.PrepareJob(ctx, "", spec, nil, bearer(testToken), &log); err != nil {
		t.Fatalf("PrepareJob: %v\n%s", err, log.String())
	}
	out := log.String() + "\n" + err.Error()
	if m := traceLine.FindAllString(out, 5); len(m) > 0 {
		t.Fatalf("trace output in the job log / error (%q):\n%s", m, out)
	}
	for _, needle := range []string{testToken, "refused-credential-xyz", "WWW-Authenticate", "Recv header"} {
		if strings.Contains(out, needle) {
			t.Fatalf("%q in the job log / error:\n%s", needle, out)
		}
	}
	noSecret(t, testToken, s.Root)

	// [workspace] git_trace = true: traces come back, still without
	// credential lines or the credential
	GitTrace = true
	defer func() { GitTrace = false }()
	log.Reset()
	if _, err := s.FetchURLAuth(ctx, "proj3", u, "main", bearer(testToken), "", &log); err != nil {
		t.Fatal(err)
	}
	if !traceLine.MatchString(log.String()) {
		t.Logf("no trace with git_trace (git printed none): %s", log.String())
	}
	if strings.Contains(log.String(), testToken) || sensitiveLine.MatchString(log.String()) {
		t.Fatalf("git_trace log has a credential line:\n%s", log.String())
	}
	if os.Getenv("GIT_TRACE") != "1" {
		t.Fatal("test environment changed")
	}
}
