package updatesvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/lubobill1990/longarm/internal/update"
)

// Caller is who sent a request over the pipe (from its token).
type Caller struct {
	SID string
	// Admin: an elevated member of the local Administrators group.
	Admin bool
	// System: LocalSystem (the admin worker, the service itself).
	System bool
}

func (c Caller) String() string {
	switch {
	case c.System:
		return "SYSTEM"
	case c.Admin:
		return c.SID + " (administrator)"
	}
	return c.SID
}

// Settings are the machine's: the recorded hub and owner (HKLM, written by
// the installer: administrators only) and the auto-update setting.
type Settings interface {
	// Hub the installer recorded ("" = none).
	Hub() (string, error)
	// Owner is the SID of the user the MSI was installed for ("" = none).
	Owner() string
	// Auto: automatic updates on, every hours.
	Auto() (enabled bool, hours int)
	SetAuto(enabled bool, hours int) error
}

// Workspace is where packages are downloaded: a directory only SYSTEM can
// use, without reparse points on the way (windows.go; tests fake it).
type Workspace interface {
	// Prepare returns an empty directory for one install of version and
	// the path of its installer log.
	Prepare(version string) (dir, log string, err error)
	// Lock opens path so that nobody can change, replace or delete it
	// until it is closed; the package is verified through it.
	Lock(path string) (io.ReadCloser, error)
	// Remove deletes a finished install's directory.
	Remove(dir string)
}

// Launcher starts the installer detached from the service: the MSI stops
// (and starts again) LongarmUpdate, so msiexec must not be the service's
// child in its job (windows.go: a one-shot SYSTEM scheduled task).
type Launcher interface {
	Launch(msi, log string) error
	// Outcome is msiexec's exit code from its log; done = false while it
	// runs (or before it started).
	Outcome(log string) (code int, done bool, err error)
	// Forget removes what Launch left behind (the scheduled task).
	Forget()
}

// Store keeps State across restarts (the update ends with a new service).
type Store interface {
	Load() (*State, error)
	Save(*State) error
}

// State is what the service remembers.
type State struct {
	Pending   *Pending  `json:"pending,omitempty"`
	Last      *Result   `json:"last,omitempty"`
	Available string    `json:"available,omitempty"`
	Checked   time.Time `json:"checked,omitempty"`
}

// Pending is an install that was launched.
type Pending struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Dir     string    `json:"dir"`
	MSI     string    `json:"msi"`
	Log     string    `json:"log"`
	Started time.Time `json:"started"`
	By      string    `json:"by,omitempty"`
}

// Defaults.
const (
	DefaultAutoHours = 6
	maxAutoHours     = 24 * 7
	// IdleFor: auto-update waits until no job started for this long.
	DefaultIdleFor = 5 * time.Minute
	// MaxDefer: ... but not longer than this (shims survive an upgrade).
	DefaultMaxDefer = 24 * time.Hour
	// InstallTimeout: an install without an outcome after this failed.
	DefaultInstallTimeout = 2 * time.Hour
)

// Config of a Service.
type Config struct {
	Version   string // installed (this binary's)
	Arch      string // runtime.GOARCH: the MSI of this installation's architecture
	PublicKey string // the release key built into this binary
	Settings  Settings
	// NewSource: the release source of the recorded hub.
	NewSource func(hub string) (Source, error)
	Workspace Workspace
	Launcher  Launcher
	Store     Store
	Now       func() time.Time
	Log       *slog.Logger

	// Rate limits: every caller (by SID) RequestRate requests/s (burst
	// RequestBurst); install requests InstallEvery apart (burst
	// InstallBurst) for everybody together.
	RequestRate  rate.Limit
	RequestBurst int
	InstallEvery time.Duration
	InstallBurst int

	IdleFor, MaxDefer, InstallTimeout time.Duration
	// WatchEvery: how often a launched install's log is read (default 5 s).
	WatchEvery time.Duration
}

func (c *Config) defaults() {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.RequestRate == 0 {
		c.RequestRate = 2
	}
	if c.RequestBurst == 0 {
		c.RequestBurst = 20
	}
	if c.InstallEvery == 0 {
		c.InstallEvery = 30 * time.Second
	}
	if c.InstallBurst == 0 {
		c.InstallBurst = 3
	}
	if c.IdleFor == 0 {
		c.IdleFor = DefaultIdleFor
	}
	if c.MaxDefer == 0 {
		c.MaxDefer = DefaultMaxDefer
	}
	if c.InstallTimeout == 0 {
		c.InstallTimeout = DefaultInstallTimeout
	}
	if c.WatchEvery == 0 {
		c.WatchEvery = 5 * time.Second
	}
}

// Service is LongarmUpdate's logic.
type Service struct {
	cfg Config

	installs *rate.Limiter

	mu         sync.Mutex
	callers    map[string]*rate.Limiter
	state      State
	busy       bool // an install is being prepared or runs
	phase      string
	target     string
	started    time.Time
	message    string
	activity   time.Time // a worker's last job start
	refreshing bool
	autoNext   time.Time
	ctx        context.Context
	watchers   sync.WaitGroup
}

// New makes a Service (Start runs it).
func New(cfg Config) *Service {
	cfg.defaults()
	s := &Service{cfg: cfg, callers: map[string]*rate.Limiter{}, phase: PhaseIdle, ctx: context.Background(),
		installs: rate.NewLimiter(rate.Every(cfg.InstallEvery), cfg.InstallBurst)}
	if st, err := cfg.Store.Load(); err == nil && st != nil {
		s.state = *st
	} else if err != nil {
		cfg.Log.Warn("update state unreadable: starting afresh", "err", err)
	}
	return s
}

// Start reconciles an install the previous service launched (it usually
// ends with this new service) and runs the auto-update loop until ctx ends.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	p := s.state.Pending
	s.mu.Unlock()
	if p != nil {
		s.reconcile(ctx, p)
	}
	go s.autoLoop(ctx)
}

func (s *Service) reconcile(ctx context.Context, p *Pending) {
	if update.Compare(s.cfg.Version, p.To) == 0 {
		code, _, _ := s.cfg.Launcher.Outcome(p.Log)
		s.finish(p, true, code, "")
		return
	}
	code, done, err := s.cfg.Launcher.Outcome(p.Log)
	switch {
	case done:
		s.finish(p, false, code, fmt.Sprintf("the installer failed (msiexec exit code %d; log %s)", code, p.Log))
	case s.cfg.Now().Sub(p.Started) > s.cfg.InstallTimeout:
		msg := "the installer did not finish"
		if err != nil {
			msg += ": " + err.Error()
		}
		s.finish(p, false, 0, msg)
	default:
		s.mu.Lock()
		s.busy, s.phase, s.target, s.started = true, PhaseInstalling, p.To, p.Started
		s.mu.Unlock()
		s.watchers.Add(1)
		go s.watch(ctx, p, nil)
	}
}

// Wait waits for the goroutines that watch a launched install (they end
// with Start's context).
func (s *Service) Wait() { s.watchers.Wait() }

// finish cleans up after an install - its directory (every handle on the
// package is closed by now: watch closes its lock first), the scheduled
// task - and then records the result.
func (s *Service) finish(p *Pending, ok bool, code int, msg string) {
	if p.Dir != "" {
		s.cfg.Workspace.Remove(p.Dir)
	}
	s.cfg.Launcher.Forget()
	r := &Result{From: p.From, To: p.To, OK: ok, Code: code, Error: msg, At: s.cfg.Now().UTC(), By: p.By}
	s.mu.Lock()
	s.state.Pending = nil
	s.state.Last = r
	s.busy, s.phase, s.target, s.message = false, PhaseIdle, "", ""
	st := s.state
	s.mu.Unlock()
	if err := s.cfg.Store.Save(&st); err != nil {
		s.cfg.Log.Warn("saving the update state", "err", err)
	}
	if ok {
		s.cfg.Log.Info("update installed", "from", p.From, "to", p.To, "by", p.By)
	} else {
		s.cfg.Log.Error("update failed", "from", p.From, "to", p.To, "err", msg, "by", p.By)
	}
}

// watch waits for a launched installer's outcome (the MSI usually stops
// this service first: the next one reconciles). lock keeps the package
// unchanged meanwhile.
func (s *Service) watch(ctx context.Context, p *Pending, lock io.Closer) {
	defer s.watchers.Done()
	unlock := func() {
		if lock != nil {
			lock.Close() // before finish removes the directory (Windows can't delete an open file)
			lock = nil
		}
	}
	defer unlock()
	t := time.NewTicker(s.cfg.WatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		code, done, _ := s.cfg.Launcher.Outcome(p.Log)
		if done {
			ok := code == 0 || code == 3010 || code == 1641
			msg := ""
			if !ok {
				msg = fmt.Sprintf("the installer failed (msiexec exit code %d; log %s)", code, p.Log)
			}
			unlock()
			s.finish(p, ok, code, msg)
			return
		}
		if s.cfg.Now().Sub(p.Started) > s.cfg.InstallTimeout {
			unlock()
			s.finish(p, false, 0, "the installer did not finish within "+s.cfg.InstallTimeout.String())
			return
		}
	}
}

// Status is the service's state.
func (s *Service) Status() *Status {
	auto, hours := s.cfg.Settings.Auto()
	hub, _ := s.cfg.Settings.Hub()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &Status{Installed: s.cfg.Version, Available: s.state.Available, Checked: s.state.Checked, Hub: hub,
		Arch: s.cfg.Arch, State: s.phase, Target: s.target, Started: s.started, Message: s.message,
		Auto: auto, AutoIntervalHours: normHours(hours), CanVerify: strings.TrimSpace(s.cfg.PublicKey) != ""}
	if auto {
		st.AutoNext = s.autoNext
	}
	if s.state.Last != nil {
		l := *s.state.Last
		st.Last = &l
	}
	return st
}

func normHours(h int) int {
	switch {
	case h <= 0:
		return DefaultAutoHours
	case h > maxAutoHours:
		return maxAutoHours
	}
	return h
}

// allow applies the per-caller request limit.
func (s *Service) allow(c Caller) bool {
	key := c.SID
	if c.System {
		key = "S-1-5-18"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.callers[key]
	if l == nil {
		if len(s.callers) >= 256 {
			s.callers = map[string]*rate.Limiter{}
		}
		l = rate.NewLimiter(s.cfg.RequestRate, s.cfg.RequestBurst)
		s.callers[key] = l
	}
	return l.Allow()
}

func refuse(code, format string, a ...any) *Response {
	return &Response{OK: false, Code: code, Error: fmt.Sprintf(format, a...)}
}

// Handle answers one request of caller.
func (s *Service) Handle(ctx context.Context, c Caller, req *Request) *Response {
	if !s.allow(c) {
		return refuse(CodeRateLimited, "too many requests to the update service: try again in a minute")
	}
	switch req.Op {
	case OpStatus:
		s.maybeRefresh()
		return &Response{OK: true, Phase: s.Status().State, Status: s.Status()}
	case OpActivity:
		s.mu.Lock()
		s.activity = s.cfg.Now()
		s.mu.Unlock()
		return &Response{OK: true}
	case OpSetAuto:
		owner := s.cfg.Settings.Owner()
		if !c.Admin && !c.System && (owner == "" || !strings.EqualFold(owner, c.SID)) {
			s.cfg.Log.Warn("set-auto refused", "caller", c.String())
			return refuse(CodeDenied, "only this machine's owner or a local administrator may change automatic updates")
		}
		h := normHours(req.IntervalHours)
		if err := s.cfg.Settings.SetAuto(req.Auto, h); err != nil {
			return refuse(CodeFailed, "saving the setting: %v", err)
		}
		s.cfg.Log.Info("auto-update set", "auto", req.Auto, "hours", h, "by", c.String(), "requester", req.Requester)
		s.mu.Lock()
		s.autoNext = time.Time{}
		s.mu.Unlock()
		st := s.Status()
		return &Response{OK: true, Phase: st.State, Status: st}
	case OpInstall:
		if req.Reinstall && !c.Admin {
			return refuse(CodeDenied, "installing the same version again is for a local administrator only")
		}
		if req.Latest == (req.Version != "") {
			return refuse(CodeInvalid, "install: a version or latest")
		}
		if req.Version != "" && !update.ValidVersion(req.Version) {
			return refuse(CodeInvalid, "invalid version %q", req.Version)
		}
		if !s.installs.Allow() {
			return refuse(CodeRateLimited, "an install was requested moments ago: try again in a minute")
		}
		by := c.String()
		if req.Requester != "" {
			by += " for " + clip(req.Requester, 120)
		}
		return s.install(ctx, req, by)
	}
	return refuse(CodeInvalid, "unknown request %q", clip(req.Op, 40))
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Service) setPhase(phase, target, msg string) {
	s.mu.Lock()
	s.phase, s.target, s.message = phase, target, msg
	s.mu.Unlock()
}

// install resolves, downloads, verifies and launches one install (one at
// a time). The hub is always the recorded one: nothing in req names it.
func (s *Service) install(ctx context.Context, req *Request, by string) *Response {
	s.mu.Lock()
	if s.busy {
		t := s.target
		s.mu.Unlock()
		return refuse(CodeBusy, "an update (to %s) is in progress", t)
	}
	s.busy, s.started = true, s.cfg.Now().UTC()
	s.mu.Unlock()
	launched := false
	defer func() {
		if !launched {
			s.mu.Lock()
			s.busy, s.phase, s.target, s.message = false, PhaseIdle, "", ""
			s.mu.Unlock()
		}
	}()
	if strings.TrimSpace(s.cfg.PublicKey) == "" {
		return refuse(CodeUnavailable, "this build of the update service has no release key: it installs nothing")
	}
	hub, err := s.cfg.Settings.Hub()
	if err != nil || hub == "" {
		return refuse(CodeUnavailable, "no hub is recorded for this machine (HKLM\\Software\\Longarm Hub): sign in first")
	}
	src, err := s.cfg.NewSource(hub)
	if err != nil {
		return refuse(CodeUnavailable, "hub %s: %v", hub, err)
	}
	s.setPhase(PhaseChecking, req.Version, "asking "+hub)
	var rel *Release
	if req.Latest {
		rel, err = src.Latest(ctx)
	} else {
		rel, err = src.Get(ctx, req.Version)
	}
	if errors.Is(err, ErrNoRelease) {
		if req.Latest {
			return refuse(CodeNotFound, "hub %s has no stable release", hub)
		}
		return refuse(CodeNotFound, "hub %s has no release %s", hub, req.Version)
	}
	if err != nil {
		return refuse(CodeUnavailable, "hub %s: %v", hub, err)
	}
	to, from := rel.Version, s.cfg.Version
	if req.Latest {
		s.mu.Lock()
		s.state.Available, s.state.Checked = to, s.cfg.Now().UTC()
		s.mu.Unlock()
	}
	switch err := update.CheckNewer(from, to); {
	case errors.Is(err, update.ErrSameVersion) && !req.Reinstall:
		return &Response{OK: true, Phase: PhaseUpToDate, From: from, To: to, Message: "Longarm " + to + " is installed already"}
	case errors.Is(err, update.ErrSameVersion):
	case errors.Is(err, update.ErrDowngrade):
		return refuse(CodeRefused, "%v", err)
	case err != nil:
		return refuse(CodeRefused, "%v", err)
	}
	name := update.MSIName(s.cfg.Arch)
	f := rel.File(name)
	if f == nil {
		return refuse(CodeNotFound, "release %s has no %s (no MSI for this machine's architecture, %s)", to, name, s.cfg.Arch)
	}
	if f.Size <= 0 || f.Size > update.MaxPackageSize || f.SHA256 == "" {
		return refuse(CodeInvalid, "release %s: the record of %s has no usable size / sha256", to, name)
	}

	s.setPhase(PhaseDownloading, to, fmt.Sprintf("downloading %s (%d MB)", name, (f.Size+(1<<20)-1)>>20))
	dir, logPath, err := s.cfg.Workspace.Prepare(to)
	if err != nil {
		return refuse(CodeFailed, "the download directory: %v", err)
	}
	keep := false
	defer func() {
		if !keep {
			s.cfg.Workspace.Remove(dir)
		}
	}()
	msi := filepath.Join(dir, name)
	if err := downloadFile(ctx, src, to, name, msi, f.Size); err != nil {
		return refuse(CodeUnavailable, "downloading %s: %v", name, err)
	}
	var sig bytes.Buffer
	if err := src.Download(ctx, to, name+update.SigExt, &sig, update.MaxSignatureSize); err != nil {
		if errors.Is(err, ErrNoRelease) {
			return refuse(CodeInvalid, "%s %s is not signed: refusing it", name, to)
		}
		return refuse(CodeUnavailable, "downloading %s%s: %v", name, update.SigExt, err)
	}

	s.setPhase(PhaseVerifying, to, "verifying "+name)
	lock, err := s.cfg.Workspace.Lock(msi)
	if err != nil {
		return refuse(CodeFailed, "locking %s: %v", name, err)
	}
	data, err := io.ReadAll(io.LimitReader(lock, update.MaxPackageSize+1))
	if err == nil {
		err = update.VerifyPackage(s.cfg.PublicKey, data, sig.Bytes(), name, to, f.SHA256, f.Size)
	}
	if err != nil {
		lock.Close()
		s.cfg.Log.Error("refusing a package", "file", name, "version", to, "err", err, "by", by)
		return refuse(CodeInvalid, "refusing %s %s: %v", name, to, err)
	}

	p := &Pending{From: from, To: to, Dir: dir, MSI: msi, Log: logPath, Started: s.cfg.Now().UTC(), By: by}
	s.mu.Lock()
	s.state.Pending = p
	st := s.state
	s.mu.Unlock()
	if err := s.cfg.Store.Save(&st); err != nil {
		lock.Close()
		s.mu.Lock()
		s.state.Pending = nil
		s.mu.Unlock()
		return refuse(CodeFailed, "saving the update state: %v", err)
	}
	s.cfg.Log.Info("installing", "from", from, "to", to, "msi", msi, "log", logPath, "by", by)
	if err := s.cfg.Launcher.Launch(msi, logPath); err != nil {
		lock.Close()
		s.finish(p, false, 0, "starting the installer: "+err.Error())
		keep = true // finish removed it
		return refuse(CodeFailed, "starting the installer: %v", err)
	}
	keep, launched = true, true
	s.setPhase(PhaseInstalling, to, "installing "+to+" (Longarm restarts; jobs and sessions keep running)")
	s.watchers.Add(1)
	go s.watch(s.ctx, p, lock)
	return &Response{OK: true, Phase: PhaseInstalling, From: from, To: to,
		Message: fmt.Sprintf("installing Longarm %s (from %s); Longarm restarts, running jobs and sessions keep running", to, from)}
}

func downloadFile(ctx context.Context, src Source, version, name, path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = src.Download(ctx, version, name, f, size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// maybeRefresh looks up the hub's latest stable release in the background
// when the last look is older than an hour.
func (s *Service) maybeRefresh() {
	s.mu.Lock()
	if s.refreshing || s.cfg.Now().Sub(s.state.Checked) < time.Hour {
		s.mu.Unlock()
		return
	}
	s.refreshing = true
	ctx := s.ctx
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			s.refreshing = false
			s.mu.Unlock()
		}()
		s.refresh(ctx)
	}()
}

func (s *Service) refresh(ctx context.Context) (string, error) {
	hub, err := s.cfg.Settings.Hub()
	if err != nil || hub == "" {
		return "", errors.New("no hub recorded")
	}
	src, err := s.cfg.NewSource(hub)
	if err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	rel, err := src.Latest(cctx)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.state.Available, s.state.Checked = rel.Version, s.cfg.Now().UTC()
	st := s.state
	s.mu.Unlock()
	s.cfg.Store.Save(&st)
	return rel.Version, nil
}

// autoLoop: with auto-update on, the hub's latest stable release is
// installed every few hours - when no job started for IdleFor (a job that
// starts during the install keeps running anyway: its shim survives), at
// most MaxDefer later. With it off, the loop only refreshes "available".
func (s *Service) autoLoop(ctx context.Context) {
	first := 10*time.Minute + time.Duration(rand.Int64N(int64(10*time.Minute)))
	s.mu.Lock()
	s.autoNext = s.cfg.Now().Add(first)
	s.mu.Unlock()
	var due time.Time
	for {
		s.mu.Lock()
		next := s.autoNext
		if next.IsZero() { // the setting changed: soon
			next = s.cfg.Now().Add(time.Minute)
			s.autoNext = next
		}
		s.mu.Unlock()
		wait := time.Until(next)
		if wait > time.Minute {
			wait = time.Minute // re-read autoNext (a changed setting)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(wait, time.Second)):
		}
		if s.cfg.Now().Before(next) {
			continue
		}
		auto, hours := s.cfg.Settings.Auto()
		interval := time.Duration(normHours(hours)) * time.Hour
		if !auto {
			due = time.Time{}
			s.refresh(ctx)
			s.setNext(interval)
			continue
		}
		if due.IsZero() {
			due = s.cfg.Now()
		}
		if retry := s.autoStep(ctx, due); retry {
			s.setNext(5 * time.Minute)
			continue
		}
		due = time.Time{}
		s.setNext(interval)
	}
}

func (s *Service) setNext(d time.Duration) {
	s.mu.Lock()
	s.autoNext = s.cfg.Now().Add(d)
	s.mu.Unlock()
}

// autoStep runs one automatic update; retry = try again in a few minutes
// (busy, or not idle yet).
func (s *Service) autoStep(ctx context.Context, due time.Time) (retry bool) {
	s.mu.Lock()
	busy, last := s.busy, s.activity
	s.mu.Unlock()
	if busy {
		return true
	}
	now := s.cfg.Now()
	if !last.IsZero() && now.Sub(last) < s.cfg.IdleFor && now.Sub(due) < s.cfg.MaxDefer {
		s.cfg.Log.Info("auto-update: a job started recently; waiting", "last_job", last)
		return true
	}
	resp := s.install(ctx, &Request{Op: OpInstall, Latest: true}, "auto-update")
	if !resp.OK && resp.Code != CodeNotFound {
		s.cfg.Log.Warn("auto-update", "err", resp.Error, "code", resp.Code)
	}
	return resp.Code == CodeBusy
}
