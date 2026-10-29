package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/doctor"
	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Run modes of the local worker.
const (
	modeSession = "session" // user-session worker: a scheduled task at logon (Windows)
	modeService = "service" // a system service
	modeNone    = ""        // neither: started by hand (longarmd run)
)

// workerEnv is what longarmd doctor looks at (faked in tests).
type workerEnv interface {
	// Cert: the worker certificate's identity and end; an error when
	// there is none (not enrolled).
	Cert() (pki.Identity, time.Time, error)
	// Status of the running longarmd (an error when it doesn't answer).
	Status(ctx context.Context) (*pb.WorkerStatus, error)
	// RunMode: session, service or "" and whether its task / service is
	// registered, and its state ("running", "stopped", ...).
	RunMode() (mode string, registered bool, state string)
	// Logs: the log files that exist.
	Logs() []string
	// RecentErrors: the last ERROR lines of longarmd's log since a time.
	RecentErrors(since time.Time) []string
}

func doctorCmd() *cobra.Command {
	var dataDir string
	var asJSON bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check this worker: enrolled, certificate, running, hub link, run mode, logs, recent errors; says what to run next",
		Long: `Check this machine's worker and print a checklist; every line that isn't
ok ends with the exact command to run next ("next: ...").

Checks: enrolled (a worker certificate) and until when it is valid;
longarmd running (local control API) and its hub link; how it is started
(user-session scheduled task or service) and whether that is registered;
its log files; ERROR lines in longarmd.log of the last 24 hours.

--json prints the report for programs (docs/usage.md#doctor---json). Exit
code: 0 without failures (warnings allowed), 1 with failures.

The client side has its own: longarm doctor.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := filepath.Abs(dataDir)
			if err != nil {
				return err
			}
			r := doctor.New("longarmd", version.Version, time.Now())
			r.Add(workerChecks(cmd.Context(), &realWorkerEnv{dataDir: dir}, time.Now())...)
			if asJSON {
				err = r.WriteJSON(os.Stdout)
			} else {
				err = r.WriteText(os.Stdout)
			}
			if err != nil {
				return err
			}
			if !r.OK {
				os.Exit(1)
			}
			return nil
		},
	}
	c.Flags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory; env LONGARM_DATA_DIR")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output (docs/usage.md#doctor---json)")
	return c
}

// workerCertWarn: the certificate check warns this close to the end.
const workerCertWarn = 72 * time.Hour

// workerChecks runs the checklist of longarmd doctor.
func workerChecks(ctx context.Context, env workerEnv, now time.Time) []doctor.Check {
	var out []doctor.Check
	id, notAfter, err := env.Cert()
	enrolled := err == nil
	if enrolled {
		out = append(out, doctor.Check{ID: "signed-in", Title: "enrolled", Status: doctor.OK,
			Detail: fmt.Sprintf("as worker %s", id.Name), Data: map[string]any{"name": id.Name}})
		out = append(out, doctor.CertCheck(notAfter, now, workerCertWarn, "longarmd login --force"))
	} else {
		out = append(out, doctor.Check{ID: "signed-in", Title: "enrolled", Status: doctor.Fail,
			Detail: "no worker certificate (" + oneLineErr(err) + ")", Fix: "longarmd login"})
	}

	mode, registered, state := env.RunMode()
	start := startCommand(mode, runtime.GOOS)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	st, err := env.Status(cctx)
	cancel()
	if err != nil {
		out = append(out, doctor.Check{ID: "running", Title: "longarmd", Status: doctor.Fail, Detail: "not running", Fix: start})
		out = append(out, doctor.Check{ID: "hub", Title: "hub link", Status: doctor.Skip, Detail: "longarmd isn't running"})
	} else {
		out = append(out, doctor.Check{ID: "running", Title: "longarmd", Status: doctor.OK,
			Detail: fmt.Sprintf("running (pid %d, %s), %d jobs, %d sessions", st.Pid, st.Version, st.JobsRunning, st.SessionsRunning),
			Data:   map[string]any{"pid": st.Pid, "version": st.Version}})
		out = append(out, hubLinkCheck(st))
	}

	rm := doctor.Check{ID: "run-mode", Title: "run mode", Data: map[string]any{"mode": mode, "registered": registered, "state": state}}
	switch mode {
	case modeSession:
		if registered {
			rm.Status, rm.Detail = doctor.OK, "user-session worker: a scheduled task starts it at your logon"
		} else {
			rm.Status, rm.Detail, rm.Fix = doctor.Fail, "user-session worker, but its scheduled task is missing", "longarmd session-mode install"
		}
	case modeService:
		switch {
		case !registered:
			rm.Status, rm.Detail, rm.Fix = doctor.Fail, "service mode, but the service is not installed", "longarmd service install --user <account>  # elevated"
		case state != "" && state != string(platform.ServiceRunning):
			rm.Status, rm.Detail, rm.Fix = doctor.Fail, "service "+state, start
		default:
			rm.Status, rm.Detail = doctor.OK, "service ("+state+")"
		}
	default:
		rm.Status, rm.Detail, rm.Fix = doctor.Warn, "started by hand: nothing starts it at boot or logon", startByHandFix()
	}
	out = append(out, rm)

	logs := env.Logs()
	lc := doctor.Check{ID: "logs", Title: "logs", Status: doctor.Info, Data: map[string]any{"files": logs}}
	if len(logs) == 0 {
		lc.Detail = "no log file (a service on Linux / macOS logs to the system journal)"
	} else {
		lc.Detail = strings.Join(logs, ", ")
	}
	out = append(out, lc)

	errs := env.RecentErrors(now.Add(-24 * time.Hour))
	ec := doctor.Check{ID: "errors", Title: "recent errors", Data: map[string]any{"lines": errs}}
	if len(errs) == 0 {
		ec.Status, ec.Detail = doctor.OK, "none in the last 24h"
	} else {
		ec.Status, ec.Fix = doctor.Warn, "longarmd diag --upload"
		ec.Detail = fmt.Sprintf("%d in the last 24h, last: %s", len(errs), errs[len(errs)-1])
	}
	out = append(out, ec)
	return out
}

func hubLinkCheck(st *pb.WorkerStatus) doctor.Check {
	c := doctor.Check{ID: "hub", Title: "hub link"}
	h := st.GetHub()
	switch {
	case h.GetAddress() == "":
		c.Status, c.Detail, c.Fix = doctor.Fail, "no hub configured", "longarmd login"
	case h.Connected:
		c.Status, c.Detail = doctor.OK, h.Address+" connected ("+h.NodeState+")"
		if h.NodeState == "pending" {
			c.Status, c.Fix = doctor.Warn, "longarm admin approve "+st.Name+"  # an admin"
		}
	default:
		c.Status, c.Detail, c.Fix = doctor.Fail, h.Address+" disconnected", "longarmd diag --upload"
		if h.Error != "" {
			c.Detail += ": " + h.Error
			if strings.Contains(strings.ToLower(h.Error), "certificate") || strings.Contains(h.Error, "revoked") {
				c.Fix = "longarmd login --force"
			}
		}
	}
	return c
}

// startCommand is how to start the local worker in its run mode.
func startCommand(mode, goos string) string {
	switch {
	case mode == modeSession:
		return "longarmd session-mode start"
	case mode == modeService && goos == "windows":
		return "longarmd service start  # elevated"
	case mode == modeService:
		return "sudo longarmd service start"
	case goos == "windows":
		return "longarmd session-mode install  (or in the foreground: longarmd run)"
	}
	return "sudo longarmd service install --user $USER  (or in the foreground: longarmd run)"
}

// notRunningHelp completes "longarmd is not running": how to start it in
// its run mode, and where its logs are.
func notRunningHelp(err error, mode string, logs []string, goos string) error {
	msg := err.Error() + "\n  start it: " + startCommand(mode, goos)
	if len(logs) > 0 {
		msg += "\n  logs: " + strings.Join(logs, ", ")
	}
	msg += "\n  checklist: longarmd doctor"
	return errors.New(msg)
}

func oneLineErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// realWorkerEnv is workerEnv on this machine.
type realWorkerEnv struct{ dataDir string }

func (e *realWorkerEnv) Cert() (pki.Identity, time.Time, error) {
	b, err := pki.LoadBundle(filepath.Join(e.dataDir, "tls"))
	if err != nil {
		return pki.Identity{}, time.Time{}, err
	}
	if b.Identity.Role != pki.RoleWorker {
		return pki.Identity{}, time.Time{}, fmt.Errorf("%s holds a %s certificate, not a worker one", filepath.Join(e.dataDir, "tls"), b.Identity.Role)
	}
	var na time.Time
	if l := b.Leaf(); l != nil {
		na = l.NotAfter
	}
	return b.Identity, na, nil
}

func (e *realWorkerEnv) Status(ctx context.Context) (*pb.WorkerStatus, error) {
	c, closeFn, err := localClient(e.dataDir)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	return c.Status(ctx, &pb.LocalStatusRequest{})
}

func (e *realWorkerEnv) RunMode() (string, bool, string) { return detectRunMode() }

// detectRunMode: the installer's record (Windows), else a registered
// service or user-session task.
func detectRunMode() (mode string, registered bool, state string) {
	mode, _ = longarmd.InstalledWorker()
	svc, svcErr := platform.QueryService(defaultServiceName)
	svcThere := svcErr == nil && svc != platform.ServiceNotInstalled
	task := false
	if sid, err := longarmd.CurrentUserSID(); err == nil {
		task = longarmd.Schtasks{}.Exists(longarmd.SessionTaskName(sid))
	}
	if mode == "" {
		switch {
		case svcThere:
			mode = modeService
		case task:
			mode = modeSession
		}
	}
	switch mode {
	case modeService:
		return mode, svcThere, string(svc)
	case modeSession:
		return mode, task, ""
	}
	return mode, false, ""
}

func (e *realWorkerEnv) Logs() []string { return workerLogs(e.dataDir) }

func workerLogs(dataDir string) []string {
	var out []string
	for _, f := range []string{longarmd.LogFileName, longarmd.SessionLogFile} {
		p := filepath.Join(dataDir, f)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func (e *realWorkerEnv) RecentErrors(since time.Time) []string {
	f, err := os.Open(filepath.Join(e.dataDir, longarmd.LogFileName))
	if err != nil {
		return nil
	}
	defer f.Close()
	return recentErrors(f, since, 5)
}

// recentErrors returns up to max of the last "level=ERROR" lines (slog's
// text format) at or after since, from the last 1 MiB of r.
func recentErrors(r io.ReadSeeker, since time.Time, max int) []string {
	const tail = 1 << 20
	if end, err := r.Seek(0, io.SeekEnd); err == nil && end > tail {
		r.Seek(end-tail, io.SeekStart)
	} else {
		r.Seek(0, io.SeekStart)
	}
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "level=ERROR") {
			continue
		}
		if ts, ok := strings.CutPrefix(line, "time="); ok {
			if sp := strings.IndexByte(ts, ' '); sp > 0 {
				if t, err := time.Parse(time.RFC3339Nano, ts[:sp]); err == nil && t.Before(since) {
					continue
				}
			}
		}
		if len(line) > 200 {
			line = line[:197] + "..."
		}
		out = append(out, line)
		if len(out) > max {
			out = out[1:]
		}
	}
	return out
}

// startByHandFix: how to make a worker started by hand start by itself.
func startByHandFix() string {
	if runtime.GOOS == "windows" {
		return "longarmd session-mode install"
	}
	return "sudo longarmd service install --user $USER"
}
