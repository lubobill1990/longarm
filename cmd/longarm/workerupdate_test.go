package main

import (
	"testing"

	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func TestWorkerUpdateOutput(t *testing.T) {
	for code, want := range map[string]int{"refused": exitUsage, "unsupported": exitUsage, "invalid": exitUsage,
		"not_found": exitNotFound, "busy": exitTempFail, "rate_limited": exitTempFail, "denied": exitNoPerm,
		"unavailable": exitLongarmError, "failed": exitLongarmError, "": exitLongarmError} {
		if got := updateCodeExit(code); got != want {
			t.Errorf("%q: exit %d, want %d", code, got, want)
		}
	}
	for _, tc := range []struct {
		o    updateOutcome
		want string
	}{
		{updateOutcome{Jobs: 2, KeptJobs: 2, Sessions: 1, KeptSessions: 1}, "2 jobs and 1 session kept running"},
		{updateOutcome{Jobs: 1, KeptJobs: 1}, "1 job kept running"},
		{updateOutcome{Jobs: 3, KeptJobs: 2, Sessions: 2, KeptSessions: 2}, "2 of 3 jobs and 2 sessions kept running"},
	} {
		if got := keptNote(&tc.o); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.o, got, tc.want)
		}
	}
	if err := errTooOld("w1", "0.1.5"); exitCodeFor(err, true) != exitUsage {
		t.Errorf("too old: exit %d", exitCodeFor(err, true))
	}
}

// TestAdminFallback: a user-session worker that refused to update itself
// (an older longarmd that missed the Longarm Update service) hands over to
// its admin worker; nothing else does.
func TestAdminFallback(t *testing.T) {
	unsupported := &pb.MachineUpdateEvent{Phase: "failed", Method: "self", Code: "unsupported"}
	nodes := []*pb.NodeInfo{
		{Name: "cpc-1", Os: "windows", Companion: "cpc-1-admin"},
		{Name: "cpc-1-admin", Os: "windows", AdminWorker: true, Companion: "cpc-1"},
		{Name: "cpc-2", Os: "windows"}, // companion unknown (the slim cache): matched by name
		{Name: "cpc-2-admin", Os: "windows", Labels: map[string]string{sched.AdminLabel: "true"}},
		{Name: "cpc-3", Os: "windows"},
		{Name: "lnx", Os: "linux", Companion: "lnx-admin"},
		{Name: "lnx-admin", Os: "linux", AdminWorker: true},
	}
	for _, tc := range []struct {
		worker string
		last   *pb.MachineUpdateEvent
		want   string
	}{
		{"cpc-1", unsupported, "cpc-1-admin"},
		{"cpc-2", unsupported, "cpc-2-admin"},
		{"cpc-3", unsupported, ""},       // no admin worker
		{"cpc-1-admin", unsupported, ""}, // the admin worker itself
		{"lnx", unsupported, ""},         // the service is Windows-only
		{"nobody", unsupported, ""},
		{"cpc-1", &pb.MachineUpdateEvent{Phase: "failed", Method: "msi", Code: "unsupported"}, ""}, // the service said no
		{"cpc-1", &pb.MachineUpdateEvent{Phase: "failed", Method: "self", Code: "refused"}, ""},    // a downgrade
		{"cpc-1", &pb.MachineUpdateEvent{Phase: "installing", Method: "msi"}, ""},
		{"cpc-1", nil, ""},
	} {
		if got := adminFallback(tc.worker, nodes, tc.last); got != tc.want {
			t.Errorf("%s %v: %q, want %q", tc.worker, tc.last, got, tc.want)
		}
	}
}
