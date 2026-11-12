package server

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"longarm/internal/jobs"
	pb "longarm/proto/longarm/v1"
)

func TestKillIfCreatedAt(t *testing.T) {
	e := newAuthzEnv(t)
	info, err := pb.NewNodeClient(e.alice).Info(cctx(t), &pb.InfoRequest{})
	if err != nil || !slices.Contains(info.GetFeatures(), jobs.FeatureKillIfCreatedAt) {
		t.Fatalf("conditional kill feature: %v %v", info, err)
	}
	client := pb.NewJobsClient(e.alice)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	job, err := client.Submit(cctx(t), &pb.SubmitRequest{Spec: &pb.JobSpec{
		Id: "KILLGEN", Argv: []string{exe}, Env: map[string]string{"LONGARM_TEST_SLEEP": "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		at   *timestamppb.Timestamp
		code codes.Code
	}{
		{"stale", timestamppb.New(job.CreatedAt.AsTime().Add(-time.Second)), codes.FailedPrecondition},
		{"zero", timestamppb.New(time.Time{}), codes.InvalidArgument},
		{"invalid", &timestamppb.Timestamp{Seconds: 1 << 60}, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.Kill(cctx(t), &pb.KillRequest{Id: job.Spec.Id, IfCreatedAt: tc.at}); status.Code(err) != tc.code {
				t.Fatalf("conditional kill: %v", err)
			}
			sp := e.srv.Spaces.Shared()
			current, err := sp.Jobs.Get(job.Spec.Id)
			if err != nil || current.Status.State.Terminal() {
				t.Fatalf("current generation affected: %+v %v", current, err)
			}
			if jobs.Dir(filepath.Join(sp.Root, "jobs", job.Spec.Id)).KillRequested() {
				t.Fatal("refused kill wrote a kill marker")
			}
		})
	}
	killed, err := client.Kill(cctx(t), &pb.KillRequest{Id: job.Spec.Id, IfCreatedAt: job.CreatedAt})
	if err != nil || killed.GetState() != pb.JobState_JOB_STATE_KILLED {
		t.Fatalf("matching generation: %v %v", killed, err)
	}
	sp := e.srv.Spaces.Shared()
	if err := sp.Jobs.RemoveGen(job.Spec.Id, job.CreatedAt.AsTime()); err != nil {
		t.Fatal(err)
	}
	replacement, err := client.Submit(cctx(t), &pb.SubmitRequest{Spec: job.Spec})
	if err != nil || replacement.GetCreatedAt().AsTime().Equal(job.CreatedAt.AsTime()) {
		t.Fatalf("replacement generation: %v %v", replacement, err)
	}
	if _, err := client.Kill(cctx(t), &pb.KillRequest{Id: job.Spec.Id, IfCreatedAt: job.CreatedAt}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("old generation after replacement: %v", err)
	}
	if jobs.Dir(filepath.Join(sp.Root, "jobs", job.Spec.Id)).KillRequested() {
		t.Fatal("old generation kill reached the replacement")
	}
	if killed, err := client.Kill(cctx(t), &pb.KillRequest{Id: job.Spec.Id}); err != nil || killed.GetState() != pb.JobState_JOB_STATE_KILLED {
		t.Fatalf("unconditional kill: %v %v", killed, err)
	}
}
