package workspace

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/testutil/gitserver"
)

// noSecret fails when secret is in any file under the roots.
func noSecret(t *testing.T, secret string, roots ...string) {
	t.Helper()
	for _, root := range roots {
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(secret)) {
					t.Errorf("credential on disk in %s", p)
				}
			}
			return nil
		})
	}
}

func needLFS(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if exec.Command("git", "lfs", "version").Run() != nil {
		t.Skip("git-lfs not installed")
	}
}

// TestFetchLFSHostileConfig: a repository's .lfsconfig naming another LFS
// endpoint (lfs.url, remote.*.lfsurl) is ignored when a credential is
// forwarded - the endpoint is the fetched URL's - and that other host never
// gets the credential.
func TestFetchLFSHostileConfig(t *testing.T) {
	needLFS(t)
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	rec := &recorder{}
	evil := httptest.NewTLSServer(http.HandlerFunc(rec.handler))
	defer evil.Close()
	srv := gitserver.New(t, t.TempDir(), testToken)
	src := filepath.Join(t.TempDir(), "src")
	os.MkdirAll(src, 0o755)
	run(t, src, "init", "-q", "-b", "main")
	content := []byte("LFS content behind a hostile .lfsconfig\n")
	srv.AddLFS(content)
	os.WriteFile(filepath.Join(src, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644)
	os.WriteFile(filepath.Join(src, "big.bin"), gitserver.LFSPointer(content), 0o644)
	os.WriteFile(filepath.Join(src, ".lfsconfig"), []byte(fmt.Sprintf(
		"[lfs]\n\turl = %s/steal/info/lfs\n[remote \"origin\"]\n\tlfsurl = %s/steal2/info/lfs\n", evil.URL, evil.URL)), 0o644)
	run(t, src, "-c", "filter.lfs.clean=", "-c", "filter.lfs.process=", "-c", "filter.lfs.required=false", "add", ".")
	run(t, src, "commit", "-qm", "hostile")
	c := run(t, src, "rev-parse", "HEAD")
	run(t, srv.Root, "clone", "-q", "--bare", src, filepath.Join(srv.Root, "proj.git"))

	root := t.TempDir()
	s := &Store{Root: root}
	var log bytes.Buffer
	spec := jobs.Spec{ID: "H1", Workspace: &jobs.Workspace{Repo: "proj", URL: srv.RepoURL("proj"), Ref: c, LFSFetch: true}}
	dir, err := s.PrepareJob(context.Background(), "", spec, nil, bearer(testToken), &log)
	if err != nil {
		t.Fatalf("PrepareJob: %v\n%s", err, log.String())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "big.bin")); !bytes.Equal(b, content) {
		t.Fatalf("big.bin = %q\n%s", b, log.String())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.hits != 0 || len(rec.auths) != 0 {
		t.Fatalf("the .lfsconfig endpoint was asked %d times, with credentials %q", rec.hits, rec.auths)
	}
	noSecret(t, testToken, root)
}

// TestFetchLFSFailureLeavesNoCredential: git-lfs failing hard (its LFS
// directories are files) may write its environment and config to
// lfs/logs; the credential is in neither, and the logs are removed after
// the fetch.
func TestFetchLFSFailureLeavesNoCredential(t *testing.T) {
	needLFS(t)
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	const secret = "synthetic-LFS-fatal-credential-0042"
	srv := gitserver.New(t, t.TempDir(), secret)
	_, c2, _ := gitRemote(t, srv)
	root := t.TempDir()
	s := &Store{Root: root}
	var log bytes.Buffer
	if _, err := s.FetchURLAuth(context.Background(), "proj", srv.RepoURL("proj"), c2, bearer(secret), "", &log); err != nil {
		t.Fatal(err)
	}
	m := s.Mirror("proj")
	os.MkdirAll(filepath.Join(m, "lfs"), 0o755)
	os.WriteFile(filepath.Join(m, "lfs", "tmp"), []byte("not a directory"), 0o644)
	os.WriteFile(filepath.Join(m, "lfs", "objects"), []byte("not a directory"), 0o644)
	err := s.FetchLFS(context.Background(), "proj", srv.RepoURL("proj"), c2, bearer(secret), "", &log)
	t.Logf("lfs fetch (expected to fail): %v", err)
	if _, err := os.Stat(filepath.Join(m, "lfs", "logs")); err == nil {
		t.Error("lfs/logs left behind")
	}
	if strings.Contains(log.String(), secret) {
		t.Fatal("credential in the log")
	}
	noSecret(t, secret, root)
}
