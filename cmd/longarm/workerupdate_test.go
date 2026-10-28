package main

import (
	"testing"
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
