package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The stdin payload reaches the job and nothing else: not job.json, the
// job as the API returns it, the audit log, nor any file of the worker.
func TestSubmitStdinPayload(t *testing.T) {
	e := newAuthzEnv(t)
	info, err := pb.NewNodeClient(e.alice).Info(cctx(t), &pb.InfoRequest{})
	if err != nil || !slices.Contains(info.Features, jobs.FeatureStdin) {
		t.Fatalf("features %v: %v", info.GetFeatures(), err)
	}
	const marker = "PAYLOAD-0xC0FFEE-secret"
	payload := []byte(strings.Repeat("prompt "+marker+"\n", 2000)) // > a pipe buffer
	exe, _ := os.Executable()
	jc := pb.NewJobsClient(e.alice)
	j, err := jc.Submit(cctx(t), &pb.SubmitRequest{Stdin: payload,
		Spec: &pb.JobSpec{Id: "STDIN1", Argv: []string{exe}, Env: map[string]string{"LONGARM_TEST_STDIN_SHA": "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if j.Spec.StdinBytes != int64(len(payload)) {
		t.Fatalf("stdin_bytes = %d", j.Spec.StdinBytes)
	}
	st, err := jc.Wait(cctx(t), &pb.WaitRequest{Id: "STDIN1"})
	if err != nil {
		t.Fatal(err)
	}
	for {
		if j, err = st.Recv(); err != nil {
			t.Fatal(err)
		}
		if j.State == pb.JobState_JOB_STATE_DONE || j.State == pb.JobState_JOB_STATE_FAILED {
			break
		}
	}
	var out bytes.Buffer
	ls, err := jc.Logs(cctx(t), &pb.LogsRequest{Id: "STDIN1"})
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
	if want := fmt.Sprintf("%d %x\n", len(payload), sha256.Sum256(payload)); j.State != pb.JobState_JOB_STATE_DONE || out.String() != want {
		t.Fatalf("job %s read %q, want %q", j.State, out.String(), want)
	}
	if strings.Contains(j.String(), marker) {
		t.Fatal("payload in the job's API record")
	}
	time.Sleep(100 * time.Millisecond) // the audit line follows the reply
	audit, _ := os.ReadFile(e.srv.Audit.path)
	if !strings.Contains(string(audit), fmt.Sprintf(`"stdin_bytes":%d`, len(payload))) {
		t.Errorf("submit not audited with the payload size:\n%s", audit)
	}
	root := filepath.Dir(e.srv.Audit.path)
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(marker)) {
				t.Errorf("payload on disk in %s", p)
			}
		}
		return nil
	})

	// Over the limit: refused, no job.
	_, err = jc.Submit(cctx(t), &pb.SubmitRequest{Stdin: make([]byte, jobs.MaxStdinBytes+1),
		Spec: &pb.JobSpec{Id: "STDIN2", Argv: []string{exe}}})
	if err == nil || !strings.Contains(err.Error(), "InvalidArgument") {
		t.Fatalf("oversized payload: %v", err)
	}
	if _, err := jc.Get(context.Background(), &pb.GetJobRequest{Id: "STDIN2"}); err == nil {
		t.Fatal("oversized payload: job created")
	}
}
