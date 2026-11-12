package longarmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"longarm/internal/hubclient"
	"longarm/internal/update"
	"longarm/internal/version"
	pb "longarm/proto/longarm/v1"
)

// UpdateConfig is the [update] section of longarmd.toml (self-update, DESIGN
// §7.2). Updates need a hub and a build with an update public key.
type UpdateConfig struct {
	Disabled bool   `toml:"disabled"`
	Channel  string `toml:"channel"` // stable | preview | dev; default: the machine's (the tray), else stable
	// Interval between checks (default 1h; the hub also nudges on a push).
	Interval Duration `toml:"interval"`
	// HealthWindow: a new version must stay healthy this long (default 60s).
	HealthWindow Duration `toml:"health_window"`
	// StartGrace: the watchdog waits this long for the new version to come
	// up at all before it rolls back (default 2m).
	StartGrace Duration `toml:"start_grace"`
}

// updater checks the hub for the version this worker should run and
// applies it (see internal/update).
type updater struct {
	cfg      update.Config
	addr     string
	agent    *hubAgent
	interval time.Duration
	restart  func() // ends Run with update.ErrRestart
	canApply error  // why this installation can't update itself (nil = it can)
	// remote: how longarm worker update installs a release here
	// (machineUpdater.RemoteMethod; nil: "")
	remote func() string
	kick   chan struct{}

	mu     sync.Mutex
	busy   bool // an update is being applied, or this version is on probation
	missed bool // a check was skipped while busy: run it when the probation ends
	status *pb.UpdateStatus
	// the update being applied (the hub's NodeInfo.update): its attempt,
	// who asked (rollout | owner) and when it started
	attempt   string
	initiator string
	started   time.Time
	lastProg  time.Time // the last progress report
}

func (u *updater) enabled() bool { return u.canApply == nil }

// trigger asks for a check now (welcome, hub hint).
func (u *updater) trigger() {
	select {
	case u.kick <- struct{}{}:
	default:
	}
}

// Status is the last update outcome (for hello and the local API).
func (u *updater) Status() *pb.UpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.status == nil {
		return nil
	}
	return proto.Clone(u.status).(*pb.UpdateStatus)
}

func (u *updater) setStatus(s *pb.UpdateStatus) {
	s.At = timestamppb.Now()
	u.mu.Lock()
	if u.attempt != "" && s.To != "" {
		s.Attempt, s.Initiator, s.Started, s.Method = u.attempt, u.initiator, timestamppb.New(u.started), "self"
	}
	u.status = s
	u.mu.Unlock()
	u.agent.send(&pb.NodeReport{Msg: &pb.NodeReport_Update{Update: s}}, false)
	u.agent.notify(&pb.LocalEvent{Kind: "update", Message: fmt.Sprintf("%s %s -> %s %s", s.State, s.From, s.To, s.Error)})
	u.agent.tel.event("update.status", map[string]string{"state": s.State, "from": s.From, "to": s.To, "error": s.Error})
}

// begin starts an update attempt (initiator: rollout | owner).
func (u *updater) begin(initiator string) {
	u.mu.Lock()
	u.started = time.Now()
	u.attempt, u.initiator = fmt.Sprintf("self-%d", u.started.UnixNano()), initiator
	u.mu.Unlock()
}

// progressFetch is fetch reporting the download's progress to the hub
// (every 2 s at most); total: what the files to download weigh.
func (u *updater) progressFetch(fetch update.Fetch, from, to string, total int64) update.Fetch {
	var done int64
	var mu sync.Mutex
	return func(ctx context.Context, ver, name string, w io.Writer) error {
		return fetch(ctx, ver, name, writerFunc(func(b []byte) (int, error) {
			n, err := w.Write(b)
			mu.Lock()
			done += int64(n)
			d := done
			mu.Unlock()
			u.mu.Lock()
			due := time.Since(u.lastProg) >= 2*time.Second
			if due {
				u.lastProg = time.Now()
			}
			u.mu.Unlock()
			if due {
				u.setStatus(&pb.UpdateStatus{State: "applying", Phase: "downloading", From: from, To: to, BytesDone: min(d, total), BytesTotal: total,
					Message: "downloading and verifying " + to})
			}
			return n, err
		}))
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// plannedBytes is what Apply downloads of files: the binaries this
// installation has (update.Binaries next to longarmd).
func plannedBytes(exe string, files []update.File) int64 {
	var total int64
	for _, f := range files {
		for _, bin := range update.Binaries {
			if f.Name != update.ReleaseName(bin, runtime.GOOS, runtime.GOARCH) {
				continue
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(exe), exeFile(bin))); bin != "longarmd" && err != nil {
				continue // not installed here: not updated
			}
			total += f.Size
		}
	}
	return total
}

func exeFile(bin string) string {
	if runtime.GOOS == "windows" {
		return bin + ".exe"
	}
	return bin
}

func (u *updater) Run(ctx context.Context) {
	t := time.NewTicker(u.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-u.kick:
		case <-t.C:
		}
		if err := u.check(ctx); err != nil && ctx.Err() == nil {
			u.cfg.Log.Warn("update check", "err", err)
		}
	}
}

func (u *updater) check(ctx context.Context) error {
	u.mu.Lock()
	if u.busy {
		u.missed = true
		u.mu.Unlock()
		return nil
	}
	u.mu.Unlock()
	conn, err := hubclient.Dial(u.addr, u.agent.live)
	if err != nil {
		return err
	}
	defer conn.Close()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	resp, err := conn.Fleet().CheckUpdate(cctx, &pb.UpdateCheckRequest{Os: runtime.GOOS, Arch: runtime.GOARCH,
		Version: version.Version, Channel: u.agent.releaseChannel()})
	cancel()
	if err != nil {
		return err
	}
	if !u.enabled() {
		// It can't install the update itself (an MSI install under
		// Program Files, a user-session worker): it says so (status, the
		// hub's node list, the tray); the MSI or install script updates it.
		method := ""
		if u.remote != nil {
			method = u.remote()
		}
		u.agent.setUpdateAvailable(availableUpdate(version.Version, cmp.Or(resp.Version, resp.LatestVersion), u.canApply, method))
		return nil
	}
	target := resp.Version
	if target == "" || target == version.Version {
		return nil
	}
	if !update.ValidVersion(target) {
		return fmt.Errorf("hub offered an invalid version %q", target)
	}
	if update.Skipped(u.cfg.DataDir, target) {
		u.cfg.Log.Info("update: target version failed here before; staying", "target", target)
		return nil
	}
	u.mu.Lock()
	u.busy = true
	u.mu.Unlock()
	u.cfg.Log.Info("update: applying", "from", version.Version, "to", target)
	u.begin("rollout")
	files := releaseFiles(resp.Files)
	u.setStatus(&pb.UpdateStatus{State: "applying", Phase: "downloading", From: version.Version, To: target, BytesTotal: plannedBytes(u.cfg.Exe, files)})
	dctx, dcancel := context.WithTimeout(ctx, 15*time.Minute)
	err = update.Apply(dctx, u.cfg, target, files, u.progressFetch(hubFetch(conn), version.Version, target, plannedBytes(u.cfg.Exe, files)))
	dcancel()
	if err != nil {
		u.mu.Lock()
		u.busy = false
		u.mu.Unlock()
		// "error" (unlike "failed") doesn't halt the rollout: the hub offers
		// the version again at the next check.
		u.setStatus(&pb.UpdateStatus{State: "error", From: version.Version, To: target, Error: err.Error()})
		return err
	}
	u.setStatus(&pb.UpdateStatus{State: update.PhaseRestarting, Phase: "restarting", From: version.Version, To: target})
	time.Sleep(500 * time.Millisecond) // let the report reach the hub
	u.restart()
	return nil
}

func releaseFiles(fs []*pb.ReleaseFile) []update.File {
	var files []update.File
	for _, f := range fs {
		files = append(files, update.File{Name: f.Name, SHA256: f.Sha256, Size: f.Size, Signature: f.Signature})
	}
	return files
}

// hubFetch downloads release files through the hub (Fleet.DownloadRelease).
func hubFetch(conn *hubclient.Conn) func(ctx context.Context, ver, name string, w io.Writer) error {
	return func(ctx context.Context, ver, name string, w io.Writer) error {
		st, err := conn.Fleet().DownloadRelease(ctx, &pb.ReleaseFileRequest{Version: ver, Name: name})
		if err != nil {
			return err
		}
		for {
			c, err := st.Recv()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if _, err := w.Write(c.Data); err != nil {
				return err
			}
		}
	}
}

// releaseVersion: a released version (development builds: "dev").
var releaseVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+`)

// availableUpdate is the notice for a worker running running that can't
// update itself (why) when the hub has target: nil unless target is newer.
// method: how its owner's longarm worker update would install it ("msi",
// "none"; the UI offers "Update to X" only when it can work).
func availableUpdate(running, target string, why error, method string) *pb.UpdateAvailable {
	if !releaseVersion.MatchString(target) || !update.ValidVersion(target) || !releaseVersion.MatchString(running) ||
		update.Compare(target, running) <= 0 {
		return nil
	}
	reason := "this installation can't update itself"
	if why != nil {
		reason = why.Error()
	}
	if method == "self" { // not here: why says it can't; keep the notice honest
		method = "none"
	}
	return &pb.UpdateAvailable{Version: target, Reason: reason, Method: method}
}

// startup handles a finished or in-progress update when longarmd starts: the
// outcome goes to the hub (hello + report), and a new version runs its
// health check (hub session within the window).
func (u *updater) startup(ctx context.Context, hubOn bool) {
	chk, out := update.Startup(u.cfg)
	if out != nil && !update.Reported(u.cfg.DataDir) {
		u.mu.Lock()
		u.status = &pb.UpdateStatus{State: out.State, From: out.From, To: out.To, Error: out.Error, At: timestamppb.New(out.At)}
		u.mu.Unlock()
	}
	if chk == nil {
		return
	}
	u.mu.Lock()
	u.busy = true
	u.mu.Unlock()
	go func() {
		res := chk.Run(ctx, func() error {
			if !hubOn || u.agent.everConnected() {
				return nil
			}
			return errors.New("no hub session since the update")
		})
		if res == nil {
			return
		}
		u.mu.Lock()
		u.busy = res.State != update.PhaseOK // unhealthy: the watchdog is about to replace us
		missed := u.missed && !u.busy
		u.missed = false
		u.mu.Unlock()
		u.setStatus(&pb.UpdateStatus{State: res.State, From: res.From, To: res.To, Error: res.Error})
		if res.State == update.PhaseOK {
			update.MarkReported(u.cfg.DataDir)
		}
		if missed {
			u.trigger() // e.g. a push that arrived during the health window
		}
	}()
}

// reported is called once the hub acknowledged a hello carrying status.
func (u *updater) reported(s *pb.UpdateStatus) {
	if s == nil {
		return
	}
	switch s.State {
	case update.PhaseOK, update.PhaseRolledBack, update.PhaseFailed:
		update.MarkReported(u.cfg.DataDir)
	}
}

func newUpdater(cfg Config, agent *hubAgent, exe string, log *slog.Logger, restart func()) *updater {
	u := &updater{addr: cfg.Hub.Address, agent: agent, restart: restart,
		kick: make(chan struct{}, 1), interval: cfg.Update.Interval.Duration}
	if u.interval <= 0 {
		u.interval = time.Hour
	}
	mode := cfg.UpdateMode
	if mode == "" {
		mode = "exec"
	}
	u.cfg = update.Config{DataDir: cfg.DataDir, Exe: exe, Version: version.Version,
		HealthWindow: cfg.Update.HealthWindow.Duration, StartGrace: cfg.Update.StartGrace.Duration,
		Mode: mode, ServiceName: cfg.ServiceName, Args: cfg.Args, Log: log.With("component", "update")}
	if u.cfg.PublicKey == "" {
		u.cfg.PublicKey = update.PublicKey
	}
	if u.canApply = update.CanApply(exe, u.cfg.PublicKey); u.canApply != nil {
		u.cfg.Log.Info("self-update unavailable", "why", u.canApply)
	}
	return u
}

// Phases of an owner's update (MachineUpdate.Start, method "self").
const (
	wantUpToDate   = "up_to_date"
	wantRestarting = "restarting"
)

// errCode is a refusal with the MachineUpdateEvent code.
type errCode struct {
	code string
	err  error
}

func (e *errCode) Error() string { return e.err.Error() }
func (e *errCode) Unwrap() error { return e.err }

func coded(code string, format string, a ...any) error {
	return &errCode{code: code, err: fmt.Errorf(format, a...)}
}

// applyWanted is an owner's update of an installation that updates itself
// (longarm worker update): want ("latest" or a version) from the hub, only
// when it is newer than this version, signatures verified as for a
// rollout. It returns the version and wantUpToDate or wantRestarting (the
// restart follows a moment later); progress hears the phases.
func (u *updater) applyWanted(ctx context.Context, want string, progress func(phase, msg string)) (string, string, error) {
	if u.canApply != nil {
		return "", "", coded("unsupported", "this installation can't update itself: %v", u.canApply)
	}
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return "", "", coded("busy", "an update is in progress (or the new version is still on probation)")
	}
	u.busy = true
	u.mu.Unlock()
	keepBusy := false
	defer func() {
		if !keepBusy {
			u.mu.Lock()
			u.busy = false
			u.mu.Unlock()
		}
	}()
	progress("checking", "asking the hub for "+want)
	conn, err := hubclient.Dial(u.addr, u.agent.live)
	if err != nil {
		return "", "", coded("unavailable", "hub: %v", err)
	}
	defer conn.Close()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	resp, err := conn.Fleet().CheckUpdate(cctx, &pb.UpdateCheckRequest{Os: runtime.GOOS, Arch: runtime.GOARCH,
		Version: version.Version, Channel: u.agent.releaseChannel(), Want: want})
	cancel()
	if err != nil {
		switch status.Code(err) {
		case codes.NotFound:
			return "", "", coded("not_found", "%s", status.Convert(err).Message())
		case codes.InvalidArgument:
			return "", "", coded("invalid", "%s", status.Convert(err).Message())
		}
		return "", "", coded("unavailable", "hub: %v", err)
	}
	to := resp.Version
	if to == "" || (want != "latest" && to != want) {
		return "", "", coded("unavailable", "the hub did not offer %s (it is older than this worker: update the hub)", want)
	}
	switch err := update.CheckNewer(version.Version, to); {
	case errors.Is(err, update.ErrSameVersion):
		return to, wantUpToDate, nil
	case err != nil:
		return "", "", coded("refused", "%v", err)
	}
	progress("downloading", "downloading and verifying "+to)
	u.cfg.Log.Info("update: applying (asked by the owner)", "from", version.Version, "to", to)
	u.begin("owner")
	files := releaseFiles(resp.Files)
	total := plannedBytes(u.cfg.Exe, files)
	u.setStatus(&pb.UpdateStatus{State: "applying", Phase: "downloading", From: version.Version, To: to, BytesTotal: total})
	dctx, dcancel := context.WithTimeout(ctx, 15*time.Minute)
	err = update.Apply(dctx, u.cfg, to, files, u.progressFetch(hubFetch(conn), version.Version, to, total))
	dcancel()
	if err != nil {
		// "error": the owner's attempt failed; it doesn't halt a rollout
		u.setStatus(&pb.UpdateStatus{State: "error", From: version.Version, To: to, Error: err.Error()})
		return "", "", coded("failed", "%v", err)
	}
	keepBusy = true
	u.setStatus(&pb.UpdateStatus{State: update.PhaseRestarting, Phase: "restarting", From: version.Version, To: to})
	go func() {
		time.Sleep(1500 * time.Millisecond) // the answer and the report go out first
		u.restart()
	}()
	return to, wantRestarting, nil
}
