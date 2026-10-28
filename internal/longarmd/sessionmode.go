package longarmd

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/signin"
)

// User-session mode (Windows; the MSI's RUN=session, the default): longarmd
// runs as the signed-in user, started at that user's logon - no service,
// no account password (an Entra-only machine signed in with a security key
// has none). The pieces:
//
//   - a per-user scheduled task (SessionTaskXML) "at logon of <user>",
//     running only while the user is logged on (an interactive token: no
//     password stored), repeated every 5 minutes with "do not start a new
//     instance" - so a host that died comes back within minutes - with no
//     time limit, not stopped on battery;
//   - the host it starts (SessionHost: `longarm-tray --worker-host`, a
//     GUI-subsystem binary so that no console window opens at logon, or
//     `longarmd session-mode run`): one per user (SessionLockFile), it
//     runs `longarmd run` hidden and restarts it when it exits - at once
//     for a self-update (exit 75), else with a growing delay;
//   - the data dir is the user's own (~\.longarm, the default for a user):
//     no admin rights needed to sign in, and the tray / `longarmd login` /
//     `longarm` find it without an environment variable.
//
// Jobs and sessions run in the user's logon session: they keep running
// while an RDP connection is disconnected (the session stays), and stop at
// logoff or reboot until the user logs on again. A worker that must run
// without anyone logged on is a Windows service (RUN=service).

const (
	// SessionLockFile in the data dir: held by the one host per user.
	SessionLockFile = "longarmd-session.lock"
	// SessionStateFile: the host's and longarmd's process ids (stop,
	// uninstall).
	SessionStateFile = "longarmd-session.json"
	// SessionLogFile: the host's log (longarmd's own goes to longarmd.log).
	SessionLogFile = "longarmd-session.log"
	// EnvSupervised is set for a longarmd a SessionHost runs: it exits
	// with ExitRestart for a self-update (the host restarts it) instead of
	// relaunching itself.
	EnvSupervised = "LONGARM_SUPERVISED"
	// ExitRestart: longarmd exits with this code to be restarted with a
	// new binary (self-update).
	ExitRestart = 75
	// SessionTaskInterval: the task's repetition, which brings a host
	// back that died (a running one makes the task ignore the trigger).
	SessionTaskInterval = 5 * time.Minute
)

// SessionTaskName is the scheduled task of user sid's worker.
func SessionTaskName(sid string) string { return "LongarmWorker-" + sid }

// TrayTaskName is the on-demand task the MSI starts the tray with, in the
// installing user's session (the MSI runs elevated).
func TrayTaskName(sid string) string { return "LongarmTray-" + sid }

// ValidUserSID accepts a user's SID (S-1-5-21-..., S-1-12-1-... for Entra
// accounts) and refuses LocalSystem & co.: a user-session worker needs a
// user who logs on.
func ValidUserSID(sid string) error {
	s := strings.ToUpper(strings.TrimSpace(sid))
	if !strings.HasPrefix(s, "S-1-") || strings.ContainsAny(s, " \t\"'<>&") {
		return fmt.Errorf("%q is not a SID", sid)
	}
	switch s {
	case "S-1-5-18", "S-1-5-19", "S-1-5-20":
		return fmt.Errorf("%s is a system account: the user-session worker runs as the user who logs on (install as that user, or RUN=service)", sid)
	}
	return nil
}

// TaskSpec describes a scheduled task SessionTaskXML renders.
type TaskSpec struct {
	UserSID     string
	Description string
	Exe         string
	Args        []string
	WorkDir     string
	// AtLogon: start at the user's logon and every SessionTaskInterval
	// after (a host that died comes back); false: on demand only.
	AtLogon bool
}

type taskXMLDoc struct {
	XMLName  xml.Name `xml:"Task"`
	Version  string   `xml:"version,attr"`
	NS       string   `xml:"xmlns,attr"`
	Info     taskInfo `xml:"RegistrationInfo"`
	Triggers struct {
		Logon *taskLogonTrigger `xml:"LogonTrigger,omitempty"`
	} `xml:"Triggers"`
	Principals struct {
		Principal taskPrincipal `xml:"Principal"`
	} `xml:"Principals"`
	Settings taskSettings `xml:"Settings"`
	Actions  struct {
		Context string   `xml:"Context,attr"`
		Exec    taskExec `xml:"Exec"`
	} `xml:"Actions"`
}

type taskInfo struct {
	Author      string `xml:"Author"`
	Description string `xml:"Description"`
}

type taskLogonTrigger struct {
	Repetition struct {
		Interval          string `xml:"Interval"`
		StopAtDurationEnd bool   `xml:"StopAtDurationEnd"`
	} `xml:"Repetition"`
	Enabled bool   `xml:"Enabled"`
	UserID  string `xml:"UserId"`
}

type taskPrincipal struct {
	ID        string `xml:"id,attr"`
	UserID    string `xml:"UserId"`
	LogonType string `xml:"LogonType"`
	RunLevel  string `xml:"RunLevel"`
}

type taskSettings struct {
	MultipleInstancesPolicy    string `xml:"MultipleInstancesPolicy"`
	DisallowStartIfOnBatteries bool   `xml:"DisallowStartIfOnBatteries"`
	StopIfGoingOnBatteries     bool   `xml:"StopIfGoingOnBatteries"`
	AllowHardTerminate         bool   `xml:"AllowHardTerminate"`
	StartWhenAvailable         bool   `xml:"StartWhenAvailable"`
	RunOnlyIfNetworkAvailable  bool   `xml:"RunOnlyIfNetworkAvailable"`
	IdleSettings               struct {
		StopOnIdleEnd bool `xml:"StopOnIdleEnd"`
		RestartOnIdle bool `xml:"RestartOnIdle"`
	} `xml:"IdleSettings"`
	AllowStartOnDemand bool   `xml:"AllowStartOnDemand"`
	Enabled            bool   `xml:"Enabled"`
	Hidden             bool   `xml:"Hidden"`
	RunOnlyIfIdle      bool   `xml:"RunOnlyIfIdle"`
	WakeToRun          bool   `xml:"WakeToRun"`
	ExecutionTimeLimit string `xml:"ExecutionTimeLimit"`
	Priority           int    `xml:"Priority"`
	RestartOnFailure   *struct {
		Interval string `xml:"Interval"`
		Count    int    `xml:"Count"`
	} `xml:"RestartOnFailure,omitempty"`
}

type taskExec struct {
	Command          string `xml:"Command"`
	Arguments        string `xml:"Arguments,omitempty"`
	WorkingDirectory string `xml:"WorkingDirectory,omitempty"`
}

// SessionTaskXML renders a Task Scheduler 1.3 task definition (UTF-8;
// TaskXMLFile makes the UTF-16 file schtasks /Create /XML wants). The task
// runs as UserSID with the user's interactive token (only while the user
// is logged on: no password is stored), not elevated; no time limit (the
// default would stop it after 72 hours), not stopped on battery, normal
// priority (the default, 7, is below normal for CPU and I/O).
func SessionTaskXML(t TaskSpec) ([]byte, error) {
	if err := ValidUserSID(t.UserSID); err != nil {
		return nil, err
	}
	if t.Exe == "" {
		return nil, errors.New("task without a program")
	}
	d := taskXMLDoc{Version: "1.3", NS: "http://schemas.microsoft.com/windows/2004/02/mit/task"}
	d.Info = taskInfo{Author: "Longarm", Description: t.Description}
	if t.AtLogon {
		lt := &taskLogonTrigger{Enabled: true, UserID: t.UserSID}
		lt.Repetition.Interval = isoDuration(SessionTaskInterval)
		d.Triggers.Logon = lt
	}
	d.Principals.Principal = taskPrincipal{ID: "Author", UserID: t.UserSID, LogonType: "InteractiveToken", RunLevel: "LeastPrivilege"}
	s := &d.Settings
	s.MultipleInstancesPolicy = "IgnoreNew"
	s.AllowHardTerminate, s.StartWhenAvailable, s.AllowStartOnDemand, s.Enabled = true, true, true, true
	s.ExecutionTimeLimit = "PT0S"
	s.Priority = 5
	if t.AtLogon {
		s.RestartOnFailure = &struct {
			Interval string `xml:"Interval"`
			Count    int    `xml:"Count"`
		}{Interval: "PT1M", Count: 10}
	}
	d.Actions.Context = "Author"
	d.Actions.Exec = taskExec{Command: t.Exe, Arguments: windowsArgs(t.Args), WorkingDirectory: t.WorkDir}
	b, err := xml.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte("<?xml version=\"1.0\" encoding=\"UTF-16\"?>\n"), b...), nil
}

// TaskXMLFile encodes a task definition as schtasks /XML reads it:
// UTF-16LE with a byte order mark.
func TaskXMLFile(x []byte) []byte {
	u := utf16.Encode([]rune(string(x)))
	out := make([]byte, 2, 2+2*len(u))
	out[0], out[1] = 0xff, 0xfe
	for _, c := range u {
		out = append(out, byte(c), byte(c>>8))
	}
	return out
}

func isoDuration(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("PT%dM", int(d/time.Minute))
	}
	return fmt.Sprintf("PT%dS", int(d/time.Second))
}

// windowsArgs quotes arguments for a Windows command line (the rules of
// CommandLineToArgvW, as syscall.EscapeArg).
func windowsArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = escapeWinArg(a)
	}
	return strings.Join(q, " ")
}

func escapeWinArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, c := range s {
		switch c {
		case '\\':
			slashes++
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
		}
		slashes = 0
		b.WriteRune(c)
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}

// TaskScheduler is the part of Windows' Task Scheduler user-session mode
// uses (schtasks.exe on Windows; fakes in tests).
type TaskScheduler interface {
	// Register creates or replaces the task name from a definition
	// (SessionTaskXML).
	Register(name string, def []byte) error
	Delete(name string) error
	// Run starts the task now (in its user's session); End stops it.
	Run(name string) error
	End(name string) error
	Exists(name string) bool
}

// SessionInstallOptions for InstallSessionTask.
type SessionInstallOptions struct {
	UserSID string
	// HostExe and HostArgs: what the task runs (longarm-tray.exe
	// --worker-host, else longarmd.exe session-mode run).
	HostExe  string
	HostArgs []string
	// Start the host now (restarting one that runs: a new version).
	Start bool
	// DataDir: the user's data dir, to stop an older host's longarmd
	// before the new host starts ("" = leave it).
	DataDir string
	Sched   TaskScheduler
	Out     io.Writer
}

// InstallSessionTask registers (or replaces) the user's worker task and
// with Start (re)starts it: the old host ends, its longarmd is stopped
// (jobs and sessions keep running in their shims), the new host starts.
func InstallSessionTask(o SessionInstallOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	def, err := SessionTaskXML(TaskSpec{UserSID: o.UserSID, AtLogon: true,
		Description: "Longarm worker (longarmd) for this user: starts at logon, restarts it when it stops. Remove: uninstall Longarm, or longarmd session-mode uninstall.",
		Exe:         o.HostExe, Args: o.HostArgs, WorkDir: filepath.Dir(o.HostExe)})
	if err != nil {
		return err
	}
	name := SessionTaskName(o.UserSID)
	if err := o.Sched.Register(name, def); err != nil {
		return fmt.Errorf("register task %s: %w", name, err)
	}
	fmt.Fprintf(o.Out, "registered task %s (%s %s)\n", name, o.HostExe, windowsArgs(o.HostArgs))
	if !o.Start {
		return nil
	}
	o.Sched.End(name) // not running: an error, fine
	if o.DataDir != "" {
		StopSessionProcesses(o.DataDir, o.Out)
	}
	if err := o.Sched.Run(name); err != nil {
		return fmt.Errorf("start task %s: %w", name, err)
	}
	fmt.Fprintf(o.Out, "started task %s\n", name)
	return nil
}

// RemoveSessionTask deletes the user's worker task (and the tray task)
// and stops the host and its longarmd.
func RemoveSessionTask(sched TaskScheduler, sid, dataDir string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	var errs []error
	for _, name := range []string{SessionTaskName(sid), TrayTaskName(sid)} {
		if !sched.Exists(name) {
			continue
		}
		sched.End(name)
		if err := sched.Delete(name); err != nil {
			errs = append(errs, fmt.Errorf("delete task %s: %w", name, err))
			continue
		}
		fmt.Fprintf(out, "removed task %s\n", name)
	}
	if dataDir != "" {
		StopSessionProcesses(dataDir, out)
	}
	return errors.Join(errs...)
}

// LaunchInSession starts exe with args once in user sid's logon session,
// not elevated, through an on-demand task (TrayTaskName; no trigger, kept
// for the next install, removed with the user-session worker): the MSI
// runs elevated - as SYSTEM in its custom actions - and must not start the
// tray as itself.
func LaunchInSession(sched TaskScheduler, sid, exe string, args []string) error {
	def, err := SessionTaskXML(TaskSpec{UserSID: sid, Exe: exe, Args: args, WorkDir: filepath.Dir(exe),
		Description: "Longarm: starts the Longarm tray once after an install (in the user's session, not elevated)."})
	if err != nil {
		return err
	}
	name := TrayTaskName(sid)
	if err := sched.Register(name, def); err != nil {
		return err
	}
	return sched.Run(name)
}

// SessionState is SessionStateFile: who runs.
type SessionState struct {
	HostPID    int    `json:"host_pid"`
	HostStart  uint64 `json:"host_start"`
	ChildPID   int    `json:"longarmd_pid,omitempty"`
	ChildStart uint64 `json:"longarmd_start,omitempty"`
	Exe        string `json:"exe,omitempty"`
	// Restarts of longarmd by this host, and why the last one ended.
	Restarts int       `json:"restarts"`
	LastExit string    `json:"last_exit,omitempty"`
	Updated  time.Time `json:"updated"`
}

// LoadSessionState reads SessionStateFile (nil when there is none).
func LoadSessionState(dataDir string) (*SessionState, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, SessionStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st SessionState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (st *SessionState) save(dataDir string) error {
	st.Updated = time.Now().UTC()
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := filepath.Join(dataDir, SessionStateFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return platform.ReplaceFile(tmp, filepath.Join(dataDir, SessionStateFile))
}

// StopSessionProcesses stops the host and the longarmd it started (by the
// process ids in SessionStateFile, checked against their start times: a
// reused pid is left alone). Jobs and sessions keep running in their
// shims, as when the service stops.
func StopSessionProcesses(dataDir string, out io.Writer) {
	st, err := LoadSessionState(dataDir)
	if err != nil || st == nil {
		return
	}
	kill := func(what string, pid int, start uint64) {
		if pid <= 0 || pid == os.Getpid() || !platform.ProcessAlive(pid, start) {
			return
		}
		if err := platform.KillProcess(pid); err != nil {
			fmt.Fprintf(out, "stop %s (pid %d): %v\n", what, pid, err)
			return
		}
		fmt.Fprintf(out, "stopped %s (pid %d)\n", what, pid)
	}
	kill("the worker host", st.HostPID, st.HostStart)
	kill("longarmd", st.ChildPID, st.ChildStart)
}

// Proc is a started longarmd (SessionHost).
type Proc interface {
	PID() int
	// Wait returns the exit code (-1 when unknown) once it ended.
	Wait() (int, error)
}

// SessionHost runs longarmd for the user (the user-session worker's host):
// one per data dir, restarting longarmd whenever it exits.
type SessionHost struct {
	DataDir string
	// Start runs `longarmd run` for the data dir (hidden, with
	// EnvSupervised set).
	Start func(ctx context.Context) (Proc, error)
	// Running reports whether a longarmd holds the data dir already (one
	// started by hand, or left behind by a host that was killed): the
	// host then waits instead of starting a second one.
	Running func() bool
	// PrepareSignIn readies the data dir before each start (the default:
	// PrepareSessionSignIn).
	PrepareSignIn func() error
	Log           *slog.Logger
	// Sleep (tests): waits d or until ctx ends.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now (tests).
	Now func() time.Time
}

// Backoff of SessionHost: longarmd that ran at least restartReset is
// restarted after restartMin, one that failed sooner after twice the
// previous delay, at most restartMax. A self-update's exit restarts at once.
const (
	restartMin   = 2 * time.Second
	restartMax   = 2 * time.Minute
	restartReset = 5 * time.Minute
	adoptPoll    = 5 * time.Second
)

// RestartDelay is how long SessionHost waits before starting longarmd
// again after it exited with code having run for ran; prev is the last
// delay.
func RestartDelay(code int, ran, prev time.Duration) time.Duration {
	if code == ExitRestart {
		return 0
	}
	if ran >= restartReset || prev <= 0 {
		return restartMin
	}
	d := 2 * prev
	if d > restartMax {
		d = restartMax
	}
	return d
}

// ErrHostRunning: another host has the data dir.
var ErrHostRunning = errors.New("the user-session worker host already runs for this data dir")

// Run hosts longarmd until ctx ends. It returns ErrHostRunning when another
// host holds the data dir (the scheduled task's repetition then simply
// finds it running).
func (h *SessionHost) Run(ctx context.Context) error {
	if h.Log == nil {
		h.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if h.Sleep == nil {
		h.Sleep = sleepCtx
	}
	if h.Now == nil {
		h.Now = time.Now
	}
	if h.PrepareSignIn == nil {
		h.PrepareSignIn = func() error { return PrepareSessionSignIn(h.DataDir) }
	}
	if err := os.MkdirAll(h.DataDir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(h.DataDir, SessionLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := platform.LockFile(lock); err != nil {
		return ErrHostRunning
	}
	st, _ := LoadSessionState(h.DataDir)
	if st == nil {
		st = &SessionState{}
	}
	st.HostPID = os.Getpid()
	st.HostStart, _ = platform.ProcessStartTime(st.HostPID)
	st.Restarts = 0
	st.save(h.DataDir)
	h.Log.Info("worker host started", "data_dir", h.DataDir, "pid", st.HostPID)
	var delay time.Duration
	for ctx.Err() == nil {
		if h.Running != nil && h.Running() {
			// a longarmd we didn't start (or the previous host's): watch it
			if err := h.Sleep(ctx, adoptPoll); err != nil {
				break
			}
			continue
		}
		if err := h.PrepareSignIn(); err != nil {
			h.Log.Warn("preparing the sign-in", "err", err)
		}
		started := h.Now()
		p, err := h.Start(ctx)
		code := -1
		if err != nil {
			h.Log.Error("starting longarmd", "err", err)
			st.LastExit = "start: " + err.Error()
		} else {
			st.ChildPID = p.PID()
			st.ChildStart, _ = platform.ProcessStartTime(st.ChildPID)
			st.save(h.DataDir)
			h.Log.Info("longarmd started", "pid", st.ChildPID)
			var werr error
			code, werr = p.Wait()
			st.LastExit = fmt.Sprintf("exit %d", code)
			if werr != nil && code < 0 {
				st.LastExit = werr.Error()
			}
			h.Log.Info("longarmd ended", "pid", st.ChildPID, "code", code, "ran", h.Now().Sub(started).Round(time.Second), "err", werr)
			st.ChildPID, st.ChildStart = 0, 0
		}
		if ctx.Err() != nil {
			break
		}
		delay = RestartDelay(code, h.Now().Sub(started), delay)
		st.Restarts++
		st.save(h.DataDir)
		if code == ExitRestart {
			h.Log.Info("restarting longarmd for a self-update")
			delay = 0 // and the next failure starts over at restartMin
			continue
		}
		h.Log.Info("restarting longarmd", "in", delay)
		if err := h.Sleep(ctx, delay); err != nil {
			break
		}
	}
	h.Log.Info("worker host stopped")
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// DataDirRunning reports whether a longarmd holds dataDir's lock.
func DataDirRunning(dataDir string) bool {
	f, err := os.OpenFile(filepath.Join(dataDir, LockFile), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	return platform.LockFile(f) != nil
}

// PrepareSessionSignIn readies a user-session worker's data dir that is
// neither enrolled nor waiting for an enrollment: when a hub is known (the
// installer's, else this build's default: signin.FallbackHub) it records a
// sign-in to come (SetUpSignIn), so longarmd waits until the tray or
// `longarmd login` signs the machine in, then starts serving by itself.
func PrepareSessionSignIn(dataDir string) error {
	if _, err := os.Stat(filepath.Join(dataDir, TLSDir, pki.NodeCertFile)); err == nil {
		return nil
	}
	hf, err := hubclient.LoadFile(dataDir)
	if err != nil || hf.Address != "" {
		return err // a join code's enrollment, or a sign-in already pending
	}
	hub, fp, _ := signin.FallbackHub()
	if hub == "" {
		return nil
	}
	return SetUpSignIn(dataDir, hub, fp)
}

// ProfileDataDir is a user's longarmd data dir below the profile dir
// (~\.longarm, as DefaultDataDir for that user).
func ProfileDataDir(profile string) string { return filepath.Join(profile, homeDataDirName) }

// sessionHostLog opens the host's log file (kept below 4 MiB: started over
// when larger).
func sessionHostLog(dataDir string) (io.WriteCloser, error) {
	p := filepath.Join(dataDir, SessionLogFile)
	if fi, err := os.Stat(p); err == nil && fi.Size() > 4<<20 {
		os.Rename(p, p+".1")
	}
	return os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// NewSessionHost is the host for dataDir running longarmdExe (`longarmd
// session-mode run`, `longarm-tray --worker-host`), logging to
// SessionLogFile. close releases the log.
func NewSessionHost(dataDir, longarmdExe string) (h *SessionHost, close func(), err error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, nil, err
	}
	lf, err := sessionHostLog(dataDir)
	if err != nil {
		return nil, nil, err
	}
	h = &SessionHost{DataDir: dataDir, Log: slog.New(slog.NewTextHandler(lf, nil)),
		Running: func() bool { return DataDirRunning(dataDir) },
		Start: func(ctx context.Context) (Proc, error) {
			return startSupervised(longarmdExe, []string{"run", "--data-dir", dataDir, "--log-file", filepath.Join(dataDir, LogFileName)})
		}}
	return h, func() { lf.Close() }, nil
}

// SessionStatus describes the user-session worker for `longarmd
// session-mode status` and the tray.
func SessionStatus(dataDir string) string {
	var b bytes.Buffer
	st, err := LoadSessionState(dataDir)
	switch {
	case err != nil:
		fmt.Fprintf(&b, "state: %v\n", err)
	case st == nil:
		b.WriteString("host: never ran\n")
	default:
		host := "stopped"
		if platform.ProcessAlive(st.HostPID, st.HostStart) {
			host = fmt.Sprintf("running (pid %d)", st.HostPID)
		}
		fmt.Fprintf(&b, "host: %s, %d restarts of longarmd, last exit: %s\n", host, st.Restarts, orNone(st.LastExit))
	}
	if DataDirRunning(dataDir) {
		b.WriteString("longarmd: running\n")
	} else {
		b.WriteString("longarmd: not running\n")
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
