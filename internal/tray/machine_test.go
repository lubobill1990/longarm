package tray

import (
	"context"
	"strings"
	"testing"
)

// TestQuitSummary: the confirmation of "Quit Longarm on this machine" says
// what stops and what is running.
func TestQuitSummary(t *testing.T) {
	s := QuitSummary(MachineState{Worker: true, SessionWorker: true, Jobs: 2, Sessions: 1, Forwards: 1})
	for _, want := range []string{"this machine's worker until you sign in", "client daemon", "2 jobs and 1 session", "1 port forward will stop"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
	s = QuitSummary(MachineState{Worker: true, Jobs: 3})
	if strings.Contains(s, "until you sign in") || !strings.Contains(s, "Windows service and keeps running") {
		t.Errorf("service worker:\n%s", s)
	}
	if s := QuitSummary(MachineState{}); strings.Contains(s, "worker") {
		t.Errorf("no worker:\n%s", s)
	}
}

// TestQuitLongarm: the session worker (until the next sign-in) and the
// client daemon are stopped; a service worker is left alone; a daemon that
// isn't running is no problem.
func TestQuitLongarm(t *testing.T) {
	s, err := NewServer(Options{LongarmConfigDir: t.TempDir(), LongarmdDataDir: t.TempDir(), LongarmExe: "longarm", LongarmdExe: "longarmd"}, &fakeDesk{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var ran []string
	s.runCmd = func(name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	if p := s.QuitLongarm(MachineState{Worker: true, SessionWorker: true}); len(p) != 0 {
		t.Fatalf("problems %v", p)
	}
	if len(ran) != 2 || ran[0] != "longarmd session-mode stop" || !strings.HasSuffix(ran[1], "daemon stop") {
		t.Fatalf("ran %q", ran)
	}
	ran = nil
	s.QuitLongarm(MachineState{Worker: true}) // the service: not ours to stop
	if len(ran) != 1 || !strings.HasSuffix(ran[0], "daemon stop") {
		t.Fatalf("service worker: ran %q", ran)
	}
	// nothing answers: no worker, no forwards
	if st := s.MachineState(context.Background()); st.Worker || st.Forwards != 0 {
		t.Fatalf("state without processes: %+v", st)
	}
}
