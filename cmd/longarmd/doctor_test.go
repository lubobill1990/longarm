package main

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/doctor"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type fakeWorkerEnv struct {
	id         pki.Identity
	notAfter   time.Time
	certErr    error
	status     *pb.WorkerStatus
	statusErr  error
	mode       string
	registered bool
	state      string
	logs       []string
	errs       []string
}

func (f *fakeWorkerEnv) Cert() (pki.Identity, time.Time, error) { return f.id, f.notAfter, f.certErr }
func (f *fakeWorkerEnv) Status(context.Context) (*pb.WorkerStatus, error) {
	return f.status, f.statusErr
}
func (f *fakeWorkerEnv) RunMode() (string, bool, string) { return f.mode, f.registered, f.state }
func (f *fakeWorkerEnv) Logs() []string                  { return f.logs }
func (f *fakeWorkerEnv) RecentErrors(time.Time) []string { return f.errs }

func byID(cs []doctor.Check, id string) doctor.Check {
	for _, c := range cs {
		if c.ID == id {
			return c
		}
	}
	return doctor.Check{}
}

func TestWorkerChecks(t *testing.T) {
	now := time.Now()
	healthy := func() *fakeWorkerEnv {
		return &fakeWorkerEnv{
			id: pki.Identity{Role: pki.RoleWorker, Name: "pc1"}, notAfter: now.Add(20 * 24 * time.Hour),
			status: &pb.WorkerStatus{Name: "pc1", Pid: 42, Version: "0.1.3", Hub: &pb.HubLink{Address: "hub:7443", Connected: true, NodeState: "approved"}},
			mode:   modeSession, registered: true, logs: []string{`C:\Users\a\.longarm\longarmd.log`},
		}
	}
	cs := workerChecks(context.Background(), healthy(), now)
	for _, id := range []string{"signed-in", "certificate", "running", "hub", "run-mode", "errors"} {
		if c := byID(cs, id); c.Status != doctor.OK {
			t.Errorf("%s: %+v", id, c)
		}
	}
	if c := byID(cs, "logs"); c.Status != doctor.Info || !strings.Contains(c.Detail, "longarmd.log") {
		t.Errorf("logs: %+v", c)
	}
	for _, b := range []struct {
		name string
		mod  func(*fakeWorkerEnv)
		id   string
		st   doctor.Status
		fix  string
	}{
		{"not enrolled", func(f *fakeWorkerEnv) { f.certErr = errors.New("no tls") }, "signed-in", doctor.Fail, "longarmd login"},
		{"expired", func(f *fakeWorkerEnv) { f.notAfter = now.Add(-time.Hour) }, "certificate", doctor.Fail, "longarmd login --force"},
		{"stopped (session)", func(f *fakeWorkerEnv) { f.statusErr = errors.New("not running") }, "running", doctor.Fail, "longarmd session-mode start"},
		{"stopped (service)", func(f *fakeWorkerEnv) {
			f.statusErr, f.mode, f.state = errors.New("not running"), modeService, string(platform.ServiceStopped)
		}, "run-mode", doctor.Fail, startCommand(modeService, runtime.GOOS)},
		{"task missing", func(f *fakeWorkerEnv) { f.registered = false }, "run-mode", doctor.Fail, "longarmd session-mode install"},
		{"by hand", func(f *fakeWorkerEnv) { f.mode = modeNone }, "run-mode", doctor.Warn, startByHandFix()},
		{"hub down", func(f *fakeWorkerEnv) { f.status.Hub = &pb.HubLink{Address: "hub:7443", Error: "timeout"} }, "hub", doctor.Fail, "longarmd diag --upload"},
		{"revoked", func(f *fakeWorkerEnv) { f.status.Hub = &pb.HubLink{Address: "hub:7443", Error: "certificate revoked"} }, "hub", doctor.Fail, "longarmd login --force"},
		{"pending", func(f *fakeWorkerEnv) { f.status.Hub.NodeState = "pending" }, "hub", doctor.Warn, "longarm admin approve pc1  # an admin"},
		{"errors", func(f *fakeWorkerEnv) { f.errs = []string{"time=x level=ERROR msg=boom"} }, "errors", doctor.Warn, "longarmd diag --upload"},
	} {
		f := healthy()
		b.mod(f)
		cs := workerChecks(context.Background(), f, now)
		if c := byID(cs, b.id); c.Status != b.st || c.Fix != b.fix {
			t.Errorf("%s: %+v", b.name, c)
		}
		for _, c := range cs {
			if (c.Status == doctor.Fail || c.Status == doctor.Warn) && c.Fix == "" {
				t.Errorf("%s: %s is %s without a next command", b.name, c.ID, c.Status)
			}
		}
	}
}

func TestStartCommandAndNotRunning(t *testing.T) {
	for _, c := range []struct{ mode, goos, want string }{
		{modeSession, "windows", "longarmd session-mode start"},
		{modeService, "windows", "longarmd service start  # elevated"},
		{modeService, "linux", "sudo longarmd service start"},
		{modeNone, "windows", "longarmd session-mode install  (or in the foreground: longarmd run)"},
		{modeNone, "linux", "sudo longarmd service install --user $USER  (or in the foreground: longarmd run)"},
	} {
		if got := startCommand(c.mode, c.goos); got != c.want {
			t.Errorf("%s/%s: %q", c.mode, c.goos, got)
		}
	}
	err := notRunningHelp(errors.New(`longarmd is not running (no C:\d\longarmd.local)`), modeSession,
		[]string{`C:\d\longarmd.log`, `C:\d\longarmd-session.log`}, "windows")
	want := "longarmd is not running (no C:\\d\\longarmd.local)\n" +
		"  start it: longarmd session-mode start\n" +
		"  logs: C:\\d\\longarmd.log, C:\\d\\longarmd-session.log\n" +
		"  checklist: longarmd doctor"
	if err.Error() != want {
		t.Errorf("not running:\n%s\nwant\n%s", err, want)
	}
}

func TestRecentErrors(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	log := "time=2026-10-26T10:00:00Z level=ERROR msg=old\n" +
		"time=2026-10-28T10:00:00Z level=INFO msg=fine\n" +
		"time=2026-10-28T10:01:00Z level=ERROR msg=\"hub: refused\"\n" +
		"time=2026-10-28T11:00:00.5+00:00 level=ERROR msg=late\n"
	got := recentErrors(strings.NewReader(log), now.Add(-24*time.Hour), 5)
	if len(got) != 2 || !strings.Contains(got[0], "refused") || !strings.Contains(got[1], "late") {
		t.Errorf("errors: %q", got)
	}
	if got := recentErrors(strings.NewReader(log), now.Add(-72*time.Hour), 1); len(got) != 1 || !strings.Contains(got[0], "late") {
		t.Errorf("max 1: %q", got)
	}
}
