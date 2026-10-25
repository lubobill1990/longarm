// Package shim is the per-job supervisor process (`longarmd shim <jobdir>`), the
// containerd-shim pattern from DESIGN §3.1.
//
// longarmd spawns one detached shim per job. The shim owns the job's process-tree
// container (Windows Job Object with KILL_ON_JOB_CLOSE / process group on
// Unix), starts the command, copies its output through the secret redactor
// into out.log (size-capped, rotating; see package logfile), and records
// status/exit. Because the shim is a separate, detached process, longarmd can
// crash, restart or self-update without affecting running jobs; on restart
// longarmd re-adopts the job by reading status.json and checking the shim's pid +
// start time.
//
// Two modes:
//   - pipe (jobs): stdout+stderr through one pipe, no stdin.
//   - pty (interactive sessions, spec.Pty): the command runs on a pseudo
//     terminal (ConPTY / pty) and the shim serves an attach channel (named
//     pipe / unix socket, address in status.json) that longarmd connects to for
//     each attached client; see session.go.
//
// Control: longarmd -> shim kill requests are file based - a "kill" marker in the
// job dir, polled every 200 ms. Secrets
// arrive once on stdin (JSON map) and live only in this process's memory and
// the child's env.
package shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/logfile"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/redact"
)

// KillPollInterval is how often the shim checks for the kill marker.
const KillPollInterval = 200 * time.Millisecond

// drainTimeout bounds how long the shim waits for the output pipe to close
// after the tree was killed (an escaped process could hold it open).
const drainTimeout = 5 * time.Second

// Main runs the shim for the job in dir and returns the process exit code
// (0 when the job was supervised to completion, whatever its own exit code).
func Main(dir string, stdin io.Reader) int {
	d := jobs.Dir(dir)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("job", d.ID())
	if err := platform.SetupConsole(); err != nil {
		log.Warn("no UTF-8 console for the job", "err", err)
	}
	if err := run(d, stdin, log); err != nil {
		log.Error("shim failed", "err", err)
		return 1
	}
	return 0
}

// job is the state shared by both modes.
type job struct {
	d       jobs.Dir
	spec    *jobs.Spec
	st      *jobs.Status
	log     *slog.Logger
	secrets map[string]string
	logf    *logfile.Writer
	out     *redact.Writer // redacting writer into logf
}

func (j *job) finish(state jobs.State, code int, reason string) error {
	if err := jobs.CheckTransition(j.st.State, state); err != nil {
		return err
	}
	j.st.State, j.st.Reason, j.st.EndedAt = state, reason, time.Now().UTC()
	j.st.ExitCode = &code
	return j.d.WriteStatus(j.st)
}

// failStart records a start failure in the log and the status.
func (j *job) failStart(what string, err error) error {
	fmt.Fprintf(j.out, "longarmd: %s: %v\n", what, err)
	j.out.Flush()
	return j.finish(jobs.Failed, jobs.ExitNotFound, what+": "+err.Error())
}

// workdir is the job's working directory: the prepared worktree (+ cwd
// inside it) for workspace jobs, else cwd, else the home directory.
func (j *job) workdir() string {
	if j.st.Workdir != "" {
		if filepath.IsAbs(j.spec.Cwd) {
			return j.spec.Cwd
		}
		return filepath.Join(j.st.Workdir, filepath.FromSlash(j.spec.Cwd))
	}
	if j.spec.Cwd != "" {
		return j.spec.Cwd
	}
	home, _ := os.UserHomeDir()
	return home
}

// watchKill polls the kill marker until stop is closed; on a request it sets
// killed and kills the container.
func (j *job) watchKill(ctr platform.Container, killed *atomic.Bool, stop <-chan struct{}) {
	t := time.NewTicker(KillPollInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if j.d.KillRequested() {
				killed.Store(true)
				j.log.Info("kill requested")
				if err := ctr.Kill(); err != nil {
					j.log.Error("kill", "err", err)
				}
				return
			}
		}
	}
}

func (j *job) finishExit(killed bool, code int) error {
	switch {
	case killed: // a marker arriving after a natural exit does not count
		return j.finish(jobs.Killed, platform.KilledExitCode, "killed by request")
	case code == 0:
		return j.finish(jobs.Done, 0, "")
	default:
		return j.finish(jobs.Failed, code, fmt.Sprintf("exit status %d", code))
	}
}

func run(d jobs.Dir, stdin io.Reader, log *slog.Logger) error {
	spec, err := d.ReadSpec()
	if err != nil {
		return err
	}
	secrets := map[string]string{}
	if stdin != nil {
		b, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
		if err != nil {
			return fmt.Errorf("read secrets: %w", err)
		}
		if len(b) > 0 {
			if err := json.Unmarshal(b, &secrets); err != nil {
				return fmt.Errorf("decode secrets: %w", err)
			}
		}
	}
	st, err := d.ReadStatus()
	if err != nil {
		return err
	}
	if st.State != jobs.Preparing {
		return fmt.Errorf("job is %s, expected preparing", st.State)
	}
	st.ShimPID = os.Getpid()
	// The start time guards against pid reuse when longarmd later checks whether
	// this shim is alive; without it a dead shim could look alive forever.
	if st.ShimStart, err = platform.ProcessStartTime(st.ShimPID); err != nil || st.ShimStart == 0 {
		return fmt.Errorf("cannot read own process start time: %v", err)
	}
	if err := d.WriteStatus(st); err != nil {
		return err
	}
	j := &job{d: d, spec: spec, st: st, log: log, secrets: secrets}

	if d.KillRequested() {
		return j.finish(jobs.Killed, platform.KilledExitCode, "killed before start")
	}

	if j.logf, err = logfile.OpenWriter(d.Path(jobs.LogFile), spec.LogMaxBytes); err != nil {
		return err
	}
	defer j.logf.Close()
	values := make([]string, 0, len(secrets))
	for _, v := range secrets {
		values = append(values, v)
	}
	j.out = redact.NewWriter(j.logf, redact.New(values))
	j.logf.OnRotateError = func(err error) { log.Warn("log rotation failed; will retry", "err", err) }

	// The child inherits the shim's environment (= longarmd's, i.e. the service
	// account's) plus job env plus secrets. Setting them on the shim itself
	// also makes exec's PATH lookup honour a job-provided PATH.
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		os.Setenv(k, spec.Env[k])
	}
	for k, v := range secrets {
		os.Setenv(k, v)
	}

	ctr, err := platform.NewContainer(platform.ContainerOptions{
		Name: "longarm-" + spec.ID, MemLimitBytes: uint64(spec.MemLimitBytes), KillOnOwnerDeath: true,
	})
	if err != nil {
		return j.failStart("cannot create job container", err)
	}
	defer ctr.Close()
	if spec.Pty {
		return j.runSession(ctr)
	}
	return j.runPipe(ctr)
}

// runPipe runs a batch job: output through one pipe into out.log.
func (j *job) runPipe(ctr platform.Container) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command(j.spec.Argv[0], j.spec.Argv[1:]...)
	cmd.Dir = j.workdir()
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = pw, pw // one pipe: interleaving preserved
	startErr := ctr.Start(cmd)
	pw.Close()
	if startErr != nil {
		pr.Close()
		return j.failStart(fmt.Sprintf("failed to start %q", j.spec.Argv[0]), startErr)
	}

	j.st.State = jobs.Running
	j.st.PID = cmd.Process.Pid
	j.st.PIDStart, _ = platform.ProcessStartTime(j.st.PID)
	j.st.StartedAt = time.Now().UTC()
	if err := j.d.WriteStatus(j.st); err != nil {
		j.log.Error("write running status", "err", err)
	}
	j.log.Info("started", "pid", j.st.PID, "argv", j.spec.Argv)

	dst := &sink{w: j.out, log: j.log}
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(dst, pr)
		copied <- err
	}()

	var killed atomic.Bool
	stopWatch := make(chan struct{})
	go j.watchKill(ctr, &killed, stopWatch)

	waitErr := cmd.Wait()
	close(stopWatch)
	// The main process is gone: the job is over. Kill leftovers so they
	// release the output pipe and do not outlive their job.
	_ = ctr.Kill()
	select {
	case err := <-copied:
		if err != nil && !errors.Is(err, os.ErrClosed) {
			j.log.Warn("copy output", "err", err)
		}
	case <-time.After(drainTimeout):
		j.log.Warn("output pipe still open after kill; closing")
		pr.Close()
		<-copied
	}
	pr.Close()
	dst.Stop()
	if err := j.out.Flush(); err != nil {
		j.log.Error("flush log", "err", err)
	}
	_ = j.logf.Sync()
	return j.finishExit(killed.Load(), exitCode(cmd, waitErr))
}

func exitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState == nil {
		return -1
	}
	if c := cmd.ProcessState.ExitCode(); c >= 0 {
		return c
	}
	return signalExitCode(cmd.ProcessState)
}

// sink is the destination of a job's output copier. After the first write
// error it logs once and discards, so the job's pipe keeps draining (a full
// pipe would block the job forever); Stop turns late writes into no-ops
// before the log is flushed and closed (and waits for one in flight).
type sink struct {
	mu      sync.Mutex
	w       io.Writer
	log     *slog.Logger
	failed  bool
	stopped bool
}

func (s *sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.failed {
		return len(p), nil
	}
	if _, err := s.w.Write(p); err != nil {
		s.failed = true
		s.log.Error("writing the log failed; further output is discarded", "err", err)
	}
	return len(p), nil
}

func (s *sink) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
}
