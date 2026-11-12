package main

import (
	"fmt"
	"strings"
	"time"

	pb "longarm/proto/longarm/v1"
)

// An update of a worker's machine as the hub shows it (NodeInfo.update;
// internal/hub/updatestate.go): longarm ls' STATE, longarm worker update's
// "already updating".

// updateActive: the update is not over (requested, updating, timed out).
func updateActive(u *pb.NodeUpdate) bool {
	switch u.GetState() {
	case "requested", "updating", "timed_out":
		return true
	}
	return false
}

// updatePhaseText: "downloading 12/42 MB", "installing", "waiting for the
// worker to come back (installing)"; "" when unknown.
func updatePhaseText(u *pb.NodeUpdate, online bool) string {
	p := strings.ReplaceAll(u.GetPhase(), "_", " ")
	if p == "health check" {
		p = "checking the new version"
	}
	if u.GetPhase() == "downloading" && u.GetBytesTotal() > 0 {
		p = fmt.Sprintf("downloading %d/%d MB", u.GetBytesDone()>>20, (u.GetBytesTotal()+(1<<20)-1)>>20)
	}
	if !online && u.GetState() == "updating" {
		if p == "" {
			return "restarting"
		}
		return "restarting, last " + p
	}
	return p
}

// fmtVer: "0.1.30" ("" for none).
func fmtVer(v string) string { return strings.TrimPrefix(v, "v") }

// updateStateText is longarm ls' STATE of a worker with an update
// ("" = none: the plain state): "updating 0.1.29→0.1.30 (installing)",
// "update stalled 0.1.29→0.1.30 (installing, 7m)", "online, update to
// 0.1.30 failed (signature)".
func updateStateText(u *pb.NodeUpdate, online bool, now time.Time) string {
	if u == nil || u.State == "" {
		return ""
	}
	arrow := fmtVer(u.From) + "→" + fmtVer(u.To)
	if u.From == "" {
		arrow = "→" + fmtVer(u.To)
	}
	state := "offline"
	if online {
		state = "online"
	}
	switch u.State {
	case "requested":
		return "update requested " + arrow
	case "updating":
		if p := updatePhaseText(u, online); p != "" {
			return "updating " + arrow + " (" + p + ")"
		}
		return "updating " + arrow
	case "timed_out":
		d := ""
		if t := u.GetStartedAt(); t != nil {
			d = ", started " + humanAgo(now.Sub(t.AsTime())) + " ago"
		}
		p := u.Phase
		if p == "" {
			p = "unknown phase"
		}
		return state + ": update to " + fmtVer(u.To) + " didn't finish (last phase " + strings.ReplaceAll(p, "_", " ") + d + ")"
	case "failed":
		why := u.Code
		if why == "" {
			why = "failed"
		}
		return state + ", update to " + fmtVer(u.To) + " failed (" + why + ")"
	case "mismatch":
		return state + ", came back on another version than " + fmtVer(u.To)
	case "restart_pending":
		return state + ", restart the machine to finish " + fmtVer(u.To)
	}
	return ""
}

// updateBusyError: the hub shows an update of worker's machine in
// progress (it, or its companion): a second one is refused (exit 75).
func updateBusyError(worker string, nodes []*pb.NodeInfo, now time.Time) error {
	for _, n := range nodes {
		if n.GetName() != worker && n.GetCompanion() != worker {
			continue
		}
		u := n.GetUpdate()
		if !updateActive(u) || u.GetState() == "timed_out" {
			continue
		}
		what := "updating to " + fmtVer(u.To)
		if p := updatePhaseText(u, n.Online); p != "" {
			what += " (" + p + ")"
		}
		if t := u.GetStartedAt(); t != nil {
			what += ", started " + humanAgo(now.Sub(t.AsTime())) + " ago"
		}
		if by := initiatorText(u.Initiator); by != "" {
			what += " by " + by
		}
		return &codedError{code: exitTempFail, err: fmt.Errorf("%s is already %s: wait for it (longarm ls shows its progress)", n.GetName(), what)}
	}
	return nil
}

func initiatorText(i string) string {
	switch i {
	case "owner":
		return "its owner"
	case "rollout":
		return "a rollout"
	case "auto":
		return "automatic updates"
	case "local":
		return "someone on the machine"
	}
	return ""
}
