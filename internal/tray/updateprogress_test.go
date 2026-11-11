package tray

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/updatesvc"
)

// TestProgressWatch: the window follows the service through the install -
// download, verification, msiexec (the service gone), its result - and
// says how it ended.
func TestProgressWatch(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	down := errors.New("pipe: not found")
	busy := func(phase string) *updatesvc.Status {
		return &updatesvc.Status{Installed: "0.1.34", State: phase, Target: "0.1.35", Attempt: "svc-1", BytesDone: 10 << 20, BytesTotal: 40 << 20}
	}

	t.Run("an install from start to end", func(t *testing.T) {
		w := newProgressWatch("0.1.35", t0)
		earlier := &updatesvc.Result{From: "0.1.33", To: "0.1.35", OK: false, Error: "an earlier try", At: t0.Add(-time.Hour), Attempt: "svc-0"}
		steps := []struct {
			st    *updatesvc.Status
			err   error
			phase string
		}{
			{&updatesvc.Status{Installed: "0.1.34", State: updatesvc.PhaseIdle, Last: earlier}, nil, ProgressWaiting},
			{busy(updatesvc.PhaseDownloading), nil, updatesvc.PhaseDownloading},
			{busy(updatesvc.PhaseVerifying), nil, updatesvc.PhaseVerifying},
			{busy(updatesvc.PhaseInstalling), nil, updatesvc.PhaseInstalling},
			{nil, down, ProgressRestarting},
			{&updatesvc.Status{Installed: "0.1.34", State: updatesvc.PhaseIdle, Last: earlier}, nil, ProgressRestarting},
			{&updatesvc.Status{Installed: "0.1.35", State: updatesvc.PhaseIdle, Last: &updatesvc.Result{From: "0.1.34", To: "0.1.35", OK: true, Code: 0, At: at(70), Attempt: "svc-1"}}, nil, ProgressDone},
		}
		for i, s := range steps {
			p := w.step(at(i*10), s.st, s.err)
			if p.Phase != s.phase || p.To != "0.1.35" {
				t.Fatalf("step %d: %+v, want %s", i, p, s.phase)
			}
			if i == 1 && (p.BytesTotal != 40<<20 || p.From != "0.1.34") {
				t.Fatalf("downloading: %+v", p)
			}
		}
		if p := w.step(at(62), nil, down); p.Phase != ProgressDone || p.CloseIn != 6 {
			t.Fatalf("done stays done, closing: %+v", p)
		}
		if p := w.step(at(70), nil, down); p.CloseIn != 0 {
			t.Fatalf("close_in counts down: %+v", p)
		}
	})

	t.Run("failed", func(t *testing.T) {
		w := newProgressWatch("0.1.35", t0)
		w.step(at(0), busy(updatesvc.PhaseDownloading), nil)
		p := w.step(at(5), &updatesvc.Status{Installed: "0.1.34", State: updatesvc.PhaseIdle,
			Last: &updatesvc.Result{To: "0.1.35", Error: "refusing the MSI: bad signature", At: at(5), Attempt: "svc-1"}}, nil)
		if p.Phase != ProgressFailed || !strings.Contains(p.Error, "signature") {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("an older service: no result, the new version installed", func(t *testing.T) {
		w := newProgressWatch("v0.1.35", t0)
		w.step(at(0), &updatesvc.Status{Installed: "0.1.34", State: updatesvc.PhaseInstalling, Target: "0.1.35"}, nil)
		w.step(at(5), nil, down)
		if p := w.step(at(60), &updatesvc.Status{Installed: "0.1.35", State: updatesvc.PhaseIdle}, nil); p.Phase != ProgressDone {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("back on the old version without a result", func(t *testing.T) {
		w := newProgressWatch("0.1.35", t0)
		w.step(at(0), busy(updatesvc.PhaseInstalling), nil)
		w.step(at(5), nil, down)
		idle := &updatesvc.Status{Installed: "0.1.34", State: updatesvc.PhaseIdle}
		if p := w.step(at(60), idle, nil); p.Phase != ProgressRestarting {
			t.Fatalf("%+v", p)
		}
		if p := w.step(at(60).Add(progressNoResult+time.Second), idle, nil); p.Phase != ProgressFailed || !strings.Contains(p.Error, "0.1.34") {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("never started", func(t *testing.T) {
		w := newProgressWatch("0.1.35", t0)
		idle := &updatesvc.Status{Installed: "0.1.34", State: updatesvc.PhaseIdle}
		if p := w.step(at(10), idle, nil); p.Phase != ProgressWaiting {
			t.Fatalf("%+v", p)
		}
		if p := w.step(t0.Add(progressNoStart+time.Second), idle, nil); p.Phase != ProgressFailed {
			t.Fatalf("%+v", p)
		}
		w = newProgressWatch("0.1.35", t0)
		if p := w.step(t0.Add(progressNoStart+time.Second), nil, down); p.Phase != ProgressFailed || !strings.Contains(p.Error, "doesn't answer") {
			t.Fatalf("no service: %+v", p)
		}
	})
}

// TestProgressHandler: the window's API, and the window closing itself
// once done (or on Close).
func TestProgressHandler(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	st := &updatesvc.Status{Installed: "0.1.34", State: updatesvc.PhaseDownloading, Target: "0.1.35", BytesDone: 1, BytesTotal: 2}
	quit := make(chan struct{}, 2)
	h := progressHandler("0.1.35", slog.New(slog.DiscardHandler), func() { quit <- struct{}{} },
		func(context.Context) (*updatesvc.Status, error) { mu.Lock(); defer mu.Unlock(); return st, nil },
		func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	get := func() ProgressStatus {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/trayapi/progress", nil))
		var p ProgressStatus
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("%v: %s", err, w.Body)
		}
		return p
	}
	if p := get(); p.Phase != updatesvc.PhaseDownloading || p.BytesTotal != 2 {
		t.Fatalf("%+v", p)
	}
	mu.Lock()
	st = &updatesvc.Status{Installed: "0.1.35", State: updatesvc.PhaseIdle, Last: &updatesvc.Result{To: "0.1.35", OK: true, At: now}}
	mu.Unlock()
	if p := get(); p.Phase != ProgressDone || p.CloseIn == 0 {
		t.Fatalf("%+v", p)
	}
	select {
	case <-quit:
		t.Fatal("closed before its time")
	default:
	}
	mu.Lock()
	now = now.Add(progressCloseAfter)
	mu.Unlock()
	get()
	select {
	case <-quit:
	case <-time.After(5 * time.Second):
		t.Fatal("not closed when done")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/trayapi/progress/close", strings.NewReader("{}")))
	if w.Code != 200 {
		t.Fatalf("close: %d", w.Code)
	}
	select {
	case <-quit:
		t.Fatal("quit twice")
	case <-time.After(50 * time.Millisecond):
	}
	// the page itself
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/update?to=0.1.35", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<div id=\"root\"") {
		t.Fatalf("page: %d", w.Code)
	}
}

type lockedService struct {
	mu   sync.Mutex
	st   *updatesvc.Status
	asks int
}

func (f *lockedService) Do(context.Context, *updatesvc.Request) (*updatesvc.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asks++
	return &updatesvc.Response{OK: true, Status: f.st}, nil
}

func (f *lockedService) set(st *updatesvc.Status) {
	f.mu.Lock()
	f.st = st
	f.mu.Unlock()
}

// TestWatchForInstall: the window starts once this machine's service runs
// the install the tray asked for - not for an automatic update, another
// version, or nothing.
func TestWatchForInstall(t *testing.T) {
	oldSvc, oldWithin, oldPoll := updateService, installShowsWithin, progressPoll
	defer func() { updateService, installShowsWithin, progressPoll = oldSvc, oldWithin, oldPoll }()
	progressPoll = 5 * time.Millisecond
	f := &lockedService{st: &updatesvc.Status{State: updatesvc.PhaseIdle}}
	updateService = func() serviceClient { return f }
	shown := make(chan string, 4)
	s := &Server{opts: Options{Log: slog.New(slog.DiscardHandler)}, UpdateProgress: func(v string) { shown <- v }}

	installShowsWithin = 100 * time.Millisecond
	f.set(&updatesvc.Status{State: updatesvc.PhaseDownloading, Target: "0.1.35", Initiator: updatesvc.InitiatorAuto})
	s.watchForInstall("")
	select {
	case v := <-shown:
		t.Fatalf("an automatic update got a window (%s)", v)
	case <-time.After(300 * time.Millisecond):
	}

	installShowsWithin = 5 * time.Second
	f.set(&updatesvc.Status{State: updatesvc.PhaseIdle})
	s.watchForInstall("0.1.35")
	s.watchForInstall("0.1.35") // one watch at a time
	time.Sleep(30 * time.Millisecond)
	f.set(&updatesvc.Status{State: updatesvc.PhaseDownloading, Target: "0.1.36", Initiator: updatesvc.InitiatorOwner})
	time.Sleep(30 * time.Millisecond)
	f.set(&updatesvc.Status{State: updatesvc.PhaseChecking, Target: "0.1.35", Initiator: updatesvc.InitiatorLocal})
	select {
	case v := <-shown:
		if v != "0.1.35" {
			t.Fatalf("window for %s", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no window")
	}
	select {
	case v := <-shown:
		t.Fatalf("a second window (%s)", v)
	case <-time.After(100 * time.Millisecond):
	}

	// no shell hook (not Windows), no service: nothing to watch
	s.UpdateProgress = nil
	s.watchForInstall("")
	updateService = func() serviceClient { return nil }
	s.UpdateProgress = func(string) { t.Error("no service: no window") }
	s.watchForInstall("")
}
