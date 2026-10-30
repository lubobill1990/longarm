package tray

// The tray menu's machine actions (docs/usage.md "Tray"): closing the tray
// only closes the tray - this machine keeps working (its worker serves
// jobs and sessions, the client daemon keeps your forwards) - so the menu
// says so, and offers the two things people mean by "quit": pause the
// worker (no new jobs; running ones finish), or quit Longarm on this
// machine (the session worker until the next sign-in, the client daemon,
// the tray), after a confirmation that says what is running.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/localapi"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// MachineState is what the tray menu shows of this machine.
type MachineState struct {
	Worker         bool  // a local worker answers
	Draining       bool  // paused: no new jobs
	Jobs, Sessions int32 // running on the local worker
	Forwards       int   // the client daemon's port forwards
	SessionWorker  bool  // the worker is this user's session worker (not the service)
}

const pausedReason = "paused from the tray"

func (s *Server) localWorker(ctx context.Context, fn func(context.Context, pb.LocalWorkerClient) error) error {
	cc, err := localapi.DialFile(filepath.Join(s.opts.LongarmdDataDir, "longarmd.local"), "longarmd")
	if err != nil {
		return err
	}
	defer cc.Close()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return fn(cctx, pb.NewLocalWorkerClient(cc))
}

// MachineState asks the local worker and the client daemon (either may be
// absent: zero values).
func (s *Server) MachineState(ctx context.Context) MachineState {
	var st MachineState
	if s.opts.LongarmdDataDir != "" {
		s.localWorker(ctx, func(ctx context.Context, c pb.LocalWorkerClient) error {
			w, err := c.Status(ctx, &pb.LocalStatusRequest{})
			if err == nil {
				st.Worker = true
				st.Draining = w.GetState().GetDraining()
				st.Jobs, st.Sessions = w.JobsRunning, w.SessionsRunning
			}
			return err
		})
	}
	if s.installedWorker != nil {
		mode, _ := s.installedWorker()
		st.SessionWorker = mode == "session"
	}
	if s.opts.LongarmConfigDir != "" {
		if cc, err := localapi.DialFile(filepath.Join(s.opts.LongarmConfigDir, "daemon.addr"), "longarm daemon"); err == nil {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if r, err := pb.NewLocalClientClient(cc).ListForwards(cctx, &pb.ListForwardsRequest{}); err == nil {
				st.Forwards = len(r.Forwards)
			}
			cancel()
			cc.Close()
		}
	}
	return st
}

// PauseWorker pauses (drains) or resumes the local worker: paused, the
// hub places no new work on it; running jobs and sessions continue.
func (s *Server) PauseWorker(ctx context.Context, pause bool) error {
	return s.localWorker(ctx, func(ctx context.Context, c pb.LocalWorkerClient) error {
		r := &pb.LocalDrainRequest{Draining: pause}
		if pause {
			r.Reason = pausedReason
		}
		_, err := c.SetDrain(ctx, r)
		return err
	})
}

// QuitSummary is the confirmation of "Quit Longarm on this machine".
func QuitSummary(st MachineState) string {
	var b strings.Builder
	b.WriteString("This stops ")
	var parts []string
	if st.Worker && st.SessionWorker {
		parts = append(parts, "this machine's worker until you sign in to Windows again")
	}
	parts = append(parts, "the Longarm client daemon", "the tray")
	b.WriteString(strings.Join(parts, ", "))
	b.WriteString(".")
	var running []string
	if st.Jobs > 0 {
		running = append(running, plural(int(st.Jobs), "job"))
	}
	if st.Sessions > 0 {
		running = append(running, plural(int(st.Sessions), "session"))
	}
	if len(running) > 0 && st.SessionWorker {
		fmt.Fprintf(&b, "\n\n%s running on this machine keep running, but nobody can reach them until the worker is back.", strings.Join(running, " and "))
	}
	if st.Forwards > 0 {
		fmt.Fprintf(&b, "\n\nYour %s will stop.", plural(st.Forwards, "port forward"))
	}
	if st.Worker && !st.SessionWorker {
		b.WriteString("\n\nThe worker runs as a Windows service and keeps running (pause it to take no new jobs).")
	}
	return b.String()
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// QuitLongarm stops this user's Longarm processes: the session worker
// (until the next sign-in: its scheduled task starts it again), and the
// client daemon. The caller then quits the tray. The worker service, the
// update service and other users' processes are left alone. It returns
// what failed (empty: everything stopped).
func (s *Server) QuitLongarm(st MachineState) []string {
	var problems []string
	if st.SessionWorker && s.opts.LongarmdExe != "" {
		if out, err := s.runCmd(s.opts.LongarmdExe, "session-mode", "stop"); err != nil {
			problems = append(problems, "stop the worker: "+strings.TrimSpace(string(out)+" "+err.Error()))
		}
	}
	if s.opts.LongarmExe != "" {
		if out, err := s.runCmd(s.opts.LongarmExe, s.longarmArgs("daemon", "stop")[1:]...); err != nil && !strings.Contains(string(out), "not running") {
			problems = append(problems, "stop the client daemon: "+strings.TrimSpace(string(out)+" "+err.Error()))
		}
	}
	return problems
}
