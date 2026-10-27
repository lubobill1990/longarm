package server

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/userspace"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func untrackedTar(t *testing.T, name, content string) string {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write([]byte(content))
	tw.Close()
	p := filepath.Join(t.TempDir(), "untracked.tar")
	os.WriteFile(p, b.Bytes(), 0o644)
	return p
}

// TestResubmitOverlay: a resubmit of a job with a local checkout (a new
// upload, a new staging id) is the same job when the uploaded content is
// the same, and another job (AlreadyExists) when the uncommitted or
// untracked files differ - never the old result for new content.
func TestResubmitOverlay(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	var store *workspace.Store
	e := newAuthzEnvWith(t, func(c *userspace.Config) {
		store = &workspace.Store{Root: c.DataDir}
		c.Prepare = store.PrepareJob
	})
	src := filepath.Join(t.TempDir(), "src")
	os.MkdirAll(src, 0o755)
	gitT(t, src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\n"), 0o644)
	gitT(t, src, "add", ".")
	gitT(t, src, "commit", "-qm", "c1")
	c1 := gitT(t, src, "rev-parse", "HEAD")
	bundle := filepath.Join(t.TempDir(), "b.bundle")
	gitT(t, src, "bundle", "create", "-q", bundle, "main")
	if err := store.ImportBundle(cctx(t), "ov", bundle, "main", c1); err != nil {
		t.Fatal(err)
	}
	stage := func(id, content string) {
		t.Helper()
		if err := store.Stage(id, "", untrackedTar(t, "new.txt", content)); err != nil {
			t.Fatal(err)
		}
	}
	exe, _ := os.Executable()
	jc := pb.NewJobsClient(e.alice)
	submit := func(staging string) error {
		_, err := jc.Submit(cctx(t), &pb.SubmitRequest{Spec: &pb.JobSpec{Id: "OV1", Argv: []string{exe},
			Env: map[string]string{"LONGARM_TEST_ENV_DUMP": "1"}, Workspace: &pb.JobWorkspace{Repo: "ov", Commit: c1, StagingId: staging}}})
		return err
	}
	stage("01STAGEAAAAAAAAAAAAAAAAAAA", "content v1\n")
	if err := submit("01STAGEAAAAAAAAAAAAAAAAAAA"); err != nil {
		t.Fatal(err)
	}
	if j, log := waitJobLog(t, jc, "OV1"); j.State != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("job: %s %q\n%s", j.State, j.Reason, log)
	}
	// the same content again (a retry uploads anew): the same job
	stage("01STAGEBBBBBBBBBBBBBBBBBBB", "content v1\n")
	if err := submit("01STAGEBBBBBBBBBBBBBBBBBBB"); err != nil {
		t.Fatalf("identical resubmit: %v", err)
	}
	// other untracked content: not the old job's result
	stage("01STAGECCCCCCCCCCCCCCCCCCC", "content v2\n")
	if err := submit("01STAGECCCCCCCCCCCCCCCCCCC"); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("resubmit with other content: %v", err)
	}
	if a, b := store.StagingDigest("01STAGEBBBBBBBBBBBBBBBBBBB"), store.StagingDigest("01STAGECCCCCCCCCCCCCCCCCCC"); a == b || a == "" {
		t.Fatalf("digests %q %q", a, b)
	}
}
