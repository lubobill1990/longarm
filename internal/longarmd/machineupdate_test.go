package longarmd

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lubobill1990/longarm/internal/updatesvc"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type fakeService struct {
	mu   sync.Mutex
	reqs []*updatesvc.Request
	resp func(*updatesvc.Request) *updatesvc.Response
}

func (f *fakeService) Do(_ context.Context, r *updatesvc.Request) (*updatesvc.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.mu.Unlock()
	return f.resp(r), nil
}

func testMachineUpdater(svc *fakeService) *machineUpdater {
	m := newMachineUpdater("cpc-test", slog.New(slog.NewTextHandler(io.Discard, nil)), nil, "no self-update in this test")
	if svc != nil {
		m.service = func() serviceClient { return svc }
	} else {
		m.service = nil
	}
	return m
}

func collect(t *testing.T, m *machineUpdater, req *pb.MachineUpdateRequest) []*pb.MachineUpdateEvent {
	t.Helper()
	var evs []*pb.MachineUpdateEvent
	if err := m.Start(context.Background(), "alice", req, func(e *pb.MachineUpdateEvent) error {
		evs = append(evs, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return evs
}

// TestMachineUpdateForwardsToService: an MSI install hands the owner's
// request to the update service - version or latest, nothing else (no hub,
// no file) - and reports its answer.
func TestMachineUpdateForwardsToService(t *testing.T) {
	svc := &fakeService{resp: func(r *updatesvc.Request) *updatesvc.Response {
		if r.Op == updatesvc.OpInstall {
			return &updatesvc.Response{OK: true, Phase: updatesvc.PhaseInstalling, From: "0.1.8", To: "0.1.9", Message: "installing"}
		}
		return &updatesvc.Response{OK: true, Status: &updatesvc.Status{State: updatesvc.PhaseIdle}}
	}}
	m := testMachineUpdater(svc)
	evs := collect(t, m, &pb.MachineUpdateRequest{Latest: true})
	last := evs[len(evs)-1]
	if evs[0].Phase != "checking" || last.Phase != "installing" || last.To != "0.1.9" || last.Method != "msi" {
		t.Fatalf("events %v", evs)
	}
	r := svc.reqs[0]
	if r.Op != updatesvc.OpInstall || !r.Latest || r.Version != "" || !strings.Contains(r.Requester, "alice") {
		t.Fatalf("request %+v", r)
	}

	// refusals carry the service's code
	for code, resp := range map[string]*updatesvc.Response{
		"refused":      {Code: updatesvc.CodeRefused, Error: "refusing a downgrade"},
		"busy":         {Code: updatesvc.CodeBusy, Error: "in progress"},
		"rate_limited": {Code: updatesvc.CodeRateLimited, Error: "slow down"},
		"invalid":      {Code: updatesvc.CodeInvalid, Error: "not signed"},
	} {
		svc := &fakeService{resp: func(*updatesvc.Request) *updatesvc.Response { return resp }}
		evs := collect(t, testMachineUpdater(svc), &pb.MachineUpdateRequest{Version: "0.1.7"})
		if l := evs[len(evs)-1]; l.Phase != "failed" || l.Code != code {
			t.Errorf("%s: %v", code, l)
		}
	}

	// set-auto goes to the service with the owner named
	ev, err := m.SetAuto(context.Background(), "alice", &pb.MachineUpdateAutoRequest{Enabled: true, IntervalHours: 12})
	if err != nil || ev.Phase == "failed" {
		t.Fatalf("set-auto %v %v", ev, err)
	}
	r = svc.reqs[len(svc.reqs)-1]
	if r.Op != updatesvc.OpSetAuto || !r.Auto || r.IntervalHours != 12 {
		t.Fatalf("set-auto request %+v", r)
	}

	// no service, no self-updater: a clear refusal
	n := testMachineUpdater(nil)
	evs = collect(t, n, &pb.MachineUpdateRequest{Latest: true})
	if l := evs[len(evs)-1]; l.Phase != "failed" || l.Code != "unsupported" || l.Method != "self" {
		t.Fatalf("no updater: %v", l)
	}
	if ev, _ := n.SetAuto(context.Background(), "alice", &pb.MachineUpdateAutoRequest{Enabled: true}); ev.Code != "unsupported" {
		t.Fatalf("auto without the service: %v", ev)
	}
}

// TestNoRelayedPathToUpdateService: only machineupdate.go - behind the
// owner-only MachineUpdate RPC - asks the update service to install or to
// change automatic updates; the hub package doesn't know the service.
func TestNoRelayedPathToUpdateService(t *testing.T) {
	scan := func(dir string, allowed map[string]bool, needles ...string) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			n := e.Name()
			if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || allowed[n] {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range needles {
				if strings.Contains(string(b), s) {
					t.Errorf("%s/%s uses %s", dir, n, s)
				}
			}
		}
	}
	scan(".", map[string]bool{"machineupdate.go": true}, "updatesvc.OpInstall", "updatesvc.OpSetAuto")
	scan(filepath.Join("..", "hub"), nil, "internal/updatesvc", "MachineUpdate")
}
