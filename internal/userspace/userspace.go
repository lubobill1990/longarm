// Package userspace is longarmd's data layout for multi-tenancy
// (docs/security.md): one "space" per user with its own job and session
// supervisors, mirrors, worktrees and staging data.
//
// Shared isolation (the default) has a single space - the data dir as it
// always was - and every workload runs as longarmd's account. Per-user
// isolation (longarmd as LocalSystem / root) gives every user
//
//	<users>/<account>/            LocalSystem/root only (Linux: 0710 root:<account group>)
//	  owner                       the user name
//	  jobs/ sessions/             job and session dirs: LocalSystem/root only - the
//	                              shims, which run privileged, trust job.json
//	  mirrors/ worktrees/         owned by the account, private (0700 / owner +
//	  staging/ tmp/               SYSTEM + Administrators)
//
// where <account> is the user's OS account (la-<user>, created on first use).
// The legacy shared space (<data>/jobs, <data>/sessions) stays readable for
// admins. Job ids are unique across all spaces; session names per space.
package userspace

import (
	"context"
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
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/loopguard"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/supervisor"
	"github.com/lubobill1990/longarm/internal/tenancy"
	"github.com/lubobill1990/longarm/internal/workspace"
)

// Layout names.
const (
	JobsDir     = "jobs"
	SessionsDir = "sessions"
	TmpDir      = "tmp"
	OwnerFile   = "owner"
	UsersDir    = "users"
)

// Config configures the spaces of one longarmd.
type Config struct {
	Mode    string // tenancy.Shared (default) | tenancy.PerUser
	DataDir string
	// UsersDir holds the per-user spaces (default <data>/users). Every
	// directory above it must be traversable by the accounts (Unix: o+x).
	UsersDir string
	Prefix   string // OS account prefix (default "la-")
	// CgroupRoot (Linux, per-user): parent of the jobs' cgroups (default
	// /sys/fs/cgroup/longarm; "off" = none).
	CgroupRoot string
	// Loopback (Linux, per-user): loopguard.Isolate (default), Require or
	// Allow - whether the accounts may reach each other over loopback.
	Loopback string
	// Exe is longarmd's binary (user helpers, Windows conhost wrapper).
	Exe string
	Log *slog.Logger
	// Prepare readies the worktree of a workspace job in the shared space.
	Prepare func(ctx context.Context, dir jobs.Dir, spec jobs.Spec, secrets map[string]string, auth *jobs.GitAuth, log io.Writer) (string, error)
	// ShimCommand overrides the shim (tests); default `<longarmd> shim <dir>`.
	ShimCommand func(dir string) *exec.Cmd
}

// Space is the data of one user (or the shared space).
type Space struct {
	// Owner is the user; "" for the shared space (shared mode: everyone's,
	// owners recorded per job; per-user mode: legacy data, admins only).
	Owner    string
	Root     string
	Jobs     *supervisor.Manager
	Sessions *supervisor.Manager
	WS       *workspace.Store
	// Account is the user's OS account (per-user isolation; nil otherwise).
	Account *platform.Account
	Tmp     string // the account's private temp dir

	sp     *Spaces
	cancel context.CancelFunc
	// releaseProfile ends longarmd's hold on the account's profile.
	releaseProfile func()
}

// holdProfile keeps the account's profile loaded while the space exists
// (Windows: HKCU for every process of the user; see platform.LoadProfile).
func (sp *Space) holdProfile() {
	if sp.Account == nil || sp.releaseProfile != nil {
		return
	}
	release, err := platform.LoadProfile(sp.Account)
	if err != nil {
		sp.sp.log.Warn("load user profile", "user", sp.Owner, "err", err)
		return
	}
	sp.releaseProfile = release
}

func (sp *Space) dropProfile() {
	if sp.releaseProfile != nil {
		sp.releaseProfile()
		sp.releaseProfile = nil
	}
}

// Spaces are all spaces of a longarmd.
type Spaces struct {
	cfg    Config
	ctx    context.Context
	log    *slog.Logger
	cgroup bool // per-user jobs get cgroups
	// loopback is the effective loopback mode ("" = not isolated).
	loopback string

	mu      sync.Mutex
	shared  *Space
	users   map[string]*Space
	submit  sync.Mutex // job id uniqueness across spaces
	cmu     sync.Mutex
	changed chan struct{}
	wg      sync.WaitGroup
}

// Open loads the spaces (and reconciles their jobs). Their supervisors
// poll until ctx is done.
func Open(ctx context.Context, cfg Config) (*Spaces, error) {
	if cfg.Mode == "" {
		cfg.Mode = tenancy.Shared
	}
	if cfg.Mode != tenancy.Shared && cfg.Mode != tenancy.PerUser {
		return nil, fmt.Errorf("[isolation] mode %q: want %q or %q", cfg.Mode, tenancy.Shared, tenancy.PerUser)
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Prefix == "" {
		cfg.Prefix = tenancy.DefaultAccountPrefix
	}
	if cfg.UsersDir == "" {
		cfg.UsersDir = filepath.Join(cfg.DataDir, UsersDir)
	}
	if cfg.Exe == "" {
		cfg.Exe, _ = os.Executable()
	}
	s := &Spaces{cfg: cfg, ctx: ctx, log: cfg.Log, users: map[string]*Space{}, changed: make(chan struct{})}
	if cfg.Mode == tenancy.PerUser {
		if err := platform.CanIsolate(); err != nil {
			return nil, err
		}
		if err := s.prepareUsersDir(); err != nil {
			return nil, err
		}
		s.cgroup = s.cgroupUsable()
		s.checkProcHidden()
	}
	shared, err := s.openShared()
	if err != nil {
		return nil, err
	}
	s.shared = shared
	// Nothing runs before the loopback rules cover the accounts: the
	// supervisors start last, and an error leaves no goroutines or loaded
	// profiles behind.
	fail := func(err error) (*Spaces, error) {
		for _, sp := range s.users {
			sp.dropProfile()
		}
		return nil, err
	}
	if cfg.Mode == tenancy.PerUser {
		if err := s.loadUsers(); err != nil {
			return fail(err)
		}
	}
	if err := s.initLoopback(); err != nil {
		return fail(err)
	}
	s.start(s.shared)
	for _, sp := range s.users {
		s.start(sp)
	}
	return s, nil
}

// PerUser reports per-user isolation.
func (s *Spaces) PerUser() bool { return s.cfg.Mode == tenancy.PerUser }

// Mode is the isolation mode.
func (s *Spaces) Mode() string { return s.cfg.Mode }

// Config returns the configuration.
func (s *Spaces) Config() Config { return s.cfg }

func (s *Spaces) notify() {
	s.cmu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.cmu.Unlock()
}

// Changed returns a channel closed at the next change of any job or session
// of any space.
func (s *Spaces) Changed() <-chan struct{} {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	return s.changed
}

func (s *Spaces) newManager(dir, kind string, prepare func(context.Context, jobs.Dir, jobs.Spec, map[string]string, *jobs.GitAuth, io.Writer) (string, error), mode os.FileMode) (*supervisor.Manager, error) {
	o := supervisor.Options{JobsDir: dir, Logger: s.log, Prepare: prepare, OnChange: s.notify, DirMode: mode,
		ShimCommand: s.cfg.ShimCommand}
	if kind == SessionsDir {
		o.Logger = s.log.With("kind", "session")
	}
	return supervisor.New(o)
}

// sessionPrepare is the Prepare hook of sessions started with a workspace:
// the job hook with the session's worktree name (worktrees/s.<name>.<gen>:
// never a job's, nor another generation's of the name).
func sessionPrepare(prepare func(context.Context, jobs.Dir, jobs.Spec, map[string]string, *jobs.GitAuth, io.Writer) (string, error)) func(context.Context, jobs.Dir, jobs.Spec, map[string]string, *jobs.GitAuth, io.Writer) (string, error) {
	if prepare == nil {
		return nil
	}
	return func(ctx context.Context, d jobs.Dir, spec jobs.Spec, secrets map[string]string, auth *jobs.GitAuth, log io.Writer) (string, error) {
		spec.ID = jobs.SessionWorktreeKey(spec.ID, spec.CreatedAt)
		return prepare(ctx, d, spec, secrets, auth, log)
	}
}

func (s *Spaces) openShared() (*Space, error) {
	ws := &workspace.Store{Root: s.cfg.DataDir}
	jm, err := s.newManager(filepath.Join(s.cfg.DataDir, JobsDir), JobsDir, s.cfg.Prepare, 0)
	if err != nil {
		return nil, err
	}
	sm, err := s.newManager(filepath.Join(s.cfg.DataDir, SessionsDir), SessionsDir, sessionPrepare(s.cfg.Prepare), 0)
	if err != nil {
		return nil, err
	}
	return &Space{Root: s.cfg.DataDir, Jobs: jm, Sessions: sm, WS: ws, sp: s}, nil
}

// start runs the space's supervisors until the space is purged or ctx ends.
func (s *Spaces) start(sp *Space) {
	ctx, cancel := context.WithCancel(s.ctx)
	sp.cancel = cancel
	s.wg.Add(2)
	go func() { defer s.wg.Done(); sp.Jobs.Run(ctx) }()
	go func() { defer s.wg.Done(); sp.Sessions.Run(ctx) }()
}

// Wait waits for the supervisors' loops (after ctx is done) and releases
// the users' profiles.
func (s *Spaces) Wait() {
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sp := range s.users {
		sp.dropProfile()
	}
}

// Shared is the shared space.
func (s *Spaces) Shared() *Space { return s.shared }

// All returns every space (shared first, then by owner).
func (s *Spaces) All() []*Space {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*Space{s.shared}
	owners := make([]string, 0, len(s.users))
	for o := range s.users {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	for _, o := range owners {
		out = append(out, s.users[o])
	}
	return out
}

// Lookup returns owner's existing space: shared mode the shared space,
// per-user mode the user's (nil if it has none yet).
func (s *Spaces) Lookup(owner string) *Space {
	if !s.PerUser() {
		return s.shared
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.users[owner]
}

// For returns owner's space, creating it - and with per-user isolation the
// OS account - on first use.
func (s *Spaces) For(owner string) (*Space, error) {
	if !s.PerUser() {
		return s.shared, nil
	}
	if owner == "" {
		return nil, errors.New("no owner")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sp := s.users[owner]; sp != nil {
		if sp.Account == nil { // loaded from disk; the account was gone
			acct, err := platform.EnsureAccount(s.AccountName(owner))
			if err != nil {
				return nil, err
			}
			// A re-created account is a new account (Windows: a new SID):
			// its directories need its rights again.
			if err := secureSpaceRoot(sp.Root, acct); err != nil {
				return nil, err
			}
			if err := secureUserDirs(sp.Root, acct); err != nil {
				return nil, err
			}
			// The rules cover the account before the space uses it: with
			// loopback = "require" a failure leaves the space without it
			// (the user's next call retries).
			if err := s.loopbackLocked(acct); err != nil {
				return nil, err
			}
			sp.Account = acct
			sp.holdProfile()
		}
		return sp, nil
	}
	sp, err := s.createUser(owner)
	if err != nil {
		return nil, err
	}
	if err := s.loopbackLocked(sp.Account); err != nil {
		// loopback = "require": the account exists, but nothing runs as it
		// before the rules cover it (the user's next call retries).
		sp.dropProfile()
		return nil, err
	}
	s.users[owner] = sp
	s.start(sp)
	s.notify()
	return sp, nil
}

// AccountName is owner's OS account.
func (s *Spaces) AccountName(owner string) string { return tenancy.AccountName(s.cfg.Prefix, owner) }

func (s *Spaces) createUser(owner string) (*Space, error) {
	name := s.AccountName(owner)
	acct, err := platform.EnsureAccount(name)
	if err != nil {
		return nil, fmt.Errorf("account for user %s: %w", owner, err)
	}
	root := filepath.Join(s.cfg.UsersDir, name)
	if b, err := os.ReadFile(filepath.Join(root, OwnerFile)); err == nil && strings.TrimSpace(string(b)) != owner {
		return nil, fmt.Errorf("%s belongs to user %q, not %q", root, strings.TrimSpace(string(b)), owner)
	}
	if err := secureSpaceRoot(root, acct); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(root, OwnerFile), []byte(owner+"\n"), 0o600); err != nil {
		return nil, err
	}
	return s.openUser(owner, root, acct)
}

// openUser secures the space's directories and opens its supervisors.
func (s *Spaces) openUser(owner, root string, acct *platform.Account) (*Space, error) {
	for _, d := range []string{JobsDir, SessionsDir} {
		if err := platform.SecurePrivateDir(filepath.Join(root, d)); err != nil {
			return nil, err
		}
	}
	if acct != nil {
		if err := secureUserDirs(root, acct); err != nil {
			return nil, err
		}
	}
	sp := &Space{Owner: owner, Root: root, Account: acct, Tmp: filepath.Join(root, TmpDir), sp: s}
	sp.WS = &workspace.Store{Root: root, Prune: sp.prune, PrivateMirrors: true}
	sp.holdProfile()
	var err error
	if sp.Jobs, err = s.newManager(filepath.Join(root, JobsDir), JobsDir, sp.prepare, 0o700); err != nil {
		return nil, err
	}
	if sp.Sessions, err = s.newManager(filepath.Join(root, SessionsDir), SessionsDir, sessionPrepare(sp.prepare), 0o700); err != nil {
		return nil, err
	}
	return sp, nil
}

// loadUsers opens the spaces found in the users dir.
func (s *Spaces) loadUsers() error {
	ents, err := os.ReadDir(s.cfg.UsersDir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		root := filepath.Join(s.cfg.UsersDir, e.Name())
		b, err := os.ReadFile(filepath.Join(root, OwnerFile))
		if err != nil {
			continue
		}
		owner := strings.TrimSpace(string(b))
		if owner == "" || s.AccountName(owner) != e.Name() {
			s.log.Warn("skipping user space with a mismatching owner file", "dir", root)
			continue
		}
		acct, err := platform.LookupAccount(e.Name())
		if err != nil {
			s.log.Warn("user space without its account (re-created on the user's next call)", "user", owner, "err", err)
			acct = nil
		}
		sp, err := s.openUser(owner, root, acct)
		if err != nil {
			return fmt.Errorf("user space %s: %w", owner, err)
		}
		s.users[owner] = sp // started by Open
	}
	return nil
}

// prepareUsersDir creates the users dir (LocalSystem/root only, traversable
// on Unix) and checks that the accounts can reach their data below it.
func (s *Spaces) prepareUsersDir() error {
	if err := secureDataDir(s.cfg.DataDir); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	dir := s.cfg.UsersDir
	if err := secureUsersDir(dir); err != nil {
		return fmt.Errorf("users dir: %w", err)
	}
	return checkTraversable(dir)
}

// checkProcHidden warns when other users' processes (command lines!) are
// visible in /proc (Linux without hidepid): docs/security.md.
func (s *Spaces) checkProcHidden() {
	if runtime.GOOS != "linux" {
		return
	}
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 4 && f[1] == "/proc" && f[2] == "proc" {
			if strings.Contains(f[3], "hidepid=") && !strings.Contains(f[3], "hidepid=0") && !strings.Contains(f[3], "hidepid=off") {
				return
			}
		}
	}
	s.log.Warn("per-user isolation: /proc shows every process's command line to every account; mount it with hidepid=invisible " +
		"(mount -o remount,hidepid=invisible /proc, and in /etc/fstab) - see docs/security.md")
}

// cgroupUsable reports whether per-user jobs can get cgroups.
func (s *Spaces) cgroupUsable() bool {
	if runtime.GOOS != "linux" || s.cfg.CgroupRoot == "off" {
		return false
	}
	if s.cfg.CgroupRoot == "" {
		s.cfg.CgroupRoot = "/sys/fs/cgroup/longarm"
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		s.log.Warn("cgroup v2 not available: per-user jobs run in process groups only (no memory/CPU limits, no kill of escaped processes)")
		return false
	}
	if err := os.MkdirAll(s.cfg.CgroupRoot, 0o755); err != nil {
		s.log.Warn("cannot create the jobs' cgroup root; per-user jobs run in process groups only", "dir", s.cfg.CgroupRoot, "err", err)
		return false
	}
	return true
}

// Cgroup returns the cgroup directory for a container named name in sp
// ("" when cgroups are off).
func (sp *Space) Cgroup(name string) string {
	s := sp.sp
	if !s.cgroup || sp.Account == nil {
		return ""
	}
	return filepath.ToSlash(filepath.Join(s.cfg.CgroupRoot, sp.Account.Name, name))
}

// RunAs returns how a job of this space runs (nil = as longarmd).
func (sp *Space) RunAs(kind, id string) *jobs.RunAs {
	if sp.Account == nil || sp.Owner == "" {
		return nil
	}
	ra := &jobs.RunAs{Account: sp.Account.Name, Cgroup: sp.Cgroup(kind + "-" + id), Tmp: sp.Tmp}
	if runtime.GOOS == "windows" && sp.sp.cfg.Exe != "" && kind == JobsDir {
		ra.Wrapper = []string{sp.sp.cfg.Exe, "conhost", "--"}
	}
	return ra
}

// Env returns the base environment of the space's account (nil = longarmd's).
func (sp *Space) Env() ([]string, error) {
	if sp.Account == nil {
		return nil, nil
	}
	env, err := platform.AccountEnv(sp.Account)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && sp.Tmp != "" {
		env = append(env, "TMPDIR="+sp.Tmp)
	}
	return env, nil
}

// Home is the space account's home ("" = longarmd's).
func (sp *Space) Home() string {
	if sp.Account == nil {
		return ""
	}
	return sp.Account.Home
}

// FindJob looks a job up in every space.
func (s *Spaces) FindJob(id string) (*Space, supervisor.Job, error) {
	for _, sp := range s.All() {
		if j, err := sp.Jobs.Get(id); err == nil {
			return sp, j, nil
		}
	}
	return nil, supervisor.Job{}, supervisor.ErrNotFound
}

// SubmitJob submits spec in sp; job ids are unique across all spaces.
func (s *Spaces) SubmitJob(ctx context.Context, sp *Space, spec jobs.Spec, secrets map[string]string, in supervisor.Input) (supervisor.Job, error) {
	s.submit.Lock()
	for _, o := range s.All() {
		if o != sp {
			if _, err := o.Jobs.Get(spec.ID); err == nil {
				s.submit.Unlock()
				in.GitAuth.Clear()
				return supervisor.Job{}, supervisor.ErrAlreadyExists
			}
		}
	}
	s.submit.Unlock() // the space's manager serializes its own submits
	spec.RunAs = sp.RunAs(JobsDir, spec.ID)
	return sp.Jobs.SubmitWith(ctx, spec, secrets, in)
}

// SubmitSession submits a session spec in sp; created is false when an
// identical session existed (decided under the manager's submit lock).
func (s *Spaces) SubmitSession(ctx context.Context, sp *Space, spec jobs.Spec, secrets map[string]string, auth *jobs.GitAuth) (supervisor.Job, bool, error) {
	spec.RunAs = sp.RunAs(SessionsDir, spec.ID)
	return sp.Sessions.SubmitCreated(ctx, spec, secrets, supervisor.Input{GitAuth: auth})
}

// RunningJobs counts active jobs of every space.
func (s *Spaces) RunningJobs() int {
	n := 0
	for _, sp := range s.All() {
		n += sp.Jobs.RunningCount()
	}
	return n
}

// RunningSessions counts active sessions of every space.
func (s *Spaces) RunningSessions() int {
	n := 0
	for _, sp := range s.All() {
		n += sp.Sessions.RunningCount()
	}
	return n
}

// ListJobs lists the jobs of every space, newest first.
func (s *Spaces) ListJobs(states []jobs.State, limit int) []supervisor.Job {
	var all []supervisor.Job
	for _, sp := range s.All() {
		all = append(all, sp.Jobs.List(states, 0)...)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].Spec.CreatedAt.Equal(all[j].Spec.CreatedAt) {
			return all[i].Spec.CreatedAt.After(all[j].Spec.CreatedAt)
		}
		return all[i].Spec.ID > all[j].Spec.ID
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}

// Purge kills owner's jobs and sessions and deletes the user's data (and,
// per-user, the OS account with its home). Shared mode: the owner's jobs and
// sessions are removed from the shared space.
func (s *Spaces) Purge(ctx context.Context, owner string) error {
	if owner == "" {
		return errors.New("no owner")
	}
	if !s.PerUser() {
		var errs []error
		for _, m := range []*supervisor.Manager{s.shared.Jobs, s.shared.Sessions} {
			for _, j := range m.List(nil, 0) {
				if j.Spec.OwnerName() != owner {
					continue
				}
				if !j.Status.State.Terminal() {
					m.Kill(ctx, j.Spec.ID)
				}
				if err := m.Remove(j.Spec.ID); err != nil && !errors.Is(err, supervisor.ErrNotFound) {
					errs = append(errs, err)
				}
			}
		}
		return errors.Join(errs...)
	}
	s.mu.Lock()
	sp := s.users[owner]
	s.mu.Unlock()
	name := s.AccountName(owner)
	if sp != nil {
		for _, m := range []*supervisor.Manager{sp.Jobs, sp.Sessions} {
			for _, j := range m.List(nil, 0) {
				if !j.Status.State.Terminal() {
					m.Kill(ctx, j.Spec.ID)
				}
			}
		}
		sp.cancel()
		s.mu.Lock()
		delete(s.users, owner)
		sp.dropProfile()
		s.mu.Unlock()
		s.notify()
	}
	var errs []error
	root := filepath.Join(s.cfg.UsersDir, name)
	for i := 0; i < 20; i++ {
		err := os.RemoveAll(root)
		if err == nil {
			break
		}
		if i == 19 {
			errs = append(errs, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s.cgroup {
		os.Remove(filepath.Join(s.cfg.CgroupRoot, name))
	}
	if err := platform.DeleteAccount(name); err != nil {
		errs = append(errs, err)
	}
	s.mu.Lock()
	if err := s.loopbackLocked(); err != nil {
		s.log.Warn("loopback isolation: update after the purge", "err", err)
	}
	s.mu.Unlock()
	s.log.Info("user purged", "user", owner, "account", name)
	return errors.Join(errs...)
}

// initLoopback installs the loopback isolation between the accounts
// (Linux, per-user; docs/security.md) or, when it is off, removes a table
// left from before.
func (s *Spaces) initLoopback() error {
	mode := s.cfg.Loopback
	if mode == "" {
		mode = loopguard.Isolate
		if !loopguard.Supported {
			mode = loopguard.Allow
		}
	}
	if !s.PerUser() {
		// A shared-mode longarmd leaves the table alone: it may be another
		// (per-user) longarmd's on this machine. It only concerns accounts.
		return nil
	}
	if mode == loopguard.Allow {
		if loopguard.Supported {
			if err := loopguard.Remove(); err != nil {
				s.log.Warn("loopback isolation: remove the old nftables table", "table", "inet "+loopguard.Table, "err", err)
			}
			s.log.Warn("per-user isolation with [isolation] loopback = \"allow\": the accounts can connect to each other's loopback ports - see docs/security.md")
		}
		return nil
	}
	if !loopguard.Supported {
		if mode == loopguard.Require {
			return fmt.Errorf("[isolation] loopback = %q: loopback isolation between users is implemented on Linux only", mode)
		}
		s.log.Warn("[isolation] loopback = \"isolate\" has no effect here: loopback isolation between users is implemented on Linux only")
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loopback = mode
	return s.loopbackLocked()
}

// loopbackLocked replaces the rules for the current accounts and extra
// (one about to be used; s.mu held). If that fails: an error with
// loopback = "require", else a warning.
func (s *Spaces) loopbackLocked(extra ...*platform.Account) error {
	if s.loopback == "" {
		return nil
	}
	var users []loopguard.User
	for _, sp := range s.users {
		if sp.Account != nil {
			users = append(users, loopguard.User{Name: sp.Account.Name, UID: sp.Account.UID})
		}
	}
	for _, a := range extra {
		if a != nil {
			users = append(users, loopguard.User{Name: a.Name, UID: a.UID})
		}
	}
	err := applyLoopback(users)
	if err == nil {
		s.log.Info("loopback isolation between the accounts", "table", "inet "+loopguard.Table, "accounts", len(users))
		return nil
	}
	if s.loopback == loopguard.Require {
		return fmt.Errorf("loopback isolation ([isolation] loopback = \"require\"): %w", err)
	}
	s.log.Warn("loopback isolation between the accounts is NOT active: their processes can connect to each other's loopback ports "+
		"(install nftables, or set [isolation] loopback = \"allow\" to silence this) - see docs/security.md", "err", err)
	return nil
}

// applyLoopback installs the rules (a variable for tests).
var applyLoopback = loopguard.Apply

// secureUserDirs gives the account its private dirs in the space.
func secureUserDirs(root string, acct *platform.Account) error {
	for _, d := range []string{workspace.MirrorsDir, workspace.WorktreesDir, workspace.StagingDir, TmpDir} {
		if err := platform.SecureUserDir(filepath.Join(root, d), acct); err != nil {
			return err
		}
	}
	return nil
}
