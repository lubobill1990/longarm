package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/lubobill1990/longarm/internal/client"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Messages that tell the user what to do next (golden strings in
// hints_test.go).

// forceDial (--force-dial, LONGARM_FORCE_DIAL=1) dials a worker the hub
// reports offline instead of failing at once.
var forceDial bool

// offlineFailFast: a worker the hub last saw longer ago than this fails at
// once (without --force-dial); one that just went away is still dialed (it
// may be restarting, or reachable directly while its hub link is down).
const offlineFailFast = 2 * time.Minute

// noWorkersError: nothing to talk to - no workers.toml entries, and the hub
// (if any) lists none.
func noWorkersError(s *session) error {
	return noWorkersMsg(s.cfg.Hub.Enabled() && !noHub, s.cfg.Hub.Address)
}

func noWorkersMsg(hub bool, addr string) error {
	if hub {
		return fmt.Errorf("no workers: hub %s lists none you may use; make this or another machine yours: `longarm worker add`", addr)
	}
	return fmt.Errorf("no workers: this machine isn't signed in to a hub: run `longarm login`")
}

// unknownWorkerError says that name is no worker, with the closest known
// names (from workers.toml and the hub's list or its cache: no network).
func unknownWorkerError(name, dir string, known []string) error {
	err := client.UnknownWorkerError(name, dir)
	if m := didYouMean(name, known); len(m) > 0 {
		return fmt.Errorf("%w (did you mean %s?)", err, strings.Join(m, " or "))
	}
	return err
}

// didYouMean returns up to three known names close to name: same name in
// another case, a prefix or a substring, else within a small edit distance.
func didYouMean(name string, known []string) []string {
	type cand struct {
		n string
		d int
	}
	var cs []cand
	ln := strings.ToLower(name)
	seen := map[string]bool{}
	for _, k := range known {
		if k == "" || k == name || seen[k] {
			continue
		}
		seen[k] = true
		lk := strings.ToLower(k)
		d := editDistance(ln, lk)
		switch {
		case lk == ln:
			d = 0
		case len(ln) >= 2 && (strings.HasPrefix(lk, ln) || strings.Contains(lk, ln)):
			d = 1
		case d > max(2, len(ln)/3):
			continue
		}
		cs = append(cs, cand{k, d})
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].d != cs[j].d {
			return cs[i].d < cs[j].d
		}
		return cs[i].n < cs[j].n
	})
	var out []string
	for i := 0; i < len(cs) && i < 3; i++ {
		out = append(out, cs[i].n)
	}
	return out
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			c := 1
			if ra[i-1] == rb[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// knownNames are the worker names longarm knows without asking anyone:
// workers.toml and the hub's last list (or its cache).
func (s *session) knownNames() []string {
	var out []string
	for _, w := range s.cfg.Workers {
		out = append(out, w.Name)
	}
	s.hubMu.Lock()
	nodes := s.nodes
	s.hubMu.Unlock()
	if nodes == nil {
		nodes = s.cachedNodes()
	}
	for _, n := range nodes {
		if n.State != "revoked" {
			out = append(out, n.Name)
		}
	}
	return out
}

// agoString is a coarse "how long ago" ("45s", "12m", "2h", "3d").
func agoString(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// offlineNote describes a worker the hub reports offline: "worker X is
// offline (the hub last saw it 2h ago)", plus who must be logged on for a
// worker that runs in its owner's Windows session (the installer's
// default for a worker of one's own).
func offlineNote(n *pb.NodeInfo, now time.Time) string {
	msg := "worker " + n.Name + " is offline"
	if ls := n.GetLastSeen(); ls != nil && ls.AsTime().Unix() > 0 {
		msg += " (the hub last saw it " + agoString(now.Sub(ls.AsTime())) + " ago)"
	} else {
		msg += " (the hub has not seen it)"
	}
	if n.Os == "windows" && n.Owner != "" && !n.AdminWorker {
		msg += "; a user-session worker runs only while its owner (" + n.Owner + ") is logged on"
	}
	return msg
}

// offlineError is the fail-fast error for a worker the hub has not seen for
// a while.
func offlineError(n *pb.NodeInfo, now time.Time) error {
	return &exitError{code: exitLongarmError, msg: offlineNote(n, now) + "; to try anyway: --force-dial"}
}

// workerOffline reports whether the hub's live answer says n is offline
// for longer than offlineFailFast.
func workerOffline(n *pb.NodeInfo, now time.Time) bool {
	if n.Online || n.State != "approved" {
		return false
	}
	ls := n.GetLastSeen()
	return ls == nil || now.Sub(ls.AsTime()) > offlineFailFast
}

// signInExpiredError: the client certificate expired.
func signInExpiredError(notAfter time.Time) error {
	return &exitError{code: exitNoPerm, msg: fmt.Sprintf("your sign-in expired on %s: run `longarm login`", notAfter.Local().Format("2006-01-02 15:04"))}
}

// renewWarnWindow: a certificate this close to its end gets a warning
// when renewing it fails.
const renewWarnWindow = 72 * time.Hour

// renewFailedNote is the warning when renewing the certificate failed.
func renewFailedNote(notAfter, now time.Time, err string) string {
	if left := notAfter.Sub(now); left < renewWarnWindow {
		return fmt.Sprintf("longarm: your sign-in expires on %s (in %s) and renewing it failed (%s): run `longarm login`",
			notAfter.Local().Format("2006-01-02 15:04"), agoString(left), err)
	}
	return "longarm: certificate renewal failed: " + err
}

// gitCheckoutHint: run without --repo / --cwd inside a git checkout runs in
// the worker's home, which may surprise.
func gitCheckoutHint(worker string) string {
	return fmt.Sprintf("longarm: running in %s's home directory; add `--repo .` to run in a copy of this checkout", worker)
}

// insideGitCheckout reports whether dir is inside a git work tree (looks
// for .git upwards; no git process).
func insideGitCheckout(dir string) bool {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		up := filepath.Dir(dir)
		if up == dir {
			return false
		}
		dir = up
	}
}

// followHint is printed after a detached run on a terminal.
func followHint(id string) string {
	return "longarm: follow: longarm logs " + id + " -f"
}

// stderrIsTerminal: hints for people go to a terminal only, never into a
// program's captured stderr.
var stderrIsTerminal = func() bool { return term.IsTerminal(int(os.Stderr.Fd())) }

// dialErrorMessage strips gRPC's wrapping off a failed dial ("connection
// error: desc = \"transport: Error while dialing: <err>\"") so the reason -
// e.g. "worker X is offline (...)" - comes first.
func dialErrorMessage(msg string) string {
	const pre = `connection error: desc = "transport: Error while dialing: `
	if rest, ok := strings.CutPrefix(msg, pre); ok {
		return strings.TrimSuffix(rest, `"`)
	}
	return msg
}
