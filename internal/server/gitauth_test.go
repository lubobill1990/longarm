package server

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/testutil/gitserver"
	"github.com/lubobill1990/longarm/internal/userspace"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func gitT(t *testing.T, dir string, args ...string) string {
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

// waitJob waits for a terminal state and returns the job and its log.
func waitJobLog(t *testing.T, jc pb.JobsClient, id string) (*pb.Job, string) {
	t.Helper()
	st, err := jc.Wait(cctx(t), &pb.WaitRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	var j *pb.Job
	for {
		if j, err = st.Recv(); err != nil {
			t.Fatal(err)
		}
		if j.State != pb.JobState_JOB_STATE_PREPARING && j.State != pb.JobState_JOB_STATE_RUNNING &&
			j.State != pb.JobState_JOB_STATE_QUEUED {
			break
		}
	}
	var out bytes.Buffer
	ls, err := jc.Logs(cctx(t), &pb.LogsRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	for {
		c, err := ls.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out.Write(c.Data)
	}
	return j, out.String()
}

// TestSubmitGitAuth: a credential forwarded with the submit fetches the
// workspace from an https remote that requires it (a commit that is no
// branch tip, by id) and goes nowhere else - not the job's environment,
// job.json, the API record, the audit log, the job's log or any file of
// the worker; without it the job fails explaining why. Workspace.Fetch
// (remote-first shipping) uses it the same way.
func TestSubmitGitAuth(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_SSL_NO_VERIFY", "true") // the test server's certificate
	const token = "tok-SERVER-forwarded-7f3a9c"
	srv := gitserver.New(t, t.TempDir(), token)
	src := filepath.Join(t.TempDir(), "src")
	os.MkdirAll(src, 0o755)
	gitT(t, src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\n"), 0o644)
	gitT(t, src, "add", ".")
	gitT(t, src, "commit", "-qm", "c1")
	c1 := gitT(t, src, "rev-parse", "HEAD")
	gitT(t, src, "commit", "-q", "--allow-empty", "-m", "c2")
	gitT(t, srv.Root, "clone", "-q", "--bare", src, filepath.Join(srv.Root, "proj.git"))
	u := srv.RepoURL("proj")

	var store *workspace.Store
	e := newAuthzEnvWith(t, func(c *userspace.Config) {
		store = &workspace.Store{Root: c.DataDir}
		c.Prepare = store.PrepareJob
	})
	info, err := pb.NewNodeClient(e.alice).Info(cctx(t), &pb.InfoRequest{})
	if err != nil || !slices.Contains(info.Features, jobs.FeatureGitAuth) {
		t.Fatalf("features %v: %v", info.GetFeatures(), err)
	}
	exe, _ := os.Executable()
	jc := pb.NewJobsClient(e.alice)
	submit := func(id, ref string, auth *pb.GitAuth) (*pb.Job, string) {
		t.Helper()
		_, err := jc.Submit(cctx(t), &pb.SubmitRequest{GitAuth: auth, Spec: &pb.JobSpec{Id: id, Argv: []string{exe},
			Env: map[string]string{"LONGARM_TEST_ENV_DUMP": "1"}, Workspace: &pb.JobWorkspace{Repo: "proj", Url: u, Ref: ref}}})
		if err != nil {
			t.Fatal(err)
		}
		return waitJobLog(t, jc, id)
	}

	// without a credential: fails before it runs, saying why
	j, log := submit("GA0", c1, nil)
	if j.State != pb.JobState_JOB_STATE_FAILED || !strings.Contains(j.Reason, "could not authenticate") {
		t.Fatalf("no credential: %s %q\n%s", j.State, j.Reason, log)
	}
	// with it: the job runs on c1, and its environment has no credential
	j, log = submit("GA1", c1, &pb.GitAuth{Scheme: jobs.GitAuthBearer, Secret: []byte(token)})
	if j.State != pb.JobState_JOB_STATE_DONE || !strings.Contains(log, "with the forwarded credential") {
		t.Fatalf("forwarded: %s %q\n%s", j.State, j.Reason, log)
	}
	if !j.Spec.Workspace.GitAuth {
		t.Fatal("job record does not say the fetch was authenticated")
	}
	if got := gitT(t, store.JobWorktree("GA1"), "rev-parse", "HEAD"); got != c1 {
		t.Fatalf("worktree at %s, want %s", got, c1)
	}
	if strings.Contains(log, token) || strings.Contains(strings.ToLower(log), "extraheader") ||
		strings.Contains(log, "GIT_CONFIG_") {
		t.Fatalf("credential (or its config) in the job's log / environment:\n%s", log)
	}
	if cfg := gitT(t, store.JobWorktree("GA1"), "config", "--list"); strings.Contains(cfg, token) ||
		strings.Contains(strings.ToLower(cfg), "extraheader") {
		t.Fatalf("worktree git config:\n%s", cfg)
	}
	if strings.Contains(j.String(), token) {
		t.Fatal("credential in the API record")
	}
	// a wrong one: refused, never echoed (a branch: a commit in the mirror
	// already is not fetched again)
	j, log = submit("GA2", "main", &pb.GitAuth{Scheme: jobs.GitAuthBasic, Username: "u", Secret: []byte("wrong-secret-123")})
	if j.State != pb.JobState_JOB_STATE_FAILED || !strings.Contains(j.Reason, "forwarded credential was refused") ||
		strings.Contains(j.Reason+log, "wrong-secret-123") {
		t.Fatalf("wrong credential: %s %q\n%s", j.State, j.Reason, log)
	}

	// git_auth needs a workspace url; a credential needs a scheme
	for _, req := range []*pb.SubmitRequest{
		{GitAuth: &pb.GitAuth{Scheme: "bearer", Secret: []byte(token)}, Spec: &pb.JobSpec{Id: "GA3", Argv: []string{exe}}},
		{GitAuth: &pb.GitAuth{Scheme: "magic", Secret: []byte(token)}, Spec: &pb.JobSpec{Id: "GA4", Argv: []string{exe},
			Workspace: &pb.JobWorkspace{Repo: "proj", Url: u}}},
	} {
		if _, err := jc.Submit(cctx(t), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: %v", req.Spec.Id, err)
		}
	}

	// Workspace.Fetch
	wc := pb.NewWorkspaceClient(e.alice)
	res, err := wc.Fetch(cctx(t), &pb.FetchRequest{Repo: "proj2", Url: u, Commits: []string{c1},
		GitAuth: &pb.GitAuth{Scheme: jobs.GitAuthBearer, Secret: []byte(token)}})
	if err != nil || len(res.Present) != 1 {
		t.Fatalf("Fetch: %v %v", res, err)
	}
	if _, err := wc.Fetch(cctx(t), &pb.FetchRequest{Repo: "proj2", Url: u, Commits: []string{c1}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Fetch without a credential: %v", err)
	}
	_, err = wc.Fetch(cctx(t), &pb.FetchRequest{Repo: "proj3", Url: u, Commits: []string{c1},
		GitAuth: &pb.GitAuth{Scheme: jobs.GitAuthBearer, Secret: []byte("wrong-secret-456")}})
	if status.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), "wrong-secret-456") {
		t.Fatalf("Fetch with a wrong credential: %v", err)
	}

	time.Sleep(100 * time.Millisecond) // the audit line follows the reply
	audit, _ := os.ReadFile(e.srv.Audit.path)
	if !strings.Contains(string(audit), `"git_auth":true`) || !strings.Contains(string(audit), pb.Workspace_Fetch_FullMethodName) {
		t.Errorf("not audited:\n%s", audit)
	}
	root := filepath.Dir(e.srv.Audit.path)
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(token)) || bytes.Contains(b, []byte("wrong-secret")) {
				t.Errorf("credential on disk in %s", p)
			}
		}
		return nil
	})
}
