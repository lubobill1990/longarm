package hub

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/update"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The update of a worker's machine as everyone who may see the worker sees
// it (NodeInfo.update; docs/usage.md "Updating"): "updating to 0.1.30
// (downloading 12/42 MB)" instead of "offline, reconnecting" while it
// restarts, then online on the new version, or a clear failure.
//
// Only two sources set it: the worker's own session (its update reports
// and the hello it reconnects with: handleReport, Connect) and the hub's
// rollouts (a "requested" state when the hub tells a worker to update,
// which also covers workers too old to report phases). No client request
// reaches it. The state is persisted in the node's record (store.Node
// UpdateState) on every change of state or phase, not on every progress
// report, and survives a restart of the hub; a hello rebuilds a lost one.
//
// States (NodeUpdate.state):
//
//	requested       a rollout told the worker to update; nothing reported yet
//	updating        in progress (phase), the worker may be away restarting
//	timed_out       no result by the deadline ("didn't finish"); a later
//	                hello still resolves it
//	failed          it ended on the old version (error, code)
//	mismatch        it came back on another version than the target
//	restart_pending installed; the machine must restart to finish (msiexec
//	                3010), cleared once the machine booted again
//
// Success clears the state. failed and mismatch stay until a new attempt
// or updateResultTTL; clients may dismiss them locally.
const (
	updRequested      = "requested"
	updUpdating       = "updating"
	updTimedOut       = "timed_out"
	updFailed         = "failed"
	updMismatch       = "mismatch"
	updRestartPending = "restart_pending"
)

// Phases a worker may report (UpdateStatus.phase); anything else is
// dropped.
var updatePhases = map[string]bool{"checking": true, "downloading": true, "verifying": true, "installing": true,
	"restarting": true, "health_check": true}

// Initiators a worker may report.
var updateInitiators = map[string]bool{"owner": true, "rollout": true, "auto": true, "local": true}

const (
	defaultUpdatePhase     = 15 * time.Minute
	defaultUpdateRestart   = 10 * time.Minute
	defaultUpdateRequested = 10 * time.Minute
	defaultUpdateMax       = 60 * time.Minute
	// updateResultTTL: a failure stays shown this long.
	updateResultTTL = 24 * time.Hour
	// updateStartGrace: after the hub starts, no update times out sooner
	// (the workers are just reconnecting), up to its max duration.
	updateStartGrace = 2 * time.Minute
)

func (h *Hub) updLimits() (phase, restart, requested, maxDur time.Duration) {
	u := h.cfg.Updates
	phase, restart, requested, maxDur = u.Phase.Duration, u.Restart.Duration, u.Requested.Duration, u.Max.Duration
	if phase <= 0 {
		phase = defaultUpdatePhase
	}
	if restart <= 0 {
		restart = defaultUpdateRestart
	}
	if requested <= 0 {
		requested = defaultUpdateRequested
	}
	if maxDur <= 0 {
		maxDur = defaultUpdateMax
	}
	return
}

func activeUpdate(u *pb.NodeUpdate) bool {
	if u == nil {
		return false
	}
	switch u.State {
	case updRequested, updUpdating, updTimedOut:
		return true
	}
	return false
}

// loadUpdateState reads a node's persisted update state (Open). Deadlines
// of active updates move to at least now+updateStartGrace (the hub was
// away; its workers are reconnecting), never past the update's max.
func (h *Hub) loadUpdateState(n *node, now time.Time) {
	if len(n.rec.UpdateState) == 0 {
		return
	}
	u := &pb.NodeUpdate{}
	if (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(n.rec.UpdateState, u) != nil || u.State == "" {
		return
	}
	if u.State == updRequested || u.State == updUpdating {
		_, _, _, maxDur := h.updLimits()
		limit := u.GetStartedAt().AsTime().Add(maxDur)
		if d := now.Add(updateStartGrace); u.GetDeadline().AsTime().Before(d) {
			if d.After(limit) {
				d = limit
			}
			if d.After(u.GetDeadline().AsTime()) {
				u.Deadline = tspb(d)
			}
		}
	}
	n.upd = u
}

// encodeUpdate puts n's update state into its record (saved with it).
func (n *node) encodeUpdate() {
	n.rec.UpdateState = nil
	if n.upd != nil {
		if b, err := protojson.Marshal(n.upd); err == nil {
			n.rec.UpdateState = b
		}
	}
}

// saveUpdateLocked persists n's update state (a change of state or phase).
func (h *Hub) saveUpdateLocked(n *node) {
	n.encodeUpdate()
	if err := h.store.PutNode(n.rec); err != nil {
		h.log.Warn("save update state", "node", n.rec.Name, "err", err)
	}
}

// legacyUpdateStatus is what store.Node.Update keeps: the fields an older
// hub reads (a hub downgrade must still parse it for its rollouts).
func legacyUpdateStatus(s *pb.UpdateStatus) *pb.UpdateStatus {
	return &pb.UpdateStatus{State: s.State, From: s.From, To: s.To, Error: s.Error, At: s.At}
}

// clean bounds and strips a worker's text (control characters, length).
func clean(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		s = string(r[:n-1]) + "…"
	}
	return s
}

// updateCode is a short reason for a failure from the worker's code or,
// for older workers, its message.
func updateCode(code, msg string) string {
	switch code {
	case "signature", "sha256", "disk", "msi", "downgrade", "health", "refused", "unavailable", "not_found",
		"invalid", "timeout", "version", "failed", "unsupported":
		return code
	}
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "sha256"):
		return "sha256"
	case strings.Contains(m, "signature") || strings.Contains(m, "minisig") || strings.Contains(m, "not signed"):
		return "signature"
	case strings.Contains(m, "no space") || strings.Contains(m, "not enough space") || strings.Contains(m, "disk full") || strings.Contains(m, "disk is full"):
		return "disk"
	case strings.Contains(m, "downgrade") || strings.Contains(m, "not newer") || strings.Contains(m, "older than"):
		return "downgrade"
	case strings.Contains(m, "msiexec") || strings.Contains(m, "installer"):
		return "msi"
	case strings.Contains(m, "health"):
		return "health"
	}
	return "failed"
}

// inProgress: an UpdateStatus state of an update that is not over.
func inProgress(state string) bool {
	switch state {
	case "applying", update.PhaseRestarting, update.PhaseChecking:
		return true
	}
	return false
}

func failedState(state string) bool {
	switch state {
	case update.PhaseFailed, update.PhaseRolledBack, update.PhaseUnhealthy, "error":
		return true
	}
	return false
}

// attemptOf is a report's attempt: its own ID (0.1.30+), else (older
// workers) the target version.
func attemptOf(s *pb.UpdateStatus) string {
	if s.Attempt != "" {
		return clean(s.Attempt, 80)
	}
	return "to:" + s.To
}

// sameAttempt: report s belongs to the update u shows. A rollout's
// request becomes the worker's attempt once it reports the same target;
// an older worker's reports (no attempt ID) match by target.
func sameAttempt(u *pb.NodeUpdate, s *pb.UpdateStatus) bool {
	if u == nil {
		return false
	}
	if u.Attempt == attemptOf(s) {
		return true
	}
	if u.To != s.To {
		return false
	}
	return strings.HasPrefix(u.Attempt, "rollout:") || s.Attempt == "" || strings.HasPrefix(u.Attempt, "to:")
}

// phaseOf maps a report to a phase (older workers: from the state).
func phaseOf(s *pb.UpdateStatus) string {
	if updatePhases[s.Phase] {
		return s.Phase
	}
	switch s.State {
	case update.PhaseRestarting:
		return "restarting"
	case update.PhaseChecking:
		return "health_check"
	}
	return ""
}

// deadlineFor is when an update in phase (offline: away restarting)
// times out, from now; never later than started + max.
func (h *Hub) deadlineFor(u *pb.NodeUpdate, now time.Time, offline bool) time.Time {
	phase, restart, requested, maxDur := h.updLimits()
	d := now.Add(phase)
	switch {
	case u.State == updRequested && !offline:
		d = now.Add(requested)
	case offline, u.Phase == "restarting", u.Phase == "health_check":
		d = now.Add(restart)
	}
	if limit := u.GetStartedAt().AsTime().Add(maxDur); u.StartedAt != nil && d.After(limit) {
		d = limit
	}
	return d
}

// updateReportLocked applies an update report of n's own session (a
// NodeReport.update, or the status in its hello). It reports whether the
// report changed the state or phase (persisted) - progress only is not.
func (h *Hub) updateReportLocked(n *node, s *pb.UpdateStatus, now time.Time) (changed bool) {
	if s == nil || s.To == "" || !update.ValidVersion(s.To) || n.rec.Role != "worker" || n.rec.State != stateApproved {
		return false
	}
	if s.From != "" && !update.ValidVersion(s.From) {
		s.From = ""
	}
	att := attemptOf(s)
	cur := n.upd
	if s.State == update.PhaseOK && s.RebootRequired {
		// msiexec 3010: the result may come after the new version said
		// hello (the update looked done): a restart is still pending -
		// unless the machine restarted since (UpdateRestarted).
		switch {
		case att == n.rec.UpdateRestarted, cur != nil && cur.State == updRestartPending:
			return false
		case cur != nil && sameAttempt(cur, s):
		case activeUpdate(cur), s.At == nil || now.Sub(s.At.AsTime()) > time.Hour:
			return false
		default:
			cur = &pb.NodeUpdate{From: s.From, To: s.To, Attempt: att, StartedAt: s.Started, Reported: true, Method: s.Method}
			n.upd = cur
		}
		h.restartPendingLocked(n, cur, now)
		return true
	}
	// A replay of an update that ended (and was resolved) already: an older
	// worker's reports carry no attempt ID - its in-progress report of the
	// same target counts as new unless it runs that version already.
	replay := att == n.rec.UpdateResolved && !sameAttempt(cur, s)
	if replay && (!inProgress(s.State) || s.Attempt != "" || n.rec.Version == s.To) {
		return false
	}
	switch {
	case inProgress(s.State):
		if cur != nil && sameAttempt(cur, s) && cur.State != updFailed && cur.State != updMismatch && cur.State != updRestartPending {
			old := proto.Clone(cur).(*pb.NodeUpdate)
			h.mergeProgress(cur, s, now)
			return cur.State != old.State || cur.Phase != old.Phase || cur.Attempt != old.Attempt
		}
		if cur != nil && sameAttempt(cur, s) {
			return false // it ended already; a late progress report
		}
		u := &pb.NodeUpdate{State: updUpdating, From: s.From, To: s.To, Attempt: att, StartedAt: s.Started, Reported: true}
		if u.From == "" {
			u.From = n.rec.Version
		}
		if u.StartedAt == nil || u.StartedAt.AsTime().After(now) || now.Sub(u.StartedAt.AsTime()) > 24*time.Hour {
			u.StartedAt = tspb(now) // the hub's clock
		}
		n.upd = u
		h.mergeProgress(u, s, now)
		h.log.Info("node updating", "node", n.rec.Name, "from", u.From, "to", u.To, "initiator", u.Initiator, "method", u.Method)
		return true
	case s.State == update.PhaseOK:
		if cur == nil || !sameAttempt(cur, s) {
			return false
		}
		if s.RebootRequired {
			h.restartPendingLocked(n, cur, now)
			return true
		}
		h.resolveUpdateLocked(n, now)
		return true
	case failedState(s.State):
		if cur != nil && !sameAttempt(cur, s) && activeUpdate(cur) {
			return false // an older attempt's failure: not this one's
		}
		if cur == nil && (s.At == nil || now.Sub(s.At.AsTime()) > time.Hour) {
			return false // a stale result the hub never tracked
		}
		u := cur
		if u == nil || !sameAttempt(u, s) {
			u = &pb.NodeUpdate{From: s.From, To: s.To, Attempt: att, StartedAt: s.Started, Reported: true}
			n.upd = u
		}
		if u.Initiator == "" && updateInitiators[s.Initiator] {
			u.Initiator = s.Initiator
		}
		if u.Method == "" && (s.Method == "msi" || s.Method == "self") {
			u.Method = s.Method
		}
		u.State, u.Error = updFailed, clean(s.Error, 300)
		u.Code = updateCode(s.Code, s.Error)
		if s.State == update.PhaseRolledBack || s.State == update.PhaseUnhealthy {
			u.Code = "health"
		}
		u.FinishedAt, u.UpdatedAt, u.Deadline, u.OfflineSince = tspb(now), tspb(now), nil, nil
		n.rec.UpdateResolved = u.Attempt
		h.log.Warn("node update failed", "node", n.rec.Name, "to", u.To, "code", u.Code, "err", u.Error)
		return true
	}
	return false
}

// mergeProgress copies a progress report into u.
func (h *Hub) mergeProgress(u *pb.NodeUpdate, s *pb.UpdateStatus, now time.Time) {
	prevPhase, prevDone := u.Phase, u.BytesDone
	if u.State == updRequested || u.State == updTimedOut {
		u.State = updUpdating
	}
	if s.Attempt != "" {
		u.Attempt = clean(s.Attempt, 80)
	}
	u.Reported = true
	if p := phaseOf(s); p != "" {
		u.Phase = p
	}
	if updateInitiators[s.Initiator] {
		u.Initiator = s.Initiator
	}
	if s.Method == "msi" || s.Method == "self" {
		u.Method = s.Method
	}
	u.JobsKeepRunning = u.Method != ""
	if s.From != "" {
		u.From = s.From
	}
	if s.BytesTotal > 0 && s.BytesTotal < 1<<40 && s.BytesDone >= 0 && s.BytesDone <= s.BytesTotal {
		u.BytesDone, u.BytesTotal = s.BytesDone, s.BytesTotal
	} else if u.Phase != prevPhase {
		u.BytesDone, u.BytesTotal = 0, 0
	}
	u.Message = clean(s.Message, 200)
	u.UpdatedAt = tspb(now)
	// The deadline moves with a new phase or real progress, not with a
	// report repeating the same phase.
	if u.Deadline == nil || u.Phase != prevPhase || u.BytesDone > prevDone {
		u.Deadline = tspb(h.deadlineFor(u, now, u.OfflineSince != nil))
	}
}

// restartPendingLocked: installed, the machine must restart (msiexec 3010).
func (h *Hub) restartPendingLocked(n *node, u *pb.NodeUpdate, now time.Time) {
	u.State, u.Phase, u.Deadline, u.FinishedAt, u.UpdatedAt = updRestartPending, "", nil, tspb(now), tspb(now)
	n.rec.UpdateResolved = u.Attempt
	n.rec.UpdateBootAt = now
	if up := n.metrics.GetUptimeSeconds(); up > 0 {
		n.rec.UpdateBootAt = now.Add(-time.Duration(up) * time.Second)
	}
	h.publishNodeLocked(n, "update to "+u.To+" needs a restart of the machine")
}

// resolveUpdateLocked ends n's update successfully (the state goes).
func (h *Hub) resolveUpdateLocked(n *node, now time.Time) {
	if u := n.upd; u != nil {
		n.rec.UpdateResolved = u.Attempt
		h.log.Info("node updated", "node", n.rec.Name, "from", u.From, "to", u.To, "took", now.Sub(u.GetStartedAt().AsTime()).Round(time.Second))
	}
	n.upd = nil
}

// updateHelloLocked handles a (re)connecting worker's hello: its outcome
// first (a failure, a health check, a restart pending: the evidence),
// then its version against the update in progress. Reports whether the
// state changed.
func (h *Hub) updateHelloLocked(n *node, hello *pb.NodeHello, now time.Time) bool {
	changed := false
	if s := hello.Update; s != nil {
		changed = h.updateReportLocked(n, s, now)
	}
	u := n.upd
	if u == nil {
		return changed
	}
	if u.OfflineSince != nil {
		u.OfflineSince, changed = nil, true
	}
	if !activeUpdate(u) {
		return changed
	}
	v := hello.Version
	healthCheck := hello.Update != nil && hello.Update.State == update.PhaseChecking && hello.Update.To == u.To
	switch {
	case v == u.To && healthCheck:
		// the new version runs its health check: still updating
		if u.State != updUpdating || u.Phase != "health_check" {
			u.State, u.Phase = updUpdating, "health_check"
			u.Deadline = tspb(h.deadlineFor(u, now, false))
			changed = true
		}
	case v == u.To:
		h.resolveUpdateLocked(n, now)
		changed = true
	case v == u.From || u.From == "":
		if u.State == updTimedOut {
			// back on the old version without a result: it didn't happen
			u.State, u.Code, u.FinishedAt, u.Deadline = updFailed, "timeout", tspb(now), nil
			if u.Error == "" {
				u.Error = fmt.Sprintf("the update to %s didn't finish; the worker is back on %s", u.To, v)
			}
			n.rec.UpdateResolved = u.Attempt
			changed = true
		}
		// else: back on the old version while the update runs (a network
		// blip during the download) or before its result is recorded (the
		// installer rolled back, the service reports it in a moment):
		// keep it until a result or the deadline.
	default:
		u.State, u.Code, u.FinishedAt, u.Deadline = updMismatch, "version", tspb(now), nil
		u.Error = fmt.Sprintf("came back on %s, not %s", v, u.To)
		n.rec.UpdateResolved = u.Attempt
		h.log.Warn("node update: unexpected version", "node", n.rec.Name, "want", u.To, "got", v)
		changed = true
	}
	return changed
}

// updateOfflineLocked: n's session ended. An update in progress goes on:
// the worker is away restarting (its deadline is the restart budget).
func (h *Hub) updateOfflineLocked(n *node, now time.Time) {
	u := n.upd
	if u == nil || (u.State != updUpdating && u.State != updRequested) {
		return
	}
	u.OfflineSince = tspb(now)
	d := h.deadlineFor(u, now, true)
	switch u.Phase {
	case "installing", "restarting", "health_check", "":
		u.Deadline = tspb(d) // the restart it announced: it has the restart budget to come back
	default:
		if u.Deadline == nil || u.Deadline.AsTime().Before(d) {
			u.Deadline = tspb(d) // went away mid-download: whichever is later
		}
	}
	h.saveUpdateLocked(n)
}

// updateRequestedLocked: the hub told n to update to target (a rollout).
// Workers that report (0.1.30+, and every self-updater's "applying") turn
// it into "updating"; one that doesn't take it up within the requested
// timeout while online loses it quietly.
func (h *Hub) updateRequestedLocked(n *node, target string, now time.Time) {
	if n.rec.Version == target || n.rec.UpdateHintDropped == target {
		return
	}
	if u := n.upd; u != nil && activeUpdate(u) {
		return // it is updating already (whoever asked)
	}
	u := &pb.NodeUpdate{State: updRequested, From: n.rec.Version, To: target, Initiator: "rollout", Attempt: "rollout:" + target,
		StartedAt: tspb(now), UpdatedAt: tspb(now)}
	if n.rec.UpdateEnabled {
		u.Method, u.JobsKeepRunning = "self", true
	}
	u.Deadline = tspb(h.deadlineFor(u, now, n.sess == nil))
	if n.sess == nil {
		u.OfflineSince = tspb(now)
	}
	n.upd = u
	h.saveUpdateLocked(n)
	h.publishNodeLocked(n, "update to "+target+" requested")
}

// sweepUpdatesLocked times out updates past their deadline and forgets
// old results (every second).
func (h *Hub) sweepUpdatesLocked(now time.Time) {
	for _, n := range h.nodes {
		u := n.upd
		if u == nil {
			continue
		}
		if n.rec.State == stateRevoked {
			n.upd = nil
			h.saveUpdateLocked(n)
			continue
		}
		switch u.State {
		case updRequested, updUpdating:
			if u.Deadline == nil || now.Before(u.Deadline.AsTime()) {
				continue
			}
			if u.State == updRequested && !u.Reported && n.sess != nil && n.rec.Version == u.From {
				// online, still on the old version, never started: it
				// didn't take the version (skipped, or it can't)
				h.log.Info("node did not take up the rollout's version", "node", n.rec.Name, "version", u.To)
				n.rec.UpdateHintDropped, n.upd = u.To, nil
				h.saveUpdateLocked(n)
				h.publishNodeLocked(n, "update to "+u.To+" not taken up")
				continue
			}
			u.State, u.Code, u.UpdatedAt = updTimedOut, "timeout", tspb(now)
			h.log.Warn("node update didn't finish", "node", n.rec.Name, "to", u.To, "phase", u.Phase, "started", u.GetStartedAt().AsTime())
			h.saveUpdateLocked(n)
			h.event(context.Background(), "node.update_timed_out", map[string]string{"phase": u.Phase, "to": u.To, "method": u.Method})
			h.publishNodeLocked(n, "update to "+u.To+" didn't finish")
		case updFailed, updMismatch:
			if u.FinishedAt != nil && now.Sub(u.FinishedAt.AsTime()) > updateResultTTL {
				n.upd = nil
				h.saveUpdateLocked(n)
				h.publishNodeLocked(n, "update result expired")
			}
		}
	}
}

// updateMetricsLocked ends a restart_pending once the machine booted again
// after the update (its uptime).
func (h *Hub) updateMetricsLocked(n *node, m *pb.MetricsSnapshot, now time.Time) {
	u := n.upd
	if u == nil || u.State != updRestartPending || m.GetUptimeSeconds() <= 0 {
		return
	}
	boot := now.Add(-time.Duration(m.UptimeSeconds) * time.Second)
	if boot.After(n.rec.UpdateBootAt.Add(2 * time.Minute)) {
		n.rec.UpdateRestarted = u.Attempt // its 3010 result, reported again, is done
		n.upd = nil
		n.rec.UpdateBootAt = time.Time{}
		h.saveUpdateLocked(n)
		h.publishNodeLocked(n, "restarted: update finished")
	}
}

// updateInfoLocked is NodeInfo.update of n: its own state, else its
// companion's MSI update (one MSI restarts both workers of a machine).
func (h *Hub) updateInfoLocked(n *node) *pb.NodeUpdate {
	if n.rec.State == stateRevoked || n.rec.Role != "worker" {
		return nil
	}
	if n.upd != nil {
		return proto.Clone(n.upd).(*pb.NodeUpdate)
	}
	if c := h.companionLocked(n); c != nil && activeUpdate(c.upd) && c.upd.Method == "msi" {
		u := proto.Clone(c.upd).(*pb.NodeUpdate)
		u.Via = c.rec.Name
		if n.sess == nil && u.OfflineSince == nil {
			u.OfflineSince = tspb(n.rec.LastSeen)
		}
		return u
	}
	return nil
}

// updateView: what a viewer who is neither the worker's owner nor an
// admin sees of its update - the state, phase and a short code, not the
// worker's own words (paths, hub addresses) nor the companion's name.
func updateView(u *pb.NodeUpdate) {
	if u == nil {
		return
	}
	u.Error, u.Message, u.Via = "", "", ""
}
