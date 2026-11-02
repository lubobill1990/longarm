package longarmd

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/updatesvc"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type reports struct {
	mu  sync.Mutex
	got []*pb.UpdateStatus
}

func (r *reports) add(s *pb.UpdateStatus, _ bool) {
	r.mu.Lock()
	r.got = append(r.got, s)
	r.mu.Unlock()
}
func (r *reports) list() []*pb.UpdateStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*pb.UpdateStatus(nil), r.got...)
}

// The service's install, whoever started it, reaches the hub: its phases
// with the download's progress, then the result (a 3010 as a restart
// pending); the hello carries the latest.
func TestWatchServiceReports(t *testing.T) {
	var mu sync.Mutex
	st := &updatesvc.Status{Installed: "0.1.29", State: updatesvc.PhaseDownloading, Target: "0.1.30", Started: time.Now(),
		Attempt: "svc-1", Initiator: updatesvc.InitiatorAuto, BytesDone: 12 << 20, BytesTotal: 42 << 20}
	svc := &fakeService{resp: func(r *updatesvc.Request) *updatesvc.Response {
		mu.Lock()
		defer mu.Unlock()
		c := *st
		return &updatesvc.Response{OK: true, Status: &c}
	}}
	m := testMachineUpdater(svc)
	rep := &reports{}
	m.report = rep.add
	since := time.Now().Add(-time.Hour)
	if !m.pollService(context.Background(), &since) {
		t.Fatal("not busy")
	}
	got := rep.list()
	if len(got) != 1 || got[0].Phase != "downloading" || got[0].BytesDone != 12<<20 || got[0].Initiator != "auto" || got[0].Method != "msi" ||
		got[0].Attempt != "svc-1" || got[0].To != "0.1.30" || got[0].State != "applying" {
		t.Fatalf("progress %v", got)
	}
	m.pollService(context.Background(), &since) // the same: no report
	if len(rep.list()) != 1 {
		t.Fatalf("repeated %v", rep.list())
	}
	mu.Lock()
	st.State = updatesvc.PhaseInstalling
	mu.Unlock()
	m.pollService(context.Background(), &since)
	if s := rep.list()[1]; s.Phase != "installing" || s.State != "restarting" {
		t.Fatalf("installing %v", s)
	}
	if h := m.serviceUpdate(); h.GetPhase() != "installing" {
		t.Fatalf("hello %v", h)
	}
	mu.Lock()
	st.State, st.Last = updatesvc.PhaseIdle, &updatesvc.Result{From: "0.1.29", To: "0.1.30", OK: true, Code: 3010, At: time.Now(), Attempt: "svc-1", Initiator: "auto"}
	mu.Unlock()
	if m.pollService(context.Background(), &since) {
		t.Fatal("busy after the result")
	}
	if s := rep.list()[2]; s.State != "ok" || !s.RebootRequired || s.Attempt != "svc-1" {
		t.Fatalf("result %v", s)
	}
	m.pollService(context.Background(), &since)
	if len(rep.list()) != 3 {
		t.Fatal("the result was reported twice")
	}
	// a failed installer: "error" with the code msi
	mu.Lock()
	st.Last = &updatesvc.Result{From: "0.1.29", To: "0.1.31", Error: "the installer failed (msiexec exit code 1603)", Code: 1603,
		Reason: updatesvc.CodeFailed, At: time.Now().Add(time.Second), Attempt: "svc-2"}
	mu.Unlock()
	m.pollService(context.Background(), &since)
	if s := rep.list()[3]; s.State != "error" || s.Code != "msi" || s.Error == "" {
		t.Fatalf("failure %v", s)
	}
}

// Edge case 1 (worker side): an owner's update refused before anything
// ran (a downgrade, a version the hub lacks) is reported to the hub;
// "busy" (edge case 5: an update runs already) is not - the running one
// stays shown.
func TestOwnerRefusalReported(t *testing.T) {
	svc := &fakeService{resp: func(r *updatesvc.Request) *updatesvc.Response {
		if r.Op == updatesvc.OpInstall {
			return &updatesvc.Response{Code: updatesvc.CodeRefused, Error: "0.1.28 is older than 0.1.29: refusing a downgrade"}
		}
		return &updatesvc.Response{OK: true, Status: &updatesvc.Status{State: updatesvc.PhaseIdle}}
	}}
	m := testMachineUpdater(svc)
	rep := &reports{}
	m.report = rep.add
	collect(t, m, &pb.MachineUpdateRequest{Version: "0.1.28"})
	got := rep.list()
	if len(got) != 1 || got[0].State != "error" || got[0].Code != "refused" || got[0].To != "0.1.28" || got[0].Initiator != "owner" {
		t.Fatalf("reports %v", got)
	}
	busy := &fakeService{resp: func(r *updatesvc.Request) *updatesvc.Response {
		if r.Op == updatesvc.OpInstall {
			return &updatesvc.Response{Code: updatesvc.CodeBusy, Error: "an update (to 0.1.30) is in progress"}
		}
		return &updatesvc.Response{OK: true, Status: &updatesvc.Status{State: updatesvc.PhaseIdle}}
	}}
	m2 := testMachineUpdater(busy)
	rep2 := &reports{}
	m2.report = rep2.add
	evs := collect(t, m2, &pb.MachineUpdateRequest{Version: "0.1.30"})
	if last := evs[len(evs)-1]; last.Code != "busy" || len(rep2.list()) != 0 {
		t.Fatalf("busy: %v, reports %v", last, rep2.list())
	}
}
