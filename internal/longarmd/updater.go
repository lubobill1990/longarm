package longarmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/update"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// UpdateConfig is the [update] section of longarmd.toml (self-update, DESIGN
// §7.2). Updates need a hub and a build with an update public key.
type UpdateConfig struct {
	Disabled bool   `toml:"disabled"`
	Channel  string `toml:"channel"` // default "stable"
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
	channel  string
	interval time.Duration
	restart  func() // ends Run with update.ErrRestart
	canApply error  // why this installation can't update itself (nil = it can)
	kick     chan struct{}

	mu     sync.Mutex
	busy   bool // an update is being applied, or this version is on probation
	missed bool // a check was skipped while busy: run it when the probation ends
	status *pb.UpdateStatus
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
	u.status = s
	u.mu.Unlock()
	u.agent.send(&pb.NodeReport{Msg: &pb.NodeReport_Update{Update: s}}, false)
	u.agent.notify(&pb.LocalEvent{Kind: "update", Message: fmt.Sprintf("%s %s -> %s %s", s.State, s.From, s.To, s.Error)})
	u.agent.tel.event("update.status", map[string]string{"state": s.State, "from": s.From, "to": s.To, "error": s.Error})
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
	if !u.enabled() {
		return nil
	}
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
		Version: version.Version, Channel: u.channel})
	cancel()
	if err != nil {
		return err
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
	u.setStatus(&pb.UpdateStatus{State: "applying", From: version.Version, To: target})
	var files []update.File
	for _, f := range resp.Files {
		files = append(files, update.File{Name: f.Name, SHA256: f.Sha256, Size: f.Size, Signature: f.Signature})
	}
	fetch := func(ctx context.Context, ver, name string, w io.Writer) error {
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
	dctx, dcancel := context.WithTimeout(ctx, 15*time.Minute)
	err = update.Apply(dctx, u.cfg, target, files, fetch)
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
	u.setStatus(&pb.UpdateStatus{State: update.PhaseRestarting, From: version.Version, To: target})
	time.Sleep(500 * time.Millisecond) // let the report reach the hub
	u.restart()
	return nil
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
	u := &updater{addr: cfg.Hub.Address, agent: agent, channel: agent.channel, restart: restart,
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
