package main

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	pb "longarm/proto/longarm/v1"
)

// "My jobs" lets the hub filter by submitter and limit (one small answer);
// a hub that ignores submitted_by gets asked again without the limit when
// its limited answer may have cut the caller's jobs off.
func TestHubJobsFilterAndOldHubFallback(t *testing.T) {
	me := "client/me"
	all := []*pb.FleetJob{}
	for i, by := range []string{"client/other", "client/other", me, "client/other", me} {
		all = append(all, &pb.FleetJob{Job: &pb.Job{Spec: &pb.JobSpec{Id: string(rune('a' + i)), SubmittedBy: by}}})
	}
	serve := func(newHub bool, asked *[]*pb.FleetJobsRequest) func(context.Context, *pb.FleetJobsRequest, ...grpc.CallOption) (*pb.FleetJobsResponse, error) {
		return func(_ context.Context, r *pb.FleetJobsRequest, _ ...grpc.CallOption) (*pb.FleetJobsResponse, error) {
			*asked = append(*asked, r)
			out := &pb.FleetJobsResponse{SubmittedByApplied: newHub && r.SubmittedBy != ""}
			for _, j := range all {
				if newHub && r.SubmittedBy != "" && j.Job.Spec.SubmittedBy != r.SubmittedBy {
					continue
				}
				out.Jobs = append(out.Jobs, j)
			}
			if r.Limit > 0 && len(out.Jobs) > int(r.Limit) {
				out.Jobs = out.Jobs[:r.Limit]
			}
			return out, nil
		}
	}
	mine := &pb.ClientJobsRequest{Mine: true, Limit: 2}

	var asked []*pb.FleetJobsRequest
	r, err := myHubJobs(context.Background(), serve(true, &asked), mine, nil, me)
	if err != nil || len(asked) != 1 || asked[0].SubmittedBy != me || asked[0].Limit != 2 || len(r.Jobs) != 2 {
		t.Fatalf("new hub: %v, asked %v, got %d jobs", err, asked, len(r.Jobs))
	}

	asked = nil
	r, err = myHubJobs(context.Background(), serve(false, &asked), mine, nil, me)
	if err != nil || len(asked) != 2 || asked[1].Limit != 0 || len(r.Jobs) != len(all) {
		t.Fatalf("old hub: %v, asked %v, got %d jobs", err, asked, len(r.Jobs))
	}

	// summary goes to the hub
	asked = nil
	if _, err := myHubJobs(context.Background(), serve(true, &asked), &pb.ClientJobsRequest{Mine: true, Summary: true}, nil, me); err != nil || len(asked) != 1 || !asked[0].Summary {
		t.Fatalf("summary: %v, asked %v", err, asked)
	}

	// not "mine": the limit goes to the hub, no submitter, one call
	asked = nil
	if _, err := myHubJobs(context.Background(), serve(false, &asked), &pb.ClientJobsRequest{Limit: 3}, nil, me); err != nil || len(asked) != 1 || asked[0].SubmittedBy != "" || asked[0].Limit != 3 {
		t.Fatalf("all jobs: %v, asked %v", err, asked)
	}
}
