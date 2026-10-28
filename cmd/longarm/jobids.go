package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Job ids on the command line (logs, kill, wait, jobs --id): the full id,
// a unique prefix or suffix of it (the tray shows the last 8 characters),
// or "last" - the caller's most recent job.

// lastJobArg names the caller's most recent job.
const lastJobArg = "last"

// jobLookupLimit bounds the jobs listed to resolve a short id.
const jobLookupLimit = 1000

// fullJobID reports whether arg is a complete ULID (what run generates):
// it is used as is, without listing jobs.
func fullJobID(arg string) bool {
	if len(arg) != ulid.EncodedSize {
		return false
	}
	_, err := ulid.ParseStrict(arg)
	return err == nil
}

// matchJobID resolves arg among rows: an exact id wins, else the ids arg
// is a prefix or suffix of (case-insensitively). ambiguous lists the
// candidates when there are several; ok=false when nothing matches.
func matchJobID(rows []*pb.FleetJob, arg string) (match *pb.FleetJob, ambiguous []*pb.FleetJob, ok bool) {
	for _, fj := range rows {
		if fj.GetJob().GetSpec().GetId() == arg {
			return fj, nil, true
		}
	}
	la := strings.ToLower(arg)
	seen := map[string]bool{}
	var hits []*pb.FleetJob
	for _, fj := range rows {
		id := fj.GetJob().GetSpec().GetId()
		li := strings.ToLower(id)
		if seen[fj.Worker+"/"+id] || !(strings.HasPrefix(li, la) || strings.HasSuffix(li, la)) {
			continue
		}
		seen[fj.Worker+"/"+id] = true
		hits = append(hits, fj)
	}
	switch len(hits) {
	case 0:
		return nil, nil, false
	case 1:
		return hits[0], nil, true
	}
	return nil, hits, false
}

// newestJobOf is the most recently created job owned by owner ("" = any).
func newestJobOf(rows []*pb.FleetJob, owner string) *pb.FleetJob {
	var best *pb.FleetJob
	for _, fj := range rows {
		j := fj.GetJob()
		if owner != "" && j.GetSpec().GetOwner() != owner {
			continue
		}
		if best == nil || j.GetCreatedAt().AsTime().After(best.GetJob().GetCreatedAt().AsTime()) {
			best = fj
		}
	}
	return best
}

// ambiguousJobError lists the candidates of an ambiguous short id (exit 2).
func ambiguousJobError(arg string, hits []*pb.FleetJob) error {
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].GetJob().GetCreatedAt().AsTime().After(hits[j].GetJob().GetCreatedAt().AsTime())
	})
	var b strings.Builder
	fmt.Fprintf(&b, "job id %s is ambiguous: it matches %d jobs; give more characters:", arg, len(hits))
	for i, fj := range hits {
		if i == 10 {
			fmt.Fprintf(&b, "\n  ... and %d more", len(hits)-i)
			break
		}
		j := fj.GetJob()
		fmt.Fprintf(&b, "\n  %s  %s  %s  %s", j.GetSpec().GetId(), fj.Worker, stateName(j.GetState()), strings.Join(j.GetSpec().GetArgv(), " "))
	}
	return usagef("%s", b.String())
}

// jobsForLookup lists the jobs a short id or "last" is resolved among: the
// hub's index, else the known workers (worker "" = all).
func (s *session) jobsForLookup(ctx context.Context, worker string) []*pb.FleetJob {
	if h := s.optionalHub(ctx); h != nil {
		rows, err := hubJobs(ctx, h, worker, "", nil, jobLookupLimit)
		if err == nil {
			return rows
		}
		if hubUnavailable(err) {
			s.hubDown(err)
		}
	}
	rows, _, err := workerJobs(ctx, s, worker, "", nil, 0)
	if err != nil {
		return nil
	}
	return rows
}

// resolveJobArg turns a job argument into the job's id (and its worker
// when the lookup found it; else worker as given). A full id is used as
// is; a short one that matches nothing is passed on unchanged, so a job
// id of another form (run --id) keeps working and "not found" stays exit 3.
func (s *session) resolveJobArg(ctx context.Context, worker, arg string) (string, string, error) {
	if arg != lastJobArg && fullJobID(arg) {
		return worker, arg, nil
	}
	rows := s.jobsForLookup(ctx, worker)
	if arg == lastJobArg {
		fj := newestJobOf(rows, s.dialer.Identity().Owner())
		if fj == nil {
			return "", "", notFoundf("you have no jobs yet (longarm run -- <command> submits one)")
		}
		return fj.Worker, fj.Job.Spec.Id, nil
	}
	fj, hits, ok := matchJobID(rows, arg)
	switch {
	case ok:
		return fj.Worker, fj.Job.Spec.Id, nil
	case len(hits) > 0:
		return "", "", ambiguousJobError(arg, hits)
	}
	return worker, arg, nil
}

// sessionFromArgs is logs --session without a value (the older boolean
// form): the session name is the last argument.
const sessionFromArgs = "(from-args)"

// logsSessionArgs sorts out logs' arguments: a job ("[<worker>] <id>"), or
// with -s/--session a session (-s NAME [<worker>], or the older
// "<worker> <session> --session").
func logsSessionArgs(flag string, args []string) (session bool, name string, rest []string, err error) {
	switch flag {
	case "":
		if len(args) == 0 {
			return false, "", nil, usagef("usage: longarm logs [<worker>] <job-id>, or longarm logs [<worker>] -s <session>")
		}
		return false, "", args, nil
	case sessionFromArgs:
		if len(args) == 0 {
			return false, "", nil, usagef("usage: longarm logs [<worker>] -s <session>")
		}
		return true, args[len(args)-1], args[:len(args)-1], nil
	}
	if len(args) > 1 {
		return false, "", nil, usagef("usage: longarm logs [<worker>] -s <session>")
	}
	return true, flag, args, nil
}

// defaultJobsLimit: rows of the jobs table without --limit.
const defaultJobsLimit = 20

// jobsViewOpts are the flags that decide which jobs `longarm jobs` shows.
type jobsViewOpts struct {
	limit     int
	limitSet  bool // --limit given
	json      bool
	id        bool // --id: one job, no defaults
	mine, all bool
	admin     bool // an admin certificate (sees everybody's jobs)
}

// jobsView: how many jobs to fetch, and how to cut them down.
type jobsView struct {
	fetch int  // limit for the hub / the workers (0 = all)
	show  int  // rows shown (0 = all)
	mine  bool // only the caller's own jobs

	mineByDefault bool // mine without --mine (an admin)
}

// planJobsView applies the defaults: the table shows defaultJobsLimit rows
// and, for admins, only their own jobs; --json keeps listing everything
// (its contract) unless --limit / --mine are given.
func planJobsView(o jobsViewOpts) jobsView {
	if o.id {
		return jobsView{}
	}
	v := jobsView{mine: o.mine || (o.admin && !o.all && !o.json)}
	v.mineByDefault = v.mine && !o.mine
	switch {
	case o.limitSet:
		v.show = o.limit
	case !o.json:
		v.show = defaultJobsLimit
	}
	switch {
	case v.show == 0:
	case v.mine:
		v.fetch = max(v.show*10, 200) // others' jobs are dropped here
	default:
		v.fetch = v.show + 1 // one more: tells whether there are more
	}
	return v
}

// apply filters rows (newest first) for v; more reports rows left out by
// the limit.
func (v jobsView) apply(rows []*pb.FleetJob, me string) (out []*pb.FleetJob, more bool) {
	out = rows
	if v.mine {
		out = nil
		for _, fj := range rows {
			if fj.GetJob().GetSpec().GetOwner() == me {
				out = append(out, fj)
			}
		}
	}
	if v.show > 0 && len(out) > v.show {
		return out[:v.show], true
	}
	return out, false
}

// note is the stderr line under a cut-down table.
func (v jobsView) note(more bool) string {
	var parts []string
	if more {
		parts = append(parts, fmt.Sprintf("the newest %d jobs; --limit 0 for all", v.show))
	}
	if v.mineByDefault {
		parts = append(parts, "only yours; --all for everybody's")
	}
	if len(parts) == 0 {
		return ""
	}
	return "longarm: " + strings.Join(parts, "; ")
}
