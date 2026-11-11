package tray

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/updatesvc"
	"github.com/lubobill1990/longarm/internal/webui"
)

// The update progress window (Windows): an update of this machine that the
// tray started - its banner, or a machine card's Update for this machine
// (longarm worker update -> longarmd -> the Longarm Update service) - runs
// msiexec /qn as SYSTEM, which stops every tray before it replaces them:
// nothing on the screen said what happened until the new tray came up.
// So the tray starts `longarm-tray --update-progress VERSION` from a copy
// outside the install dir (the installer stops only processes whose image
// is in it; cmd/longarm-tray/updateprogress_windows.go): one small window
// that follows the service (download, verification, the install while the
// service itself is down, its result) and closes itself once the new
// version is in.
//
//	GET  /trayapi/progress        ProgressStatus (the window's page, /update)
//	POST /trayapi/progress/close  the Close button

// Phases of ProgressStatus: the service's (updatesvc.Phase*) while it
// answers, and these.
const (
	ProgressWaiting    = "waiting"    // the service hasn't started this install (yet)
	ProgressRestarting = "restarting" // msiexec runs: the service (and the tray) are stopped
	ProgressDone       = "done"
	ProgressFailed     = "failed"
)

// ProgressStatus is what the window shows.
type ProgressStatus struct {
	To         string `json:"to"`
	From       string `json:"from,omitempty"`
	Phase      string `json:"phase"`
	Message    string `json:"message,omitempty"`
	BytesDone  int64  `json:"bytes_done,omitempty"`
	BytesTotal int64  `json:"bytes_total,omitempty"`
	Error      string `json:"error,omitempty"`
	// Reboot: installed, but Windows needs a restart to finish
	Reboot bool `json:"reboot,omitempty"`
	// CloseIn: the window closes by itself in this many seconds (done)
	CloseIn int `json:"close_in,omitempty"`
}

// progress timings (variables for tests)
var (
	// progressNoStart: an install that never showed in the service
	progressNoStart = 90 * time.Second
	// progressTooLong: the window gives up waiting
	progressTooLong = 30 * time.Minute
	// progressNoResult: the service is back after msiexec but has no
	// result for it (an older service, a lost record)
	progressNoResult = 3 * time.Minute
	// progressCloseAfter: a finished update's window closes itself
	progressCloseAfter = 8 * time.Second
)

// progressWatch turns the service's answers into a ProgressStatus.
type progressWatch struct {
	to      string
	started time.Time
	// attempt: the service's id of this install, once seen
	attempt string
	from    string
	active  bool      // the service showed this install
	gone    bool      // ... then stopped answering (msiexec runs)
	back    time.Time // ... and answers again since
	end     *ProgressStatus
	endAt   time.Time
	last    ProgressStatus
}

func newProgressWatch(to string, now time.Time) *progressWatch {
	return &progressWatch{to: to, started: now}
}

func sameVersion(a, b string) bool {
	return a != "" && strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

func activePhase(p string) bool {
	switch p {
	case updatesvc.PhaseChecking, updatesvc.PhaseDownloading, updatesvc.PhaseVerifying, updatesvc.PhaseInstalling:
		return true
	}
	return false
}

// step takes the service's status (or the error asking for it) at now.
func (w *progressWatch) step(now time.Time, st *updatesvc.Status, err error) ProgressStatus {
	if w.end != nil {
		e := *w.end
		if e.Phase == ProgressDone {
			e.CloseIn = max(0, int((progressCloseAfter-now.Sub(w.endAt)).Round(time.Second)/time.Second))
		}
		return e
	}
	p := w.next(now, st, err)
	p.To, p.From = w.to, w.from
	if p.Phase == ProgressDone || p.Phase == ProgressFailed {
		w.end, w.endAt = &p, now
		if p.Phase == ProgressDone {
			p.CloseIn = int(progressCloseAfter / time.Second)
		}
	}
	w.last = p
	return p
}

func (w *progressWatch) next(now time.Time, st *updatesvc.Status, err error) ProgressStatus {
	if now.Sub(w.started) > progressTooLong {
		return ProgressStatus{Phase: ProgressFailed, Error: "The update is taking much longer than it should. Settings > Updates in the tray shows how it ended."}
	}
	if err != nil || st == nil {
		if w.active {
			// msiexec stopped the service with the rest of Longarm
			w.gone, w.back = true, time.Time{}
			return ProgressStatus{Phase: ProgressRestarting}
		}
		if now.Sub(w.started) > progressNoStart {
			return ProgressStatus{Phase: ProgressFailed, Error: "The Longarm Update service doesn't answer, so the update can't be followed here."}
		}
		return ProgressStatus{Phase: ProgressWaiting}
	}
	if w.from == "" && !w.active {
		w.from = st.Installed
	}
	if r := st.Last; r != nil && sameVersion(r.To, w.to) && w.ours(r) {
		if !r.OK {
			msg := r.Error
			if msg == "" {
				msg = "The installer failed."
			}
			return ProgressStatus{Phase: ProgressFailed, Error: msg}
		}
		return ProgressStatus{Phase: ProgressDone, Reboot: r.RebootRequired()}
	}
	if activePhase(st.State) && (st.Target == "" || sameVersion(st.Target, w.to)) {
		w.active = true
		if st.Attempt != "" {
			w.attempt = st.Attempt
		}
		ph := st.State
		if w.gone {
			ph = ProgressRestarting // answering again, the result not recorded yet
		}
		return ProgressStatus{Phase: ph, Message: st.Message, BytesDone: st.BytesDone, BytesTotal: st.BytesTotal}
	}
	// idle (or busy with another version)
	if w.gone && sameVersion(st.Installed, w.to) {
		return ProgressStatus{Phase: ProgressDone} // an older service records no result
	}
	if w.gone {
		if w.back.IsZero() {
			w.back = now
		}
		if now.Sub(w.back) > progressNoResult {
			return ProgressStatus{Phase: ProgressFailed, Error: "Longarm started again on " + st.Installed + ", not " + w.to + "; the update service recorded no result."}
		}
		return ProgressStatus{Phase: ProgressRestarting}
	}
	if !w.active && now.Sub(w.started) > progressNoStart {
		return ProgressStatus{Phase: ProgressFailed, Error: "The update to " + w.to + " didn't start."}
	}
	return ProgressStatus{Phase: ProgressWaiting}
}

// ours: r is this install's result - the same attempt, or (an older
// service, or the attempt not seen) one recorded since the window started.
func (w *progressWatch) ours(r *updatesvc.Result) bool {
	if w.attempt != "" && r.Attempt != "" {
		return r.Attempt == w.attempt
	}
	return !r.At.Before(w.started.Add(-5 * time.Second))
}

// ProgressHandler serves the progress window (the web UI's /update page
// and its API) for the install of version to; quit closes the window.
func ProgressHandler(to string, log *slog.Logger, quit func()) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := updatesvc.NewClient("longarm-tray update progress")
	status := func(ctx context.Context) (*updatesvc.Status, error) { return c.Status(ctx) }
	return progressHandler(to, log, quit, status, time.Now)
}

func progressHandler(to string, log *slog.Logger, quit func(), status func(context.Context) (*updatesvc.Status, error), now func() time.Time) http.Handler {
	var mu sync.Mutex
	w := newProgressWatch(to, now())
	var closing sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET /trayapi/progress", func(rw http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		st, err := status(ctx)
		mu.Lock()
		prev := w.last.Phase
		p := w.step(now(), st, err)
		mu.Unlock()
		if p.Phase != prev {
			log.Info("update progress", "to", to, "phase", p.Phase, "error", p.Error)
		}
		if p.Phase == ProgressDone && p.CloseIn == 0 {
			closing.Do(func() { go quit() })
		}
		writeJSON(rw, p)
	})
	mux.HandleFunc("POST /trayapi/progress/close", func(rw http.ResponseWriter, _ *http.Request) {
		closing.Do(func() { go quit() })
		writeJSON(rw, map[string]bool{"ok": true})
	})
	mux.Handle("/", webui.Handler("/trayapi/"))
	return mux
}

// watchForInstall waits (up to installShowsWithin) for this machine's
// update service to start an install the tray asked for - of version, or
// of any version ("": a machine card's Update, which reaches this machine
// only when the card was this machine) - and then starts the window, once.
func (s *Server) watchForInstall(version string) {
	if s.UpdateProgress == nil || !s.progressWatching.CompareAndSwap(false, true) {
		return
	}
	c := updateService()
	if c == nil {
		s.progressWatching.Store(false)
		return
	}
	go func() {
		defer s.progressWatching.Store(false)
		deadline := time.Now().Add(installShowsWithin)
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			resp, err := c.Do(ctx, &updatesvc.Request{Op: updatesvc.OpStatus})
			cancel()
			if err == nil && resp.Err() == nil && resp.Status != nil {
				st := resp.Status
				if activePhase(st.State) && st.Initiator != updatesvc.InitiatorAuto && st.Target != "" &&
					(version == "" || sameVersion(st.Target, version)) {
					s.opts.Log.Info("update: showing its progress", "to", st.Target)
					s.UpdateProgress(st.Target)
					return
				}
			}
			time.Sleep(progressPoll)
		}
	}()
}

// statusWriter remembers the response's status (0: 200, nothing written yet).
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

var (
	// installShowsWithin: how long after the click the service may take to
	// start the install
	installShowsWithin = 2 * time.Minute
	progressPoll       = time.Second
)
