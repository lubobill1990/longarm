// Package supervisor is longarmd's job manager: it creates job directories,
// spawns one detached shim per job, watches status.json + shim liveness,
// reconciles every job directory on startup (DESIGN §5.1 / §7.4), and lets
// API handlers wait for state changes.
package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/logfile"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/redact"
)

// Errors returned by Manager methods.
var (
	ErrNotFound      = errors.New("job not found")
	ErrAlreadyExists = errors.New("job id already used with a different spec")
	ErrInvalid       = errors.New("invalid job spec")
	ErrActive        = errors.New("job is still active")
)

// PollInterval is how often non-terminal jobs are re-read.
const PollInterval = 200 * time.Millisecond

// Options configure a Manager.
type Options struct {
	JobsDir string
	// ShimCommand returns the command that runs `longarmd shim <dir>`; defaults
	// to the current executable.
	ShimCommand func(dir string) *exec.Cmd
	Logger      *slog.Logger
	// KillGrace is how long Kill waits for the shim to act on the marker
	// before force-terminating it.
	KillGrace time.Duration
	// OrphanGrace: a non-terminal job with no shim recorded is declared lost
	// after this long (covers longarmd dying between mkdir and shim start).
	OrphanGrace time.Duration
	// Prepare readies the working directory of a job with a workspace spec
	// (git worktree, DESIGN §5.3) while the job is "preparing"; it returns
	// the directory. log is the job's out.log (secrets redacted). Runs on a
	// goroutine; ctx is cancelled when the job is killed meanwhile. auth is
	// the fetch credential forwarded with the submit (nil: none); the
	// manager zeroes it when Prepare returns.
	Prepare func(ctx context.Context, dir jobs.Dir, spec jobs.Spec, secrets map[string]string, auth *jobs.GitAuth, log io.Writer) (string, error)
	// OnChange is called (with the manager's lock held: it must not block
	// or call back) whenever a job changed.
	OnChange func()
	// DirMode is the mode of new job directories (default 0755).
	DirMode os.FileMode
}

// Job is a snapshot of one job.
type Job struct {
	Spec    jobs.Spec
	Status  jobs.Status
	LogSize int64
}

type entry struct {
	dir      jobs.Dir
	spec     jobs.Spec
	st       jobs.Status
	loadedAt time.Time
	// shimDone is non-nil if this longarmd spawned the shim; closed when it exits
	// (we must reap our own children, or a zombie looks alive on Unix).
	shimDone chan struct{}
	rec      sync.Mutex // serializes reconcile of this entry
	// cancelPrep aborts a running Prepare (guarded by Manager.mu).
	cancelPrep context.CancelFunc
}

// Manager supervises all jobs of one longarmd.
type Manager struct {
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	jobs    map[string]*entry
	changed chan struct{} // closed (and replaced) on any change
	submit  sync.Mutex    // serializes Submit (idempotency check + create)
}

// New loads every job directory and reconciles it (re-adopt / lost).
func New(opts Options) (*Manager, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.KillGrace == 0 {
		opts.KillGrace = 5 * time.Second
	}
	if opts.OrphanGrace == 0 {
		opts.OrphanGrace = 5 * time.Second
	}
	if opts.ShimCommand == nil {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		opts.ShimCommand = func(dir string) *exec.Cmd { return exec.Command(exe, "shim", dir) }
	}
	if err := os.MkdirAll(opts.JobsDir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{opts: opts, log: opts.Logger, jobs: map[string]*entry{}, changed: make(chan struct{})}
	ents, err := os.ReadDir(opts.JobsDir)
	if err != nil {
		return nil, err
	}
	adopted, terminal := 0, 0
	for _, de := range ents {
		if !de.IsDir() || !jobs.ValidID(de.Name()) {
			continue
		}
		d := jobs.Dir(filepath.Join(opts.JobsDir, de.Name()))
		spec, err := d.ReadSpec()
		if err != nil {
			m.log.Warn("skipping job dir", "dir", d, "err", err)
			continue
		}
		st, err := d.ReadStatus()
		if errors.Is(err, os.ErrNotExist) {
			// longarmd died between writing job.json and status.json: the job
			// never started. Record that instead of leaving the id blocked.
			st = &jobs.Status{State: jobs.Lost, CreatedAt: spec.CreatedAt, EndedAt: time.Now().UTC(),
				Reason: "longarmd stopped before the job was started"}
			if spec.StdinBytes > 0 {
				code := jobs.ExitNotFound
				st.State, st.ExitCode, st.Reason = jobs.Failed, &code, jobs.StdinLostReason
			}
			if err := d.WriteStatus(st); err != nil {
				m.log.Warn("skipping job dir", "dir", d, "err", err)
				continue
			}
		} else if err != nil {
			m.log.Warn("skipping job dir with unreadable status", "dir", d, "err", err)
			continue
		}
		m.jobs[spec.ID] = &entry{dir: d, spec: *spec, st: *st, loadedAt: time.Now()}
		if st.State.Terminal() {
			terminal++
		} else {
			adopted++
		}
	}
	m.log.Info("jobs loaded", "active", adopted, "terminal", terminal)
	m.pollOnce() // reconcile immediately
	return m, nil
}

// Run polls active jobs until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.pollOnce()
		}
	}
}

// Changed returns a channel that is closed at the next change of any job.
func (m *Manager) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

func (m *Manager) broadcastLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
	if m.opts.OnChange != nil {
		m.opts.OnChange()
	}
}

func (m *Manager) pollOnce() {
	m.mu.Lock()
	active := make([]*entry, 0)
	for _, e := range m.jobs {
		if !e.st.State.Terminal() {
			active = append(active, e)
		}
	}
	m.mu.Unlock()
	for _, e := range active {
		m.reconcile(e)
	}
}

func (m *Manager) setStatus(e *entry, st *jobs.Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st.Seq == e.st.Seq && st.State == e.st.State {
		return
	}
	e.st = *st
	m.broadcastLocked()
}

// shimAlive decides whether the job's supervisor is still there.
func (m *Manager) shimAlive(e *entry, st *jobs.Status) bool {
	m.mu.Lock()
	done := e.shimDone
	m.mu.Unlock()
	if done != nil {
		select {
		case <-done:
			return false
		default:
			return true
		}
	}
	return st.ShimPID > 0 && platform.ProcessAlive(st.ShimPID, st.ShimStart)
}

// reconcile re-reads one active job and handles a vanished shim.
func (m *Manager) reconcile(e *entry) {
	e.rec.Lock()
	defer e.rec.Unlock()
	st, err := e.dir.ReadStatus()
	if err != nil {
		m.log.Warn("read status", "job", e.spec.ID, "err", err)
		return
	}
	if st.State.Terminal() {
		m.setStatus(e, st)
		return
	}
	if st.Orphaned {
		// Read-only adoption: the shim is gone, the process is not.
		if e.dir.KillRequested() {
			if platform.ProcessAlive(st.PID, st.PIDStart) {
				_ = platform.KillProcess(st.PID)
			}
			platform.KillProcessGroup(st.PID)
			code := platform.KilledExitCode
			m.longarmdWrite(e, st, jobs.Killed, &code, "killed (orphaned process)")
		} else if !platform.ProcessAlive(st.PID, st.PIDStart) {
			platform.KillProcessGroup(st.PID)
			m.longarmdWrite(e, st, jobs.Lost, nil, "shim died; process has exited, exit code unknown")
		} else {
			m.setStatus(e, st)
		}
		return
	}
	if m.shimAlive(e, st) {
		m.setStatus(e, st)
		return
	}
	m.mu.Lock()
	ours := e.shimDone != nil
	loadedAt := e.loadedAt
	m.mu.Unlock()
	if st.ShimPID == 0 && !ours && time.Since(loadedAt) < m.opts.OrphanGrace {
		return // a shim may be starting right now; give it a moment
	}
	// Shim is gone. It may have finished between the two reads.
	if st2, err := e.dir.ReadStatus(); err == nil {
		st = st2
	}
	if st.State.Terminal() {
		m.setStatus(e, st)
		return
	}
	switch {
	case e.dir.KillRequested():
		code := platform.KilledExitCode
		if st.PID > 0 && platform.ProcessAlive(st.PID, st.PIDStart) {
			_ = platform.KillProcess(st.PID)
		}
		platform.KillProcessGroup(st.PID)
		m.longarmdWrite(e, st, jobs.Killed, &code, "killed (shim force-terminated)")
	case st.State == jobs.Running && st.PID > 0 && platform.ProcessAlive(st.PID, st.PIDStart):
		st.Orphaned = true
		st.Reason = "shim died; process still running (read-only adoption)"
		m.log.Warn("job orphaned", "job", e.spec.ID, "pid", st.PID)
		if err := e.dir.WriteStatus(st); err != nil {
			m.log.Error("write status", "job", e.spec.ID, "err", err)
		}
		m.setStatus(e, st)
	default:
		// Background processes of the job may outlive its leader (Unix: only
		// the direct child gets the parent-death signal).
		platform.KillProcessGroup(st.PID)
		if st.State == jobs.Preparing && e.spec.StdinBytes > 0 {
			// it never started, and its payload is gone with longarmd's or
			// the shim's memory: it can't run as submitted
			code := jobs.ExitNotFound
			m.longarmdWrite(e, st, jobs.Failed, &code, jobs.StdinLostReason)
			return
		}
		m.longarmdWrite(e, st, jobs.Lost, nil, "supervisor (shim) exited unexpectedly")
	}
}

// longarmdWrite records a terminal state decided by longarmd (the shim is gone).
func (m *Manager) longarmdWrite(e *entry, st *jobs.Status, state jobs.State, code *int, reason string) {
	if err := jobs.CheckTransition(st.State, state); err != nil {
		m.log.Error("state", "job", e.spec.ID, "err", err)
		return
	}
	st.State, st.ExitCode, st.Reason, st.EndedAt = state, code, reason, time.Now().UTC()
	m.log.Warn("job "+string(state), "job", e.spec.ID, "reason", reason)
	if err := e.dir.WriteStatus(st); err != nil {
		m.log.Error("write status", "job", e.spec.ID, "err", err)
	}
	m.setStatus(e, st)
}

func (m *Manager) snapshot(e *entry) Job {
	m.mu.Lock()
	j := Job{Spec: e.spec, Status: e.st}
	m.mu.Unlock()
	j.LogSize = e.dir.LogSize()
	return j
}

// Get returns one job.
func (m *Manager) Get(id string) (Job, error) {
	m.mu.Lock()
	e, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return Job{}, ErrNotFound
	}
	return m.snapshot(e), nil
}

// Dir returns the job directory of id.
func (m *Manager) Dir(id string) (jobs.Dir, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.jobs[id]
	if !ok {
		return "", ErrNotFound
	}
	return e.dir, nil
}

// List returns jobs newest first, optionally filtered by state.
func (m *Manager) List(states []jobs.State, limit int) []Job {
	m.mu.Lock()
	es := make([]*entry, 0, len(m.jobs))
	for _, e := range m.jobs {
		if len(states) == 0 || slices.Contains(states, e.st.State) {
			es = append(es, e)
		}
	}
	m.mu.Unlock()
	sort.Slice(es, func(i, j int) bool {
		if !es[i].spec.CreatedAt.Equal(es[j].spec.CreatedAt) {
			return es[i].spec.CreatedAt.After(es[j].spec.CreatedAt)
		}
		return es[i].spec.ID > es[j].spec.ID
	})
	if limit > 0 && len(es) > limit {
		es = es[:limit]
	}
	out := make([]Job, len(es))
	for i, e := range es {
		out[i] = m.snapshot(e)
	}
	return out
}

// RunningCount returns the number of non-terminal jobs.
func (m *Manager) RunningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.jobs {
		if !e.st.State.Terminal() {
			n++
		}
	}
	return n
}

func sameSpec(a, b *jobs.Spec) bool {
	return a.Name == b.Name && slices.Equal(a.Argv, b.Argv) && a.Cwd == b.Cwd &&
		maps.Equal(a.Env, b.Env) && slices.Equal(a.SecretEnvNames, b.SecretEnvNames) &&
		a.MemLimitBytes == b.MemLimitBytes && maps.Equal(a.Labels, b.Labels) &&
		a.Pty == b.Pty && sameWorkspace(a.Workspace, b.Workspace) && a.Owner == b.Owner &&
		slices.Equal(a.ShareTeams, b.ShareTeams) && a.CPULimit == b.CPULimit && a.StdinBytes == b.StdinBytes
}

// sameWorkspace compares what a workspace resolves to - repository, url,
// ref, commit, the staged overlay's digest, LFS mode - not the per-attempt
// parts of a submit: every
// `longarm run --repo <local checkout>` uploads (and stages) the checkout
// anew, under a new staging id, and whether a fetch credential came along
// is no property of the job. A retried submit of the same job (e.g. after
// "stdin payload lost before start; resubmit") must match. A staged upload
// a retry does not use is left to GC (staging/ entries expire).
func sameWorkspace(a, b *jobs.Workspace) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Repo == b.Repo && a.URL == b.URL && a.Ref == b.Ref && a.Commit == b.Commit && a.Overlay == b.Overlay &&
		a.LFSFetch == b.LFSFetch
}

// Submit creates and starts a job. It is idempotent on spec.ID.
func (m *Manager) Submit(ctx context.Context, spec jobs.Spec, secrets map[string]string) (Job, error) {
	return m.SubmitStdin(ctx, spec, secrets, nil)
}

// SubmitStdin is Submit with bytes for the job's stdin. Like the secrets
// they only live in memory - here until the shim has them, then in the
// shim until the job read them; spec records only their size. A retried
// submit (same id and spec) does not deliver them again.
func (m *Manager) SubmitStdin(ctx context.Context, spec jobs.Spec, secrets map[string]string, stdin []byte) (Job, error) {
	return m.SubmitWith(ctx, spec, secrets, Input{Stdin: stdin})
}

// Input is what a submit hands to the job besides its spec and secrets,
// memory-only.
type Input struct {
	// Stdin: bytes for the job's stdin (SubmitStdin).
	Stdin []byte
	// GitAuth: a credential for the workspace fetch only (Options.Prepare);
	// the job never sees it. Zeroed once the workspace is prepared, or
	// when the submit does not use it (a retry of an existing job).
	GitAuth *jobs.GitAuth
}

// SubmitWith is Submit with in.
func (m *Manager) SubmitWith(ctx context.Context, spec jobs.Spec, secrets map[string]string, in Input) (Job, error) {
	j, _, err := m.SubmitCreated(ctx, spec, secrets, in)
	return j, err
}

// SubmitCreated is SubmitWith that also reports whether this call created
// the job (false: an identical one existed), decided under the submit lock.
func (m *Manager) SubmitCreated(ctx context.Context, spec jobs.Spec, secrets map[string]string, in Input) (Job, bool, error) {
	stdin, auth := in.Stdin, in.GitAuth
	handedOff := false
	defer func() {
		if !handedOff {
			auth.Clear()
		}
	}()
	if auth != nil && (spec.Workspace == nil || spec.Workspace.URL == "") {
		return Job{}, false, fmt.Errorf("%w: a fetch credential needs a workspace url", ErrInvalid)
	}
	if spec.Workspace != nil {
		spec.Workspace.GitAuth = auth.Valid()
	}
	if len(stdin) > jobs.MaxStdinBytes {
		return Job{}, false, fmt.Errorf("%w: stdin of %d bytes exceeds %d", ErrInvalid, len(stdin), jobs.MaxStdinBytes)
	}
	if len(stdin) > 0 && spec.Pty {
		return Job{}, false, fmt.Errorf("%w: stdin is for pipe jobs, not sessions", ErrInvalid)
	}
	spec.StdinBytes = int64(len(stdin))
	if !jobs.ValidID(spec.ID) {
		return Job{}, false, fmt.Errorf("%w: bad id %q", ErrInvalid, spec.ID)
	}
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return Job{}, false, fmt.Errorf("%w: empty argv", ErrInvalid)
	}
	spec.SecretEnvNames = slices.Sorted(maps.Keys(secrets))
	if len(spec.Env) == 0 {
		spec.Env = nil
	}
	if len(spec.Labels) == 0 {
		spec.Labels = nil
	}
	if len(spec.SecretEnvNames) == 0 {
		spec.SecretEnvNames = nil
	}

	m.submit.Lock()
	defer m.submit.Unlock()
	m.mu.Lock()
	existing, ok := m.jobs[spec.ID]
	m.mu.Unlock()
	if ok {
		if !sameSpec(&existing.spec, &spec) {
			return Job{}, false, ErrAlreadyExists
		}
		j := m.snapshot(existing)
		if len(stdin) == 0 || !j.Status.State.Terminal() || !j.Status.StartedAt.IsZero() {
			return j, false, nil // the existing job; a payload sent again is dropped
		}
		// It took a payload and ended without starting (the payload was
		// lost before the hand-off, or the program did not start): the
		// retry replaces it.
		m.log.Info("job resubmitted with its stdin payload; replacing the one that never started", "job", spec.ID)
		if err := m.removeLocked(spec.ID, time.Time{}); err != nil {
			return Job{}, false, err
		}
	}

	dir := jobs.Dir(filepath.Join(m.opts.JobsDir, spec.ID))
	mode := m.opts.DirMode
	if mode == 0 {
		mode = 0o755
	}
	if err := os.Mkdir(string(dir), mode); err != nil {
		return Job{}, false, err
	}
	spec.CreatedAt = time.Now().UTC()
	st := jobs.Status{State: jobs.Preparing, CreatedAt: spec.CreatedAt}
	if err := dir.WriteSpec(&spec); err != nil {
		return Job{}, false, err
	}
	if err := dir.WriteStatus(&st); err != nil {
		return Job{}, false, err
	}
	e := &entry{dir: dir, spec: spec, st: st, loadedAt: time.Now(), shimDone: make(chan struct{})}
	m.mu.Lock()
	m.jobs[spec.ID] = e
	m.broadcastLocked()
	m.mu.Unlock()

	if spec.Workspace != nil {
		// Preparing (fetch, worktree) can take long: do it in the background;
		// the job stays "preparing" (shimDone open = supervised by us).
		pctx, cancel := context.WithCancel(context.Background())
		m.mu.Lock()
		e.cancelPrep = cancel
		m.mu.Unlock()
		m.log.Info("job submitted; preparing workspace", "job", spec.ID, "repo", spec.Workspace.Repo, "by", spec.SubmittedBy)
		handedOff = true
		go func() {
			defer cancel()
			m.prepareAndStart(pctx, e, secrets, stdin, auth)
		}()
		return m.snapshot(e), true, nil
	}
	if err := m.spawnShim(e, secrets, stdin); err != nil {
		m.failBeforeShim(e, jobs.ExitNotFound, stdinLost(&spec, "spawn shim: "+err.Error()))
		return m.snapshot(e), true, nil
	}
	m.log.InfoContext(ctx, "job submitted", "job", spec.ID, "argv", spec.Argv, "by", spec.SubmittedBy) // ctx: trace=<operation>

	// Give the shim a moment to start the process so the caller gets a pid.
	deadline := time.After(5 * time.Second)
	for {
		ch := m.Changed()
		m.reconcile(e)
		j := m.snapshot(e)
		if j.Status.State != jobs.Preparing {
			return j, true, nil
		}
		select {
		case <-ch:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			return j, true, nil
		case <-ctx.Done():
			return j, true, nil
		}
	}
}

// spawnShim starts the job's shim. Its stdin carries the secrets (one JSON
// object), then the bytes for the job's stdin.
func (m *Manager) spawnShim(e *entry, secrets map[string]string, stdin []byte) error {
	payload, err := json.Marshal(secrets)
	if err != nil {
		return err
	}
	payload = append(payload, stdin...)
	clear(stdin) // the copy in payload is the only one now
	shimLog, err := os.OpenFile(e.dir.Path(jobs.ShimLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd, err := platform.StartDetached(func() *exec.Cmd {
		c := m.opts.ShimCommand(string(e.dir))
		c.Stdin = &zeroingReader{b: payload} // memory only, never on disk
		c.Stdout, c.Stderr = shimLog, shimLog
		return c
	})
	shimLog.Close()
	if err != nil {
		return err
	}
	done := e.shimDone
	go func() {
		_ = cmd.Wait()
		clear(payload) // written already unless the shim failed early
		close(done)
		m.reconcile(e)
	}()
	return nil
}

// zeroingReader is the shim's stdin: it writes b straight into the pipe
// (WriterTo: no copy in an intermediate buffer) and zeroes b once it was
// written completely.
type zeroingReader struct {
	b   []byte
	off int
}

func (z *zeroingReader) Read(p []byte) (int, error) {
	if z.off >= len(z.b) {
		return 0, io.EOF
	}
	n := copy(p, z.b[z.off:])
	z.off += n
	return n, nil
}

func (z *zeroingReader) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(z.b[z.off:])
	z.off += n
	if err == nil {
		clear(z.b)
	}
	return int64(n), err
}

// Kill asks the shim to kill the job's tree and waits for a terminal state;
// if the shim does not react within KillGrace it is force-terminated (on
// Windows that closes the Job Object, killing the tree; on Unix the process
// group is SIGKILLed too).
func (m *Manager) Kill(ctx context.Context, id string) (Job, error) {
	return m.KillGen(ctx, id, time.Time{})
}

// ErrReplaced: KillGen / RemoveGen found another job (a newer generation)
// under the id.
var ErrReplaced = errors.New("replaced by a newer job of the same id")

// KillGen is Kill bound to one generation of id (its spec's CreatedAt;
// zero = whichever there is): a job that replaced it is never touched.
func (m *Manager) KillGen(ctx context.Context, id string, created time.Time) (Job, error) {
	m.mu.Lock()
	e, ok := m.jobs[id]
	if ok && !created.IsZero() && !e.spec.CreatedAt.Equal(created) {
		m.mu.Unlock()
		return Job{}, ErrReplaced
	}
	m.mu.Unlock()
	if !ok {
		return Job{}, ErrNotFound
	}
	if j := m.snapshot(e); j.Status.State.Terminal() {
		return j, nil
	}
	if err := e.dir.RequestKill("longarm kill"); err != nil {
		return Job{}, err
	}
	m.mu.Lock()
	if e.cancelPrep != nil {
		e.cancelPrep() // still preparing: abort; prepareAndStart records "killed"
	}
	m.mu.Unlock()
	m.log.Info("kill requested", "job", id)
	// The grace period and escalation must not be cut short by the caller
	// going away (that would force-kill the shim before it flushed the log);
	// both are bounded anyway.
	ctx = context.WithoutCancel(ctx)
	if j, ok := m.waitTerminal(ctx, e, m.opts.KillGrace); ok {
		return j, nil
	}
	j := m.snapshot(e)
	m.log.Warn("shim did not react to kill; terminating it", "job", id, "shim", j.Status.ShimPID)
	if j.Status.ShimPID > 0 && platform.ProcessAlive(j.Status.ShimPID, j.Status.ShimStart) {
		_ = platform.KillProcess(j.Status.ShimPID)
	}
	if j.Status.PID > 0 && platform.ProcessAlive(j.Status.PID, j.Status.PIDStart) {
		_ = platform.KillProcess(j.Status.PID)
	}
	if j, ok := m.waitTerminal(ctx, e, 5*time.Second); ok {
		return j, nil
	}
	return m.snapshot(e), fmt.Errorf("job %s did not terminate", id)
}

func (m *Manager) waitTerminal(ctx context.Context, e *entry, d time.Duration) (Job, bool) {
	deadline := time.After(d)
	for {
		ch := m.Changed()
		m.reconcile(e)
		if j := m.snapshot(e); j.Status.State.Terminal() {
			return j, true
		}
		select {
		case <-ch:
		case <-time.After(PollInterval):
		case <-deadline:
			return Job{}, false
		case <-ctx.Done():
			return Job{}, false
		}
	}
}

// failBeforeShim records a failure decided by longarmd before any shim ran.
func (m *Manager) failBeforeShim(e *entry, code int, reason string) {
	// Hold the reconcile lock so a concurrent poll cannot record "lost"
	// between closing shimDone and writing the final state.
	e.rec.Lock()
	defer e.rec.Unlock()
	m.mu.Lock()
	st := e.st
	e.cancelPrep = nil
	m.mu.Unlock()
	close(e.shimDone)
	state := jobs.Failed
	if e.dir.KillRequested() {
		state, code, reason = jobs.Killed, platform.KilledExitCode, "killed while preparing"
	}
	m.longarmdWrite(e, &st, state, &code, reason)
}

// stdinLost prefixes reason with jobs.StdinLostReason for a job whose
// stdin payload went with a failed start.
func stdinLost(spec *jobs.Spec, reason string) string {
	if spec.StdinBytes > 0 {
		return jobs.StdinLostReason + " (" + reason + ")"
	}
	return reason
}

// ExitPrepareFailed is the exit code of a job whose workspace could not be
// prepared.
const ExitPrepareFailed = 126

func (m *Manager) prepareAndStart(ctx context.Context, e *entry, secrets map[string]string, stdin []byte, auth *jobs.GitAuth) {
	workdir, err := m.prepare(ctx, e, secrets, auth)
	auth.Clear() // used for the fetch only
	if err == nil && e.dir.KillRequested() {
		err = context.Canceled
	}
	if err != nil {
		m.failBeforeShim(e, ExitPrepareFailed, "prepare workspace: "+err.Error())
		return
	}
	m.mu.Lock()
	e.cancelPrep = nil
	st := e.st
	m.mu.Unlock()
	st.Workdir = workdir
	if err := e.dir.WriteStatus(&st); err != nil {
		m.failBeforeShim(e, ExitPrepareFailed, "write status: "+err.Error())
		return
	}
	m.setStatus(e, &st)
	if err := m.spawnShim(e, secrets, stdin); err != nil {
		m.failBeforeShim(e, jobs.ExitNotFound, stdinLost(&e.spec, "spawn shim: "+err.Error()))
		return
	}
	m.log.Info("workspace ready; job started", "job", e.spec.ID, "workdir", workdir)
}

// prepare runs the Prepare hook with out.log (redacted) as its log.
func (m *Manager) prepare(ctx context.Context, e *entry, secrets map[string]string, auth *jobs.GitAuth) (string, error) {
	if m.opts.Prepare == nil {
		return "", errors.New("workspaces are not supported by this longarmd")
	}
	lw, err := logfile.OpenWriter(e.dir.Path(jobs.LogFile), e.spec.LogMaxBytes)
	if err != nil {
		return "", err
	}
	defer lw.Close()
	values := make([]string, 0, len(secrets))
	for _, v := range secrets {
		values = append(values, v)
	}
	if auth.Valid() {
		values = append(values, string(auth.Secret))
	}
	out := redact.NewWriter(lw, redact.New(values))
	defer out.Flush()
	return m.opts.Prepare(ctx, e.dir, e.spec, secrets, auth, out)
}

// Remove deletes a terminal job (directory and all) - used by GC and to
// re-create a named session that has ended.
func (m *Manager) Remove(id string) error {
	return m.RemoveGen(id, time.Time{})
}

// RemoveGen is Remove bound to one generation of id (its spec's CreatedAt;
// zero = whichever there is): ErrReplaced if another job has the id now.
func (m *Manager) RemoveGen(id string, created time.Time) error {
	m.submit.Lock()
	defer m.submit.Unlock()
	return m.removeLocked(id, created)
}

// removeLocked is RemoveGen with m.submit held.
func (m *Manager) removeLocked(id string, created time.Time) error {
	m.mu.Lock()
	e, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	if !created.IsZero() && !e.spec.CreatedAt.Equal(created) {
		m.mu.Unlock()
		return ErrReplaced
	}
	if !e.st.State.Terminal() {
		m.mu.Unlock()
		return ErrActive
	}
	delete(m.jobs, id)
	m.broadcastLocked()
	m.mu.Unlock()
	e.rec.Lock()
	defer e.rec.Unlock()
	var err error
	for i := 0; i < 20; i++ { // Windows: a reader may hold a file for a moment
		if err = os.RemoveAll(string(e.dir)); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}
