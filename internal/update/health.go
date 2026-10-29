package update

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
)

// failHealthForTest makes the health check fail (set with -ldflags -X in
// the e2e rollback test only; empty in real builds).
var failHealthForTest = ""

// Outcome is what Startup found about the last update, for the hub.
type Outcome struct {
	State string // checking | ok | rolled_back | failed
	From  string
	To    string
	Error string
	At    time.Time
}

// Startup is called by longarmd early in Run (after it took the data dir lock).
// When this process is the new version of an update in progress, it
// returns a Check that must be run; otherwise it returns nil. It also
// records the outcome of a finished update (rolled back -> the version is
// skipped from now on).
func Startup(cfg Config) (*Check, *Outcome) {
	cfg.defaults()
	st, err := LoadState(cfg.DataDir)
	if err != nil {
		cfg.Log.Warn("update: state unreadable", "err", err)
		return nil, nil
	}
	if st == nil {
		Cleanup(cfg.Exe)
		return nil, nil
	}
	out := &Outcome{State: st.Phase, From: st.From, To: st.To, Error: st.Error, At: st.Updated}
	switch st.Phase {
	case PhaseRestarting, PhaseChecking, PhaseUnhealthy:
		switch {
		case cfg.Version == st.From:
			// The old version runs again (the swap was undone): nothing to
			// roll back.
			st.Phase, st.Error = PhaseFailed, fmt.Sprintf("restarted as %s, not %s", cfg.Version, st.To)
			st.Save(cfg.DataDir)
			addSkip(cfg.DataDir, st.To)
			out.State, out.Error = st.Phase, st.Error
			return nil, out
		case st.Phase == PhaseUnhealthy || cfg.Version != st.To:
			// The swapped-in binary is not what was pushed, or it already
			// failed: the watchdog must restore the old files.
			if st.Phase != PhaseUnhealthy {
				st.Phase, st.Error = PhaseUnhealthy, fmt.Sprintf("the new binary reports version %s, not %s", cfg.Version, st.To)
			}
			st.PID = os.Getpid()
			st.PIDStart, _ = platform.ProcessStartTime(st.PID)
			ensureWatchdog(cfg, st)
			st.Save(cfg.DataDir)
			out.State, out.Error = st.Phase, st.Error
			return nil, out
		}
		ensureWatchdog(cfg, st) // e.g. the machine rebooted during the window
		st.Phase, st.CheckStarted, st.PID = PhaseChecking, time.Now(), os.Getpid()
		st.PIDStart, _ = platform.ProcessStartTime(st.PID)
		if err := st.Save(cfg.DataDir); err != nil {
			cfg.Log.Warn("update: save state", "err", err)
		}
		out.State = PhaseChecking
		cfg.Log.Info("update: health check of the new version", "from", st.From, "to", st.To, "window", st.HealthWindow.Duration)
		return &Check{cfg: cfg, st: st}, out
	case PhaseRolledBack, PhaseFailed:
		addSkip(cfg.DataDir, st.To)
	case PhaseOK:
		Cleanup(cfg.Exe)
	}
	return nil, out
}

// MarkReported records that the hub got the outcome (it is not resent).
func MarkReported(dataDir string) {
	if st, err := LoadState(dataDir); err == nil && st != nil && !st.Reported {
		st.Reported = true
		st.Save(dataDir)
	}
}

// Reported tells whether the outcome in update.json was sent already.
func Reported(dataDir string) bool {
	st, err := LoadState(dataDir)
	return err != nil || st == nil || st.Reported
}

// Check is the health check of a freshly updated longarmd.
type Check struct {
	cfg Config
	st  *State
}

// Run waits for the health window and then asks healthy (nil = healthy).
// healthy is also polled during the window; it must report a hard failure
// as an error starting with "fatal:" to fail early. The outcome is written
// to update.json, where the watchdog acts on it.
func (c *Check) Run(ctx context.Context, healthy func() error) *Outcome {
	log := c.cfg.Log
	deadline := time.Now().Add(c.st.HealthWindow.Duration)
	var err error
	for {
		if failHealthForTest != "" {
			err = fmt.Errorf("fatal: %s", failHealthForTest)
		} else {
			err = healthy()
		}
		if err != nil && len(err.Error()) > 6 && err.Error()[:6] == "fatal:" {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			// longarmd is shutting down during the window: leave the state as
			// "checking"; the watchdog's deadline decides.
			return nil
		case <-time.After(time.Second):
		}
	}
	st, lerr := LoadState(c.cfg.DataDir)
	if lerr != nil || st == nil || st.To != c.st.To {
		return nil
	}
	if err != nil {
		st.Phase, st.Error = PhaseUnhealthy, err.Error()
		log.Error("update: new version unhealthy; the watchdog rolls back", "version", st.To, "err", err)
	} else {
		st.Phase = PhaseOK
		log.Info("update: new version healthy", "version", st.To)
	}
	st.Save(c.cfg.DataDir)
	return &Outcome{State: st.Phase, From: st.From, To: st.To, Error: st.Error, At: time.Now()}
}

// ---------------------------------------------------------------- watchdog

// startWatchdog runs `<oldLongarmd> update-watch --data-dir D` detached from
// longarmd (it must outlive the restart).
var startWatchdog = func(oldLongarmd, dataDir string) (*exec.Cmd, error) {
	return platform.StartDetached(func() *exec.Cmd {
		cmd := exec.Command(oldLongarmd, "update-watch", "--data-dir", dataDir)
		cmd.Dir = dataDir
		return cmd
	})
}

// Watch is the watchdog (`longarmd update-watch`): it waits until the new
// version reports healthy, or rolls back when it reports unhealthy, never
// starts within the start grace, or does not finish its check in time.
func Watch(ctx context.Context, dataDir string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	fails := 0
	self := os.Getpid()
	for {
		st, err := LoadState(dataDir)
		if err != nil {
			// Transient on Windows: update.json is being replaced ("Access
			// is denied" while the old file is delete-pending) or scanned.
			// Giving up here left a broken version running.
			if fails++; fails == 1 {
				log.Warn("update watchdog: read state; retrying", "err", err)
			}
			if fails < stateReadTries {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(watchInterval):
				}
				continue
			}
			log.Error("update watchdog: cannot read the state; giving up", "err", err)
			return err
		}
		fails = 0
		if st == nil {
			log.Info("update watchdog: no update in progress")
			return nil
		}
		if st.WatchdogPID != 0 && st.WatchdogPID != self && platform.ProcessAlive(st.WatchdogPID, st.WatchdogStart) {
			// Another live watchdog owns the update (a newer update replaced
			// the one this watchdog was started for): two watchdogs rolling
			// back the same files race each other and can leave a binary
			// missing. A recorded watchdog that is gone does not count: a
			// replacement (ensureWatchdog, after a reboot or a crash) reads
			// its dead predecessor's PID until its own is saved.
			log.Info("update watchdog: superseded", "to", st.To, "watchdog", st.WatchdogPID)
			return nil
		}
		var why string
		switch st.Phase {
		case PhaseOK, PhaseRolledBack, PhaseFailed:
			log.Info("update watchdog: done", "phase", st.Phase, "to", st.To)
			return nil
		case PhaseUnhealthy:
			why = "health check failed: " + st.Error
		case PhaseRestarting:
			if time.Since(st.Started) > st.StartGrace.Duration {
				why = fmt.Sprintf("the new version did not start within %s", st.StartGrace.Duration)
			}
		case PhaseChecking:
			if time.Since(st.CheckStarted) > st.HealthWindow.Duration+30*time.Second {
				why = "the new version did not finish its health check"
			} else if st.PID != 0 && !platform.ProcessAlive(st.PID, st.PIDStart) {
				// Crashed during the window (the service manager may restart
				// it; give it the start grace from now on).
				if time.Since(st.CheckStarted) > st.StartGrace.Duration {
					why = "the new version exited during its health check"
				}
			}
		}
		if why != "" {
			return rollback(dataDir, st, why, log)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(watchInterval):
		}
	}
}

// rollback restores the previous files and restarts the old version.
func rollback(dataDir string, st *State, why string, log *slog.Logger) error {
	log.Error("update watchdog: rolling back", "from", st.To, "to", st.From, "reason", why)
	var errs []string
	for i := len(st.Swaps) - 1; i >= 0; i-- {
		s := st.Swaps[i]
		bad := s.Path + badSuffix
		if err := moveAside(bad); err != nil {
			errs = append(errs, err.Error())
		}
		moved := true
		if err := os.Rename(s.Path, bad); err != nil {
			moved = false
			if !os.IsNotExist(err) {
				errs = append(errs, err.Error())
				continue // the new file stays in place: still a binary there
			}
		}
		// The backup can be locked for a moment (AV scan, indexer): retry,
		// and never leave the path empty.
		var err error
		for try := 0; try < 5; try++ {
			if err = os.Rename(s.Backup, s.Path); err == nil {
				break
			}
			time.Sleep(time.Duration(try+1) * time.Second)
		}
		if err != nil {
			errs = append(errs, err.Error())
			if moved {
				os.Rename(bad, s.Path)
			}
		}
	}
	// Stop the new longarmd (if it runs) so the old one can take over.
	if st.PID != 0 && platform.ProcessAlive(st.PID, st.PIDStart) {
		platform.KillProcess(st.PID)
	}
	st.Phase, st.Error, st.Reported = PhaseRolledBack, why, false
	if len(errs) > 0 {
		st.Phase, st.Error = PhaseFailed, why+"; rollback: "+fmt.Sprint(errs)
	}
	st.Save(dataDir)
	addSkip(dataDir, st.To)
	if err := restartOld(st, log); err != nil {
		log.Error("update watchdog: restart", "err", err)
		return err
	}
	return nil
}

// restartOld starts the restored longarmd: through the service manager, or as a
// detached process in exec mode.
func restartOld(st *State, log *slog.Logger) error {
	if st.Mode == "supervised" {
		// the user-session worker's host restarts longarmd (now the
		// restored binary) by itself once the new one is gone
		log.Info("update watchdog: the worker host restarts the previous version")
		return nil
	}
	if st.Mode == "service" {
		// A killed or crash-looping service is restarted by the service
		// manager's recovery; start it explicitly if it stays stopped.
		for i := 0; i < 20; i++ {
			time.Sleep(time.Second)
			if s, err := platform.QueryService(st.ServiceName); err == nil && s == platform.ServiceRunning {
				return nil
			}
		}
		return platform.StartService(st.ServiceName)
	}
	time.Sleep(500 * time.Millisecond) // let the killed process release the data dir lock
	_, err := platform.StartDetached(func() *exec.Cmd { return exec.Command(st.Exe, st.Args...) })
	return err
}

// ensureWatchdog starts a new watchdog from the previous longarmd (the backup)
// when the one Apply started is gone, e.g. after a reboot in the middle of
// an update; without it a failing new version would never be rolled back.
func ensureWatchdog(cfg Config, st *State) {
	if st.WatchdogPID != 0 && platform.ProcessAlive(st.WatchdogPID, st.WatchdogStart) {
		return
	}
	backup := ""
	for _, s := range st.Swaps {
		if filepath.Clean(s.Path) == filepath.Clean(cfg.Exe) {
			backup = s.Backup
		}
	}
	if backup == "" {
		return
	}
	if _, err := os.Stat(backup); err != nil {
		cfg.Log.Warn("update: no watchdog and no previous binary to start one from", "backup", backup)
		return
	}
	cmd, err := startWatchdog(backup, cfg.DataDir)
	if err != nil {
		cfg.Log.Warn("update: restart watchdog", "err", err)
		return
	}
	if cmd.Process != nil {
		st.WatchdogPID = cmd.Process.Pid
		st.WatchdogStart, _ = platform.ProcessStartTime(st.WatchdogPID)
		go cmd.Wait()
	}
	cfg.Log.Info("update: watchdog restarted", "pid", st.WatchdogPID)
}
