package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// longarm worker update: the owner updates a worker's machine to a newer
// signed release (docs/usage.md "Updating"), end to end - the request goes
// to the worker itself, never through a hub command. An MSI install
// (Windows) hands it to the Longarm Update service, which installs the
// hub's newer, signed MSI (the user-session worker and the admin worker
// restart with it; running jobs and sessions keep running); other installs
// update longarmd itself.

// updateOutcome is longarm worker update's result (--json; the tray).
type updateOutcome struct {
	Worker string `json:"worker"`
	Method string `json:"method,omitempty"` // msi | self
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	// Phase: up_to_date | requested (not waited for) | done | failed
	Phase        string `json:"phase"`
	Summary      string `json:"summary"`
	Jobs         int    `json:"jobs"`          // running when the update started
	KeptJobs     int    `json:"kept_jobs"`     // of those, still running after it
	Sessions     int    `json:"sessions"`      // running when the update started
	KeptSessions int    `json:"kept_sessions"` // of those, still running after it
	Companion    string `json:"companion,omitempty"`
	// Via: the admin worker that took the request (a user-session worker
	// too old to reach the machine's Longarm Update service itself)
	Via   string `json:"via,omitempty"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// updateCodeExit maps a MachineUpdateEvent code to longarm's exit code.
func updateCodeExit(code string) int {
	switch code {
	case "refused", "invalid", "unsupported":
		return exitUsage
	case "not_found":
		return exitNotFound
	case "busy", "rate_limited":
		return exitTempFail
	case "denied":
		return exitNoPerm
	}
	return exitLongarmError
}

// errTooOld: the worker doesn't serve MachineUpdate.
func errTooOld(worker, ver string) error {
	return usagef("%s runs longarmd %s, which can't be updated remotely: update it once on the machine "+
		"(the MSI or the tray's update on Windows, the install script, or the hub's rollout); later versions update with longarm worker update", worker, ver)
}

// runWorkerUpdate asks worker for the update and, with wait, waits for its
// outcome (reconnecting while it restarts). progress hears the phases.
func runWorkerUpdate(ctx context.Context, s *session, worker string, req *pb.MachineUpdateRequest, wait bool,
	timeout time.Duration, progress func(string)) (*updateOutcome, error) {
	out := &updateOutcome{Worker: worker}
	if c := s.companionOf(worker); c != "" {
		out.Companion = c
	}
	conn, err := s.conn(worker)
	if err != nil {
		return out, err
	}
	defer conn.Close()
	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	info, err := conn.Node().Info(ictx, &pb.InfoRequest{})
	cancel()
	if err != nil {
		return out, err
	}
	out.From = info.Version
	if !slices.Contains(info.Features, jobs.FeatureMachineUpdate) {
		return out, errTooOld(worker, info.Version)
	}
	started := time.Now()
	last, runJobs, runSessions, err := startMachineUpdate(ctx, conn, req, out, progress)
	if err != nil {
		return out, err
	}
	if adm := adminFallback(worker, s.updateNodes(ctx, last), last); adm != "" {
		// A user-session worker before 0.1.19 didn't see the Longarm Update
		// service (it asked the service manager for more access than a user
		// has) and tried to update itself: its admin worker - same machine,
		// SYSTEM - hands the request to the service.
		if progress != nil {
			progress(fmt.Sprintf("%s can't reach the Longarm Update service itself (longarmd %s); asking its admin worker %s", worker, out.From, adm))
		}
		if l, err := updateThrough(ctx, s, adm, req, out, progress); err == nil {
			last = l
			out.Via = adm
		} else if progress != nil {
			progress(fmt.Sprintf("through %s: %v", adm, err))
		}
	}
	out.Jobs, out.Sessions = len(runJobs), len(runSessions)
	if last == nil {
		return out, errors.New("the worker ended the update without a word")
	}
	switch last.Phase {
	case "failed":
		out.Phase, out.Code, out.Error = "failed", last.Code, last.Error
		out.Summary = fmt.Sprintf("%s: update failed: %s", worker, last.Error)
		return out, &codedError{code: updateCodeExit(last.Code), err: errors.New(out.Summary)}
	case "up_to_date":
		out.Phase = "up_to_date"
		out.Summary = fmt.Sprintf("%s: already at %s", worker, out.To)
		return out, nil
	}
	if !wait {
		out.Phase = "requested"
		out.Summary = fmt.Sprintf("%s: updating %s → %s (%s); longarm ls shows the new version, longarm worker update %s --wait waits for it",
			worker, out.From, out.To, methodName(out.Method), worker)
		return out, nil
	}
	conn.Close()
	if progress != nil {
		progress("waiting for " + worker + " to come back with " + out.To)
	}
	if err := waitForVersion(ctx, s, worker, out, started, timeout); err != nil {
		out.Phase, out.Error = "failed", err.Error()
		out.Summary = fmt.Sprintf("%s: %s → %s: %v", worker, out.From, out.To, err)
		var ce *codedError
		if errors.As(err, &ce) {
			return out, err
		}
		return out, &codedError{code: exitLongarmError, err: errors.New(out.Summary)}
	}
	out.Phase = "done"
	out.KeptJobs, out.KeptSessions = stillRunning(ctx, s, worker, runJobs, runSessions)
	out.Summary = fmt.Sprintf("%s: %s → %s", worker, out.From, out.To)
	if out.Jobs+out.Sessions > 0 {
		out.Summary += "; " + keptNote(out)
	}
	if out.Companion != "" && out.Method == "msi" {
		out.Summary += "; its admin worker " + out.Companion + " restarted too"
	}
	return out, nil
}

// startMachineUpdate runs MachineUpdate.Start on conn and returns its last
// event and what ran when it started (jobs, sessions).
func startMachineUpdate(ctx context.Context, conn *client.Conn, req *pb.MachineUpdateRequest, out *updateOutcome,
	progress func(string)) (*pb.MachineUpdateEvent, []string, []string, error) {
	st, err := conn.MachineUpdate().Start(ctx, req)
	if err != nil {
		return nil, nil, nil, err
	}
	var last *pb.MachineUpdateEvent
	var runJobs, runSessions []string
	for {
		ev, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if last != nil && last.Phase == "installing" {
				break // the worker restarts with the installer
			}
			return nil, nil, nil, err
		}
		if len(ev.RunningJobs)+len(ev.RunningSessions) > 0 {
			runJobs, runSessions = ev.RunningJobs, ev.RunningSessions
		}
		last = ev
		if ev.Method != "" {
			out.Method = ev.Method
		}
		if ev.From != "" {
			out.From = ev.From
		}
		if ev.To != "" {
			out.To = ev.To
		}
		if ev.Phase != "failed" && progress != nil {
			msg := ev.Phase
			if ev.Message != "" {
				msg += ": " + ev.Message
			}
			progress(msg)
		}
	}
	if last == nil {
		return nil, nil, nil, errors.New("the worker ended the update without a word")
	}
	return last, runJobs, runSessions, nil
}

// updateThrough asks worker (the admin worker of the machine) for the
// update: its last event.
func updateThrough(ctx context.Context, s *session, worker string, req *pb.MachineUpdateRequest, out *updateOutcome,
	progress func(string)) (*pb.MachineUpdateEvent, error) {
	conn, err := s.conn(worker)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	info, err := conn.Node().Info(ictx, &pb.InfoRequest{})
	cancel()
	if err != nil {
		return nil, err
	}
	if !slices.Contains(info.Features, jobs.FeatureMachineUpdate) {
		return nil, errTooOld(worker, info.Version)
	}
	from := out.From
	last, _, _, err := startMachineUpdate(ctx, conn, req, out, progress)
	if from != "" {
		out.From = from // the machine's (the session worker's) version
	}
	return last, err
}

// adminFallback names the admin worker to ask instead when worker (a
// user-session worker) refused to update itself ("unsupported", method
// self): an older longarmd that missed the machine's Longarm Update
// service. "" = none (another failure, no admin worker, worker is one).
func adminFallback(worker string, nodes []*pb.NodeInfo, last *pb.MachineUpdateEvent) string {
	if last == nil || last.Phase != "failed" || last.Code != "unsupported" || last.Method != "self" {
		return ""
	}
	var me *pb.NodeInfo
	byName := map[string]*pb.NodeInfo{}
	for _, n := range nodes {
		byName[n.Name] = n
		if n.Name == worker {
			me = n
		}
	}
	if me == nil || isAdminWorker(me) {
		return ""
	}
	for _, name := range []string{me.Companion, worker + "-admin"} {
		if c := byName[name]; name != "" && c != nil && isAdminWorker(c) && c.Os == "windows" {
			return c.Name
		}
	}
	return ""
}

func isAdminWorker(n *pb.NodeInfo) bool {
	return n.AdminWorker || n.Labels[sched.AdminLabel] == "true"
}

// updateNodes: the hub's workers when an update may need its admin
// worker (adminFallback), else nil. The cache when the hub is down.
func (s *session) updateNodes(ctx context.Context, last *pb.MachineUpdateEvent) []*pb.NodeInfo {
	if last == nil || last.Code != "unsupported" {
		return nil
	}
	if nodes, ok := s.hubNodes(ctx, false); ok {
		return nodes
	}
	return s.cachedNodes()
}

func methodName(m string) string {
	if m == "msi" {
		return "the Longarm Update service installs the MSI"
	}
	return "longarmd updates itself"
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// keptNote: "2 jobs and 1 session kept running" (or how many of them).
func keptNote(o *updateOutcome) string {
	var parts []string
	if o.Jobs > 0 {
		if o.KeptJobs == o.Jobs {
			parts = append(parts, plural(o.Jobs, "job"))
		} else {
			parts = append(parts, fmt.Sprintf("%d of %s", o.KeptJobs, plural(o.Jobs, "job")))
		}
	}
	if o.Sessions > 0 {
		if o.KeptSessions == o.Sessions {
			parts = append(parts, plural(o.Sessions, "session"))
		} else {
			parts = append(parts, fmt.Sprintf("%d of %s", o.KeptSessions, plural(o.Sessions, "session")))
		}
	}
	return strings.Join(parts, " and ") + " kept running"
}

// waitForVersion reconnects until the worker runs out.To (or reports the
// install failed).
func waitForVersion(ctx context.Context, s *session, worker string, out *updateOutcome, started time.Time, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not come back with %s within %s (longarm ls shows its version)", worker, out.To, timeout)
		case <-time.After(3 * time.Second):
		}
		conn, err := s.conn(worker)
		if err != nil {
			continue
		}
		ictx, icancel := context.WithTimeout(ctx, 10*time.Second)
		info, err := conn.Node().Info(ictx, &pb.InfoRequest{})
		var stat *pb.MachineUpdateEvent
		if err == nil && slices.Contains(info.Features, jobs.FeatureMachineUpdate) {
			stat, _ = conn.MachineUpdate().Status(ictx, &pb.MachineUpdateStatusRequest{})
		}
		icancel()
		conn.Close()
		if err != nil {
			continue
		}
		if info.Version == out.To {
			return nil
		}
		if l := stat; l != nil && l.LastTo == out.To && !l.LastOk && l.LastAt != nil && l.LastAt.AsTime().After(started.Add(-time.Minute)) {
			return &codedError{code: exitLongarmError, err: fmt.Errorf("the install failed: %s", l.LastError)}
		}
	}
}

// stillRunning counts the jobs and sessions of before that still run.
func stillRunning(ctx context.Context, s *session, worker string, jobIDs, sessions []string) (int, int) {
	if len(jobIDs)+len(sessions) == 0 {
		return 0, 0
	}
	conn, err := s.conn(worker)
	if err != nil {
		return 0, 0
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	running := map[string]bool{}
	if r, err := conn.Jobs().List(ctx, &pb.ListJobsRequest{States: []pb.JobState{pb.JobState_JOB_STATE_RUNNING, pb.JobState_JOB_STATE_PREPARING}}); err == nil {
		for _, j := range r.Jobs {
			running[j.GetSpec().GetId()] = true
		}
	}
	if r, err := conn.Sessions().List(ctx, &pb.ListSessionsRequest{}); err == nil {
		for _, ss := range r.Sessions {
			if ss.State == pb.JobState_JOB_STATE_RUNNING || ss.State == pb.JobState_JOB_STATE_PREPARING {
				running[ss.Name] = true
			}
		}
	}
	kj, ks := 0, 0
	for _, id := range jobIDs {
		if running[id] {
			kj++
		}
	}
	for _, id := range sessions {
		if running[id] {
			ks++
		}
	}
	return kj, ks
}

// companionOf is worker's machine companion from the hub's list (cached).
func (s *session) companionOf(worker string) string {
	for _, n := range s.cachedNodes() {
		if n.Name == worker {
			return n.Companion
		}
	}
	return ""
}

func workerUpdateCmd() *cobra.Command {
	var ver, auto string
	var latest, wait, asJSON bool
	var hours int
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "update <worker> [--latest | --version X] [--wait] [--json]",
		Short: "Update a worker's machine to a newer signed release (its owner only)",
		Long: `Update the machine a worker of yours runs on to the hub's latest stable
release (default) or --version X. Only the worker's owner may: the request
goes to the worker itself, end to end - the hub can't ask for it.

On Windows with the MSI, the Longarm Update service installs the newer
MSI from the hub (checked: signed with the release key built into it, the
version newer than the installed one, for this machine): the worker and
its admin worker restart, running jobs and sessions keep running. Other
installs (install.ps1 per user, Linux, macOS) update longarmd itself.

--wait waits until the worker runs the new version and says what kept
running ("cpc-1: 0.1.8 → 0.1.9; 2 jobs and 1 session kept running").
--auto on|off turns automatic updates of an MSI install on or off (every
--hours, default 6; the machine's owner only, never the hub).

Exit codes: 0 updated (or up to date, or requested without --wait),
2 refused (a downgrade, a worker too old to update remotely), 3 no such
worker or version, 75 busy or rate limited (retry later), 77 not the
owner, 255 the update failed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			worker := args[0]
			if ver != "" && latest {
				return usagef("--version or --latest, not both")
			}
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			if auto != "" {
				if auto != "on" && auto != "off" {
					return usagef("--auto on|off")
				}
				return setAutoUpdate(cmd.Context(), s, worker, auto == "on", hours, asJSON)
			}
			req := &pb.MachineUpdateRequest{Version: ver, Latest: ver == ""}
			var progress func(string)
			if !asJSON {
				progress = func(m string) { fmt.Fprintf(os.Stderr, "%s: %s\n", worker, m) }
			}
			out, err := runWorkerUpdate(cmd.Context(), s, worker, req, wait, timeout, progress)
			if asJSON {
				if err != nil && out.Error == "" {
					out.Phase, out.Error = "failed", err.Error()
				}
				b, _ := json.MarshalIndent(out, "", "  ")
				fmt.Println(string(b))
			} else if err == nil {
				fmt.Println(out.Summary)
			}
			return err
		},
	}
	c.Flags().StringVar(&ver, "version", "", "the version to install (the hub must have it; never older than the installed one)")
	c.Flags().BoolVar(&latest, "latest", false, "the hub's latest stable release (the default)")
	c.Flags().BoolVar(&wait, "wait", false, "wait for the worker to run the new version")
	c.Flags().BoolVar(&asJSON, "json", false, "print the outcome as JSON")
	c.Flags().DurationVar(&timeout, "timeout", 20*time.Minute, "with --wait: how long to wait")
	c.Flags().StringVar(&auto, "auto", "", "on|off: automatic updates of the machine (MSI installs)")
	c.Flags().IntVar(&hours, "hours", 0, "with --auto on: hours between checks (default 6)")
	return c
}

func setAutoUpdate(ctx context.Context, s *session, worker string, on bool, hours int, asJSON bool) error {
	conn, err := s.conn(worker)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := needUpdateFeature(ctx, conn, worker); err != nil {
		return err
	}
	ev, err := conn.MachineUpdate().SetAuto(ctx, &pb.MachineUpdateAutoRequest{Enabled: on, IntervalHours: int32(hours)})
	if err != nil {
		return err
	}
	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{"worker": worker, "auto": ev.Auto, "interval_hours": ev.AutoIntervalHours,
			"code": ev.Code, "error": ev.Error}, "", "  ")
		fmt.Println(string(b))
	}
	if ev.Phase == "failed" {
		return &codedError{code: updateCodeExit(ev.Code), err: fmt.Errorf("%s: %s", worker, ev.Error)}
	}
	if !asJSON {
		state := "off"
		if ev.Auto {
			state = fmt.Sprintf("on (every %d h; installs when no job started for 5 min)", ev.AutoIntervalHours)
		}
		fmt.Printf("%s: automatic updates %s\n", worker, state)
	}
	return nil
}

func needUpdateFeature(ctx context.Context, conn *client.Conn, worker string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info, err := conn.Node().Info(ctx, &pb.InfoRequest{})
	if err != nil {
		return err
	}
	if !slices.Contains(info.Features, jobs.FeatureMachineUpdate) {
		return errTooOld(worker, info.Version)
	}
	return nil
}
