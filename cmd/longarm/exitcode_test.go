package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func TestExitCodeFor(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"job exit mirrored", &exitError{code: 7}, 7},
		{"usage", usagef("--env %q: want KEY=VALUE", "x"), exitUsage},
		{"wrapped usage", fmt.Errorf("run: %w", usagef("bad")), exitUsage},
		{"not found", notFoundf("job x not found"), exitNotFound},
		{"plain error", errors.New("boom"), exitLongarmError},
		{"InvalidArgument", status.Error(codes.InvalidArgument, "bad id"), exitUsage},
		{"FailedPrecondition", status.Error(codes.FailedPrecondition, "x is a directory"), exitUsage},
		{"AlreadyExists", status.Error(codes.AlreadyExists, "job id in use"), exitUsage},
		{"Unimplemented", status.Error(codes.Unimplemented, "old worker"), exitUsage},
		{"OutOfRange", status.Error(codes.OutOfRange, "offset"), exitUsage},
		{"NotFound", status.Error(codes.NotFound, "job x not known to the hub"), exitNotFound},
		{"ResourceExhausted", status.Error(codes.ResourceExhausted, "no worker passes the gate: ..."), exitTempFail},
		{"PermissionDenied", status.Error(codes.PermissionDenied, "admin only"), exitNoPerm},
		{"Unauthenticated", status.Error(codes.Unauthenticated, "no cert"), exitNoPerm},
		{"trust refusal", status.Error(codes.Unavailable, "worker w1: refused by end-to-end trust: not endorsed"), exitNoPerm},
		{"Unavailable", status.Error(codes.Unavailable, "connection refused"), exitLongarmError},
		{"DeadlineExceeded", status.Error(codes.DeadlineExceeded, "slow"), exitLongarmError},
		{"Internal", status.Error(codes.Internal, "oops"), exitLongarmError},
		{"wrapped status", fmt.Errorf("place: %w", status.Error(codes.ResourceExhausted, "quota")), exitTempFail},
	} {
		if got := exitCodeFor(c.err, true); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.name, got, c.want)
		}
	}
	// Before RunE: whatever cobra returns is a usage error.
	if got := exitCodeFor(errors.New(`unknown command "x" for "longarm"`), false); got != exitUsage {
		t.Errorf("cobra error: %d", got)
	}
}

func TestMarkUsageErrors(t *testing.T) {
	newRoot := func(runErr error) *cobra.Command {
		root := &cobra.Command{Use: "longarm", SilenceUsage: true, SilenceErrors: true}
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.AddCommand(&cobra.Command{Use: "wait", Args: cobra.RangeArgs(1, 2),
			RunE: func(*cobra.Command, []string) error { return runErr }})
		return root
	}
	for _, c := range []struct {
		args   []string
		runErr error
		want   int
	}{
		{[]string{"wait", "--no-such-flag", "x"}, nil, exitUsage},
		{[]string{"wait"}, nil, exitUsage},
		{[]string{"nope"}, nil, exitUsage},
		{[]string{"wait", "x"}, nil, 0},
		{[]string{"wait", "x"}, errors.New("connection refused"), exitLongarmError},
		{[]string{"wait", "x"}, status.Error(codes.NotFound, "no job"), exitNotFound},
	} {
		root := newRoot(c.runErr)
		var validated bool
		markUsageErrors(root, &validated)
		root.SetArgs(c.args)
		err := root.Execute()
		if got := exitCodeFor(err, validated); got != c.want {
			t.Errorf("%v: exit %d (%v), want %d", c.args, got, err, c.want)
		}
	}
}

func TestRunsAlready(t *testing.T) {
	const id = "01JOB"
	if w, ok := runsAlready(status.Errorf(codes.AlreadyExists, "job %s already runs on %s", id, "w1"), id); !ok || w != "w1" {
		t.Errorf("own job: %q %v", w, ok)
	}
	for _, err := range []error{
		status.Errorf(codes.AlreadyExists, "job id %s is in use", id), // somebody else's
		status.Errorf(codes.AlreadyExists, "job %s already runs on %s", "01OTHER", "w1"),
		status.Errorf(codes.ResourceExhausted, "job %s already runs on %s", id, "w1"),
		errors.New("job 01JOB already runs on w1"),
		nil,
	} {
		if w, ok := runsAlready(err, id); ok {
			t.Errorf("%v: %q", err, w)
		}
	}
}

func TestJobsJSON(t *testing.T) {
	rows := []*pb.FleetJob{{Worker: "w1", WorkerOnline: true, Job: &pb.Job{
		Spec: &pb.JobSpec{Id: "j1", Argv: []string{"x"}, StdinBytes: 12}, State: pb.JobState_JOB_STATE_RUNNING, LogSize: 5}}}
	var got struct {
		Jobs        []map[string]any `json:"jobs"`
		Source      string           `json:"source"`
		Unreachable []string         `json:"unreachable"`
	}
	if err := json.Unmarshal(jobsJSONBytes(rows, "hub", nil), &got); err != nil {
		t.Fatal(err)
	}
	// FleetJobsResponse's fields stay as they were (protojson names).
	if got.Source != "hub" || len(got.Jobs) != 1 || got.Jobs[0]["worker"] != "w1" || got.Jobs[0]["workerOnline"] != true ||
		got.Jobs[0]["job"].(map[string]any)["state"] != "JOB_STATE_RUNNING" || got.Jobs[0]["job"].(map[string]any)["logSize"] != "5" ||
		got.Unreachable != nil {
		t.Fatalf("hub: %+v", got)
	}
	// Empty: "jobs" is [] (it was omitted before), the fallback names the
	// workers that did not answer.
	b := jobsJSONBytes(nil, "workers", []string{"w2"})
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil || fmt.Sprint(raw["jobs"]) != "[]" || raw["source"] != "workers" ||
		fmt.Sprint(raw["unreachable"]) != "[w2]" {
		t.Fatalf("workers: %s", b)
	}
}
