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
//   - pipe (jobs): stdout+stderr through one pipe; stdin is the job's stdin
//     payload, if any (then end of file), else empty.
//   - pty (interactive sessions, spec.Pty): the command runs on a pseudo
//     terminal (ConPTY / pty) and the shim serves an attach channel (named
//     pipe / unix socket, address in status.json) that longarmd connects to for
//     each attached client; see session.go.
//
// Control: longarmd -> shim kill requests are file based - a "kill" marker in the
// job dir, polled every 200 ms. Secrets
// arrive once on stdin (JSON map) and live only in this process's memory and
// the child's env; the bytes after that JSON value are the job's stdin
// payload (memory only, handed to the child through a pipe).
package shim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
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
	stdin   []byte // the job's stdin payload
	logf    *logfile.Writer
	out     *redact.Writer // redacting writer into logf
	env     []string       // the workload's environment
	home    string         // the workload account's home ("" = the shim's)
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
	if j.home != "" {
		return j.home
	}
	home, _ := os.UserHomeDir()
	return home
}

// command resolves argv[0] with the workload's PATH (per-user isolation:
// the account's, not the shim's).
func (j *job) command(argv []string) []string {
	if j.spec.RunAs == nil || strings.ContainsAny(argv[0], `/\`) {
		return argv
	}
	path := ""
	for _, kv := range j.env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "PATH") {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		if p, err := exec.LookPath(filepath.Join(dir, argv[0])); err == nil {
			return append([]string{p}, argv[1:]...)
		}
	}
	return argv
}

// dedupEnv keeps the last value of every key (case-insensitive on Windows).
func dedupEnv(env []string) []string {
	idx := map[string]int{}
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if runtime.GOOS == "windows" {
			k = strings.ToUpper(k)
		}
		if i, ok := idx[k]; ok {
			out[i] = kv
			continue
		}
		idx[k] = len(out)
		out = append(out, kv)
	}
	return out
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
	secrets, input, err := readInput(stdin)
	if err != nil {
		return err
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
	j := &job{d: d, spec: spec, st: st, log: log, secrets: secrets, stdin: input}
	if n := int64(len(input)); n != spec.StdinBytes {
		// never run a job with a missing or partial payload
		clear(input)
		return j.finish(jobs.Failed, jobs.ExitNotFound, fmt.Sprintf("%s (got %d of %d bytes)", jobs.StdinLostReason, n, spec.StdinBytes))
	}

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

	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Per-user isolation: the workload runs as the user's account, with that
	// account's environment (nothing of LocalSystem's / root's) plus job env
	// plus secrets, and - on Unix - a private umask. The (privileged) shim
	// keeps its own environment: the account's (HKCU\Environment, ...) is
	// the user's to write and only ever reaches the user's processes.
	var acct *platform.Account
	if ra := spec.RunAs; ra != nil {
		if acct, err = platform.LookupAccount(ra.Account); err != nil {
			return j.failStart("account "+ra.Account, err)
		}
		base, err := platform.AccountEnv(acct)
		if err != nil {
			return j.failStart("environment of "+ra.Account, err)
		}
		env := append([]string(nil), base...)
		if ra.Tmp != "" && runtime.GOOS != "windows" {
			env = append(env, "TMPDIR="+ra.Tmp)
		}
		for _, k := range keys {
			env = append(env, k+"="+spec.Env[k])
		}
		for k, v := range secrets {
			env = append(env, k+"="+v)
		}
		j.env, j.home = dedupEnv(env), acct.Home
		privateUmask()
	} else {
		// The child inherits the shim's environment (= longarmd's, i.e. the
		// service account's) plus job env plus secrets. Setting them on the
		// shim itself also makes exec's PATH lookup honour a job-provided PATH.
		for _, k := range keys {
			os.Setenv(k, spec.Env[k])
		}
		for k, v := range secrets {
			os.Setenv(k, v)
		}
		j.env = os.Environ()
	}

	copts := platform.ContainerOptions{
		Name: "longarm-" + spec.ID, MemLimitBytes: uint64(spec.MemLimitBytes), KillOnOwnerDeath: true,
		RunAs: acct, CPULimit: spec.CPULimit,
	}
	if spec.RunAs != nil {
		copts.Cgroup = spec.RunAs.Cgroup
	}
	ctr, err := platform.NewContainer(copts)
	if err != nil {
		return j.failStart("cannot create job container", err)
	}
	defer ctr.Close()
	if spec.Pty {
		return j.runSession(ctr)
	}
	return j.runPipe(ctr)
}

// readInput reads what longarmd sends on the shim's stdin: the secrets (a
// JSON object, possibly absent) followed by the job's stdin payload.
func readInput(r io.Reader) (map[string]string, []byte, error) {
	secrets := map[string]string{}
	if r == nil {
		return secrets, nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, 1<<20+jobs.MaxStdinBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read secrets: %w", err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return secrets, nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	defer clear(b)
	if err := dec.Decode(&secrets); err != nil {
		return nil, nil, errors.New("decode secrets: malformed input") // (the error would quote it)
	}
	rest := b[dec.InputOffset():]
	if len(rest) > jobs.MaxStdinBytes {
		return nil, nil, fmt.Errorf("stdin payload larger than %d bytes", jobs.MaxStdinBytes)
	}
	var in []byte
	if len(rest) > 0 {
		in = bytes.Clone(rest) // b is zeroed; in is, once written to the job
	}
	return secrets, in, nil
}

// feedStdin gives the job its stdin payload through a pipe the shim
// writes from memory; with no payload the job's stdin is empty. The
// returned stop closes the shim's end (a job that never reads its stdin
// can't block the shim).
func (j *job) feedStdin(cmd *exec.Cmd) (start, stop func(), err error) {
	if len(j.stdin) == 0 {
		return func() {}, func() {}, nil
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdin = r
	var once sync.Once
	stop = func() { once.Do(func() { w.Close() }) }
	start = func() {
		r.Close() // the child has its copy
		data := j.stdin
		j.stdin = nil
		go func() {
			_, _ = w.Write(data) // fails once the job closed its end
			clear(data)
			stop()
		}()
	}
	return start, stop, nil
}

// runPipe runs a batch job: output through one pipe into out.log.
func (j *job) runPipe(ctr platform.Container) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	argv := j.command(j.spec.Argv)
	if ra := j.spec.RunAs; ra != nil && len(ra.Wrapper) > 0 {
		argv = append(append([]string(nil), ra.Wrapper...), argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = j.workdir()
	cmd.Env = j.env
	cmd.Stdout, cmd.Stderr = pw, pw // one pipe: interleaving preserved
	startIn, stopIn, err := j.feedStdin(cmd)
	if err != nil {
		pw.Close()
		pr.Close()
		return j.failStart("cannot create the stdin pipe", err)
	}
	defer stopIn()
	startErr := ctr.Start(cmd)
	pw.Close()
	if startErr != nil {
		pr.Close()
		if r, ok := cmd.Stdin.(*os.File); ok {
			r.Close()
		}
		return j.failStart(fmt.Sprintf("failed to start %q", j.spec.Argv[0]), startErr)
	}
	startIn()

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
	stopIn()
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
