package tray

import (
	"fmt"
	"strings"
	"time"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Notification is one desktop notification.
type Notification struct {
	Title, Body string
}

// RelayAlertAfter: a worker whose active leg has been relayed (longnet relay,
// DERP, peer relay) this long gets a notification.
var RelayAlertAfter = 5 * time.Minute

// Rules turns successive pool views and job lists into notifications: a
// worker going offline / back online, memory crossing 90 %, the link falling
// back to a relay for more than 5 minutes, and this client's jobs ending. The
// first observation of anything only records state.
type Rules struct {
	status  map[string]string
	memHigh map[string]bool
	relayed map[string]bool
	jobs    map[string]pb.JobState // worker/id
	jobsSet bool
}

func NewRules() *Rules {
	return &Rules{status: map[string]string{}, memHigh: map[string]bool{}, relayed: map[string]bool{}, jobs: map[string]pb.JobState{}}
}

// Pool compares a pool view with the previous one. Only the user's own
// workers notify (owner = the view's user; every worker with a daemon too
// old to say who the user is): other people's machines going offline or
// filling up are theirs to watch.
func (r *Rules) Pool(v *pb.PoolView, now time.Time) []Notification {
	var out []Notification
	for _, w := range machineWorkers(v, false) { // a session worker speaks for its admin worker
		if u := v.GetUser(); u != "" && w.GetNode().GetOwner() != u {
			continue
		}
		prev, seen := r.status[w.Name]
		switch {
		case w.State == "offline" && seen && (prev == "online" || prev == "connecting"):
			msg := w.Error
			if msg == "" {
				msg = "connection lost; reconnecting"
			}
			out = append(out, Notification{w.Name + " offline", msg})
		case w.State == "online" && prev == "offline":
			out = append(out, Notification{w.Name + " online", "reconnected"})
		}
		r.status[w.Name] = w.State
		if m := w.Metrics; w.State == "online" && m != nil && m.MemTotalBytes > 0 {
			high := float64(m.MemUsedBytes)/float64(m.MemTotalBytes) >= 0.9
			if high && !r.memHigh[w.Name] {
				out = append(out, Notification{w.Name + " memory low", fmt.Sprintf("memory %.0f/%.0f GB", gib(m.MemUsedBytes), gib(m.MemTotalBytes))})
			}
			r.memHigh[w.Name] = high
		}
		a := activeLeg(w.Link)
		relayedLong := a != nil && a.Up && a.RelaySince != nil && now.Sub(a.RelaySince.AsTime()) >= RelayAlertAfter
		if relayedLong && !r.relayed[w.Name] {
			out = append(out, Notification{w.Name + " link relayed",
				fmt.Sprintf("%s for %s (%.0f ms); no direct path", nonEmpty(a.Label, a.Transport), now.Sub(a.RelaySince.AsTime()).Round(time.Minute), a.RttMs)})
		}
		if a == nil || a.RelaySince == nil {
			r.relayed[w.Name] = false
		} else if relayedLong {
			r.relayed[w.Name] = true
		}
	}
	return out
}

// Jobs compares this client's jobs with the previous list.
func (r *Rules) Jobs(jobs []*pb.FleetJob) []Notification {
	var out []Notification
	for _, fj := range jobs {
		j := fj.GetJob()
		key := fj.Worker + "/" + j.GetSpec().GetId()
		prev, seen := r.jobs[key]
		r.jobs[key] = j.GetState()
		if !r.jobsSet || prev == j.GetState() || !terminal(j.GetState()) || (seen && terminal(prev)) {
			continue
		}
		name := j.GetSpec().GetName()
		if name == "" {
			name = strings.Join(j.GetSpec().GetArgv(), " ")
			if len(name) > 60 {
				name = name[:57] + "..."
			}
		}
		switch j.GetState() {
		case pb.JobState_JOB_STATE_DONE:
			out = append(out, Notification{"job done on " + fj.Worker, name})
		case pb.JobState_JOB_STATE_FAILED:
			body := name
			if j.HasExitCode {
				body = fmt.Sprintf("exit %d: %s", j.ExitCode, name)
			}
			out = append(out, Notification{"job failed on " + fj.Worker, body})
		case pb.JobState_JOB_STATE_KILLED:
			out = append(out, Notification{"job killed on " + fj.Worker, name})
		case pb.JobState_JOB_STATE_LOST:
			out = append(out, Notification{"job lost on " + fj.Worker, name})
		}
	}
	r.jobsSet = true
	return out
}

func terminal(s pb.JobState) bool {
	switch s {
	case pb.JobState_JOB_STATE_DONE, pb.JobState_JOB_STATE_FAILED, pb.JobState_JOB_STATE_KILLED, pb.JobState_JOB_STATE_LOST:
		return true
	}
	return false
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
