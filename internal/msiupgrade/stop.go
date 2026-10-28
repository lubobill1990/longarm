package msiupgrade

import (
	"fmt"
	"io"
	"sort"
	"time"
)

// System is what stopping needs from the OS (Windows: WindowsSystem; tests
// fake it).
type System interface {
	// Processes lists the running processes (those it can't read may lack
	// Args or SID).
	Processes() ([]Proc, error)
	// Signal asks p to stop (its stop event); false when p has none (an
	// older version: it is ended instead).
	Signal(p Proc) bool
	// WaitExit waits up to d for p to end; true once it has.
	WaitExit(p Proc, d time.Duration) bool
	// Terminate ends p - only the same process (pid and start time) and
	// only while its image is still in the install dir.
	Terminate(p Proc) error
}

// StopOptions for StopProcesses.
type StopOptions struct {
	// Dir: the install dir; only processes whose image is directly in it.
	Dir string
	// SID: only this user's processes ("" = every user's).
	SID string
	// Kinds to stop (nil = all).
	Kinds []Kind
	// Timeout: how long a process asked to stop gets before it is ended
	// (default 20 s).
	Timeout time.Duration
	// Rounds: how often the processes are listed again (a host may have
	// started its longarmd once more meanwhile; default 3).
	Rounds int
	Log    io.Writer
}

// Stopped is what StopProcesses found running (and stopped).
type Stopped struct {
	// Trays and Workers: the users (SIDs) whose tray / user-session worker
	// host ran.
	Trays, Workers []string
	// Left: processes that could not be ended.
	Left []Proc
}

// StopProcesses stops the Longarm processes of o.Dir: each is asked to
// stop (System.Signal) and ended by its pid when it has no stop event or is
// still there after the timeout. Hosts go before trays and daemons, so that
// no host starts its longarmd again; the list is read again until nothing
// is left (at most o.Rounds times).
func StopProcesses(sys System, o StopOptions) (Stopped, error) {
	if o.Timeout <= 0 {
		o.Timeout = 20 * time.Second
	}
	if o.Rounds <= 0 {
		o.Rounds = 3
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	kinds := map[Kind]bool{}
	for _, k := range o.Kinds {
		kinds[k] = true
	}
	trays, workers := map[string]bool{}, map[string]bool{}
	var res Stopped
	for round := 0; round < o.Rounds; round++ {
		procs, err := sys.Processes()
		if err != nil {
			return res, fmt.Errorf("list processes: %w", err)
		}
		type target struct {
			p Proc
			k Kind
		}
		var ts []target
		for _, p := range procs {
			if !InDir(o.Dir, p.Exe) || (o.SID != "" && p.SID != o.SID) {
				continue
			}
			k, ok := Classify(p)
			if !ok || (len(kinds) > 0 && !kinds[k]) {
				continue
			}
			ts = append(ts, target{p, k})
			switch {
			case k == KindTray && p.SID != "":
				trays[p.SID] = true
			case k == KindHost && p.SID != "":
				workers[p.SID] = true
			}
		}
		if len(ts) == 0 {
			res.Left = nil
			break
		}
		order := map[Kind]int{KindHost: 0, KindTray: 1, KindDaemon: 2}
		sort.SliceStable(ts, func(i, j int) bool { return order[ts[i].k] < order[ts[j].k] })
		var asked []target
		for _, t := range ts {
			if sys.Signal(t.p) {
				fmt.Fprintf(o.Log, "asked %s (pid %d, %s) to stop\n", t.k, t.p.PID, t.p.SID)
				asked = append(asked, t)
				continue
			}
			end(sys, o.Log, t.p, string(t.k)+" (no stop event)")
		}
		deadline := time.Now().Add(o.Timeout)
		res.Left = nil
		for _, t := range asked {
			left := time.Until(deadline)
			if left < 0 {
				left = 0
			}
			if sys.WaitExit(t.p, left) {
				fmt.Fprintf(o.Log, "%s (pid %d) stopped\n", t.k, t.p.PID)
				continue
			}
			if !end(sys, o.Log, t.p, string(t.k)+" (did not stop in time)") {
				res.Left = append(res.Left, t.p)
			}
		}
		for _, t := range ts {
			if !sys.WaitExit(t.p, 5*time.Second) && !contains(res.Left, t.p) {
				res.Left = append(res.Left, t.p)
			}
		}
	}
	res.Trays, res.Workers = keys(trays), keys(workers)
	return res, nil
}

func end(sys System, log io.Writer, p Proc, why string) bool {
	if err := sys.Terminate(p); err != nil {
		fmt.Fprintf(log, "end %s pid %d: %v\n", why, p.PID, err)
		return false
	}
	fmt.Fprintf(log, "ended %s pid %d\n", why, p.PID)
	return true
}

func contains(ps []Proc, p Proc) bool {
	for _, x := range ps {
		if x.PID == p.PID && x.Start == p.Start {
			return true
		}
	}
	return false
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}
