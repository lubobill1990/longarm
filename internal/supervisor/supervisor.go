// Package supervisor is longarmd's job manager: it creates job directories,
// spawns one detached shim per job, watches status.json + shim liveness,
// reconciles every job directory on startup (DESIGN §5.1 / §7.4), and lets
// API handlers wait for state changes.
package supervisor

import (
	"bytes"
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
	"reflect"
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
	// goroutine; ctx is cancelled when the job is killed meanwhile.
	Prepare func(ctx context.Context, dir jobs.Dir, spec jobs.Spec, secrets map[string]string, log io.Writer) (string, error)
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
		a.Pty == b.Pty && reflect.DeepEqual(a.Workspace, b.Workspace)
}

// Submit creates and starts a job. It is idempotent on spec.ID.
func (m *Manager) Submit(ctx context.Context, spec jobs.Spec, secrets map[string]string) (Job, error) {
	if !jobs.ValidID(spec.ID) {
		return Job{}, fmt.Errorf("%w: bad id %q", ErrInvalid, spec.ID)
	}
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return Job{}, fmt.Errorf("%w: empty argv", ErrInvalid)
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
			return Job{}, ErrAlreadyExists
		}
		return m.snapshot(existing), nil
	}

	dir := jobs.Dir(filepath.Join(m.opts.JobsDir, spec.ID))
	if err := os.Mkdir(string(dir), 0o755); err != nil {
		return Job{}, err
	}
	spec.CreatedAt = time.Now().UTC()
	st := jobs.Status{State: jobs.Preparing, CreatedAt: spec.CreatedAt}
	if err := dir.WriteSpec(&spec); err != nil {
		return Job{}, err
	}
	if err := dir.WriteStatus(&st); err != nil {
		return Job{}, err
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
		go func() {
			defer cancel()
			m.prepareAndStart(pctx, e, secrets)
		}()
		return m.snapshot(e), nil
	}
	if err := m.spawnShim(e, secrets); err != nil {
		m.failBeforeShim(e, jobs.ExitNotFound, "spawn shim: "+err.Error())
		return m.snapshot(e), nil
	}
	m.log.Info("job submitted", "job", spec.ID, "argv", spec.Argv, "by", spec.SubmittedBy)

	// Give the shim a moment to start the process so the caller gets a pid.
	deadline := time.After(5 * time.Second)
	for {
		ch := m.Changed()
		m.reconcile(e)
		j := m.snapshot(e)
		if j.Status.State != jobs.Preparing {
			return j, nil
		}
		select {
		case <-ch:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			return j, nil
		case <-ctx.Done():
			return j, nil
		}
	}
}

func (m *Manager) spawnShim(e *entry, secrets map[string]string) error {
	payload, err := json.Marshal(secrets)
	if err != nil {
		return err
	}
	shimLog, err := os.OpenFile(e.dir.Path(jobs.ShimLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd, err := platform.StartDetached(func() *exec.Cmd {
		c := m.opts.ShimCommand(string(e.dir))
		c.Stdin = bytes.NewReader(payload) // memory only, never on disk
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
		close(done)
		m.reconcile(e)
	}()
	return nil
}

// Kill asks the shim to kill the job's tree and waits for a terminal state;
// if the shim does not react within KillGrace it is force-terminated (on
// Windows that closes the Job Object, killing the tree; on Unix the process
// group is SIGKILLed too).
func (m *Manager) Kill(ctx context.Context, id string) (Job, error) {
	m.mu.Lock()
	e, ok := m.jobs[id]
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

// ExitPrepareFailed is the exit code of a job whose workspace could not be
// prepared.
const ExitPrepareFailed = 126

func (m *Manager) prepareAndStart(ctx context.Context, e *entry, secrets map[string]string) {
	workdir, err := m.prepare(ctx, e, secrets)
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
	if err := m.spawnShim(e, secrets); err != nil {
		m.failBeforeShim(e, jobs.ExitNotFound, "spawn shim: "+err.Error())
		return
	}
	m.log.Info("workspace ready; job started", "job", e.spec.ID, "workdir", workdir)
}

// prepare runs the Prepare hook with out.log (redacted) as its log.
func (m *Manager) prepare(ctx context.Context, e *entry, secrets map[string]string) (string, error) {
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
	out := redact.NewWriter(lw, redact.New(values))
	defer out.Flush()
	return m.opts.Prepare(ctx, e.dir, e.spec, secrets, out)
}

// Remove deletes a terminal job (directory and all) - used by GC and to
// re-create a named session that has ended.
func (m *Manager) Remove(id string) error {
	m.submit.Lock()
	defer m.submit.Unlock()
	m.mu.Lock()
	e, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
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
