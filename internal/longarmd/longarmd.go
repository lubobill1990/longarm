// Package longarmd wires the worker daemon together: data dir + single-instance
// lock, TLS bundle, job supervisor, metrics sampler, one gRPC server served on
// every configured transport listener.
package longarmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/conntrack"
	"github.com/lubobill1990/longarm/internal/gc"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/localapi"
	"github.com/lubobill1990/longarm/internal/metrics"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/sched"
	"github.com/lubobill1990/longarm/internal/server"
	"github.com/lubobill1990/longarm/internal/sshd"
	"github.com/lubobill1990/longarm/internal/tenancy"
	"github.com/lubobill1990/longarm/internal/toolchain"
	"github.com/lubobill1990/longarm/internal/transport"
	"github.com/lubobill1990/longarm/internal/transport/longnet" // also registers longnet://
	"github.com/lubobill1990/longarm/internal/update"
	"github.com/lubobill1990/longarm/internal/userspace"
	"github.com/lubobill1990/longarm/internal/version"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Data-dir layout.
const (
	ConfigFile         = "longarmd.toml"
	TLSDir             = "tls"
	JobsDir            = "jobs"
	SessionsDir        = "sessions"
	TSNetDir           = "tsnet"
	LockFile           = "longarmd.lock"
	EndpointsFile      = "longarmd.endpoints" // listening URLs of the running longarmd, one per line
	AuditFile          = "audit.log"
	LogFileName        = "longarmd.log"
	SSHHostKeyFile     = "ssh_host_ed25519_key"
	AuthorizedKeysFile = "authorized_keys" // keys for direct sshd listeners
)

// DefaultDataDir is ~/.longarm (longarmd runs as a normal user account);
// --data-dir or LONGARM_DATA_DIR override it.
func DefaultDataDir() string {
	if d := os.Getenv("LONGARM_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".longarm"
	}
	return filepath.Join(home, ".longarm")
}

// FileConfig is <data>/longarmd.toml.
type FileConfig struct {
	// Listen URLs, see transport.ParseListen.
	Listen []string `toml:"listen"`
	// MaxLogBytes caps each job's / session's out.log; beyond it the log
	// rotates to out.log.1 (so at most ~2x on disk). Default 1 GiB, 0 = off.
	MaxLogBytes *int64          `toml:"max_log_bytes"`
	GC          GCConfig        `toml:"gc"`
	SSH         SSHConfig       `toml:"ssh"`
	Hub         HubConfig       `toml:"hub"`
	Toolchain   ToolchainConfig `toml:"toolchain"`
	Update      UpdateConfig    `toml:"update"`
	Isolation   IsolationConfig `toml:"isolation"`
	Forward     ForwardConfig   `toml:"forward"`
	Links       LinksConfig     `toml:"links"`
}

// IsolationConfig is the [isolation] section (multi-tenancy,
// docs/security.md).
type IsolationConfig struct {
	// Mode: "shared" (default; every workload runs as longarmd's account) or
	// "per-user" (longarmd runs as LocalSystem / root and runs each user's
	// jobs, sessions, shells, file access and git as a local account
	// la-<user> of its own; Windows and Linux).
	Mode string `toml:"mode"`
	// UsersDir holds the users' data (default <data>/users). Unix: every
	// parent must be traversable (o+x) for the accounts.
	UsersDir string `toml:"users_dir"`
	// AccountPrefix of the users' OS accounts (default "la-").
	AccountPrefix string `toml:"account_prefix"`
	// CgroupRoot (Linux) is the parent of the jobs' cgroups (default
	// /sys/fs/cgroup/longarm; "off" = process groups only).
	CgroupRoot string `toml:"cgroup_root"`
}

// ForwardConfig is the [forward] section: what port forwards may reach
// (Forward.Open from longarm forward / fleet_forward, the built-in sshd's
// direct-tcpip and incoming port links).
type ForwardConfig struct {
	// Targets: "loopback" (default: this worker's own 127.0.0.1 / ::1 ports)
	// or "any" (any host:port the worker can reach - its LAN, other workers;
	// before this setting existed that was the behaviour in shared isolation).
	Targets string `toml:"targets"`
}

// LinksConfig is the [links] section (port links through this worker).
type LinksConfig struct {
	// MaxConns caps the open connections of each link, as its source and as
	// its target (default DefaultLinkMaxConns; -1 = no cap).
	MaxConns int `toml:"max_conns"`
}

// DefaultLinkMaxConns is [links] max_conns by default.
const DefaultLinkMaxConns = 64

// HubConfig is the [hub] section. The address usually comes from hub.toml,
// written by `longarmd join`.
type HubConfig struct {
	Address  string `toml:"address"`
	Disabled bool   `toml:"disabled"`
	// Labels reported to the hub (scheduler constraints, e.g. region="eu").
	Labels map[string]string `toml:"labels"`
	// Advertise replaces the endpoints clients learn from the hub (default:
	// the listen URLs, loopback last), e.g. ["tcp://100.64.0.11:7701"].
	Advertise []string `toml:"advertise"`
	// RemoteJobs is what the hub (web panel, longarm admin) may do with this
	// worker's jobs through the hub session: "control" (default: kill,
	// read logs, apply the toolchain now), "all" (also submit command
	// jobs, which makes the hub able to run commands here) or "none".
	RemoteJobs string `toml:"remote_jobs"`
}

// ToolchainConfig is the [toolchain] section.
type ToolchainConfig struct {
	Backend   string   `toml:"backend"`    // system (default) | fake
	FakeState string   `toml:"fake_state"` // fake backend state file
	Interval  Duration `toml:"interval"`   // re-check period (default 30m)
	Disabled  bool     `toml:"disabled"`
}

// GCConfig is the [gc] section: automatic cleanup of job/session dirs,
// their worktrees and staging data, and unused mirrors.
type GCConfig struct {
	Disabled bool `toml:"disabled"`
	// Keep terminal jobs/sessions this long after they ended (default 7 days).
	Keep Duration `toml:"keep"`
	// When less than this % of the data dir's disk is free, also remove the
	// oldest terminal jobs (default 10; negative disables).
	MinFreePercent *float64 `toml:"min_free_percent"`
	// How often to run (default 1h).
	Interval Duration `toml:"interval"`
}

// SSHConfig is the [ssh] section of the built-in sshd. It is always reachable
// through `longarm proxy` (mTLS); Listen adds direct listeners that authenticate
// with public keys from <data>/authorized_keys.
type SSHConfig struct {
	Disabled bool     `toml:"disabled"`
	Listen   []string `toml:"listen"` // e.g. ["tcp://100.x.y.z:2222"]
	// Shell runs exec requests and pty-less shells, as argv prefix to which
	// the command line is appended (default: cmd.exe /c on Windows, like
	// OpenSSH for Windows; $SHELL -c elsewhere).
	Shell []string `toml:"shell"`
}

// Duration is a TOML string like "168h" or "30m".
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	d.Duration = v
	return err
}

// Defaults.
const (
	DefaultMaxLogBytes = 1 << 30
	DefaultGCKeep      = 7 * 24 * time.Hour
	DefaultGCMinFree   = 10.0
	DefaultGCInterval  = time.Hour
)

// Config is the effective configuration.
type Config struct {
	DataDir      string
	Listen       []string
	LogFile      string // "" = stderr only
	MaxLogBytes  int64
	GC           GCConfig // with defaults applied
	SSH          SSHConfig
	Hub          HubConfig // address from hub.toml if unset there
	HubPollToken string    // pending enrollment (longarmd join --no-wait)
	Warnings     []string  // config problems longarmd logs at start (and keeps running)
	Toolchain    ToolchainConfig
	Update       UpdateConfig
	Isolation    IsolationConfig
	Forward      ForwardConfig // Targets normalized
	Links        LinksConfig   // MaxConns: 0 = no cap
	// How longarmd was started, for the restart after a self-update: "service"
	// (the service manager restarts it) or "exec" (longarmd relaunches itself
	// with Args).
	UpdateMode  string
	ServiceName string
	Args        []string
}

// LoadConfig reads longarmd.toml (if present); flags given in override win.
func LoadConfig(dataDir string, listenOverride []string) (Config, error) {
	cfg := Config{DataDir: dataDir}
	var fc FileConfig
	if _, err := toml.DecodeFile(filepath.Join(dataDir, ConfigFile), &fc); err != nil && !errors.Is(err, os.ErrNotExist) {
		return cfg, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	cfg.Listen = fc.Listen
	if len(listenOverride) > 0 {
		cfg.Listen = listenOverride
	}
	if len(cfg.Listen) == 0 {
		cfg.Listen = []string{fmt.Sprintf("tcp://127.0.0.1:%d", transport.DefaultPort)}
	}
	cfg.MaxLogBytes = DefaultMaxLogBytes
	if fc.MaxLogBytes != nil {
		cfg.MaxLogBytes = *fc.MaxLogBytes
	}
	cfg.GC = fc.GC
	if cfg.GC.Keep.Duration <= 0 {
		cfg.GC.Keep.Duration = DefaultGCKeep
	}
	if cfg.GC.MinFreePercent == nil {
		v := DefaultGCMinFree
		cfg.GC.MinFreePercent = &v
	}
	if cfg.GC.Interval.Duration <= 0 {
		cfg.GC.Interval.Duration = DefaultGCInterval
	}
	cfg.SSH = fc.SSH
	cfg.Hub, cfg.Toolchain, cfg.Update = fc.Hub, fc.Toolchain, fc.Update
	cfg.Isolation = fc.Isolation
	switch cfg.Isolation.Mode {
	case "":
		cfg.Isolation.Mode = tenancy.Shared
	case tenancy.Shared, tenancy.PerUser:
	default:
		return cfg, fmt.Errorf("%s: [isolation] mode %q: want %q or %q", ConfigFile, cfg.Isolation.Mode, tenancy.Shared, tenancy.PerUser)
	}
	targets, err := server.ParseForwardTargets(fc.Forward.Targets)
	if err != nil {
		return cfg, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	cfg.Forward.Targets = targets
	switch cfg.Links = fc.Links; {
	case cfg.Links.MaxConns == 0:
		cfg.Links.MaxConns = DefaultLinkMaxConns
	case cfg.Links.MaxConns == -1:
		cfg.Links.MaxConns = 0
	case cfg.Links.MaxConns < 0:
		return cfg, fmt.Errorf("%s: [links] max_conns %d: want a positive number or -1 (no cap)", ConfigFile, fc.Links.MaxConns)
	}
	if cfg.Isolation.UsersDir != "" && !filepath.IsAbs(cfg.Isolation.UsersDir) {
		cfg.Isolation.UsersDir = filepath.Join(dataDir, cfg.Isolation.UsersDir)
	}
	// Invalid labels (an older longarmd.toml: "eu,os=windows", values with
	// spaces) are dropped with a warning instead of keeping longarmd (and longarmd
	// leave, the uninstaller) from starting.
	for k, v := range cfg.Hub.Labels {
		if err := sched.ValidLabel(k, v, false); err != nil {
			delete(cfg.Hub.Labels, k)
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("%s: [hub] labels: %v - ignored (one TOML entry per label: labels = { region = \"eu\", os = \"windows\" })", ConfigFile, err))
		}
	}
	hf, err := hubclient.LoadFile(dataDir)
	if err != nil {
		return cfg, err
	}
	if cfg.Hub.Address == "" {
		cfg.Hub.Address = hf.Address
		cfg.Hub.Disabled = cfg.Hub.Disabled || hf.Disabled
	}
	cfg.HubPollToken = hf.PollToken
	return cfg, nil
}

// Run runs longarmd until ctx is done or an admin calls Shutdown.
func Run(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	var logW io.Writer = os.Stderr
	if cfg.LogFile != "" {
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		// A service has no usable stderr; its write errors must not keep
		// the lines from the file (io.MultiWriter stops at the first error).
		logW = io.MultiWriter(bestEffort{os.Stderr}, f)
	}
	log := slog.New(slog.NewTextHandler(logW, nil))
	slog.SetDefault(log)
	err := run(ctx, cfg, log)
	if err != nil && !errors.Is(err, update.ErrRestart) {
		log.Error("longarmd failed", "err", err) // a service has nowhere else to say it
	}
	return err
}

func run(ctx context.Context, cfg Config, log *slog.Logger) error {
	if err := platform.SetupConsole(); err != nil { // Exec children inherit it
		log.Warn("no UTF-8 console", "err", err)
	}

	unlock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer unlock()
	// Work in the account's home: relative paths of the sshd's sftp subsystem
	// resolve against the process directory (a service starts in System32).
	if home, err := os.UserHomeDir(); err == nil {
		os.Chdir(home)
	}

	tlsDir := filepath.Join(cfg.DataDir, TLSDir)
	if cfg.HubPollToken != "" {
		// `longarmd join --no-wait`: finish the enrollment before serving.
		if err := completeEnrollment(ctx, cfg, log); err != nil {
			return err
		}
	}
	hubFile, _ := hubclient.LoadFile(cfg.DataDir)
	migrateTailnetState(cfg, hubFile.Tailnet)
	reloader, err := pki.NewReloader(tlsDir, log)
	if err != nil {
		return fmt.Errorf("%w (join a hub with `longarmd join <hub> <code>`, or issue one with `longarm admin issue --role worker --name <name> --out %s`)", err, tlsDir)
	}
	bundle := reloader.Bundle()
	if time.Until(bundle.NotAfter) < 7*24*time.Hour {
		log.Warn("worker certificate expires soon", "not_after", bundle.NotAfter)
	}
	revoked, err := pki.LoadRevocationList(filepath.Join(cfg.DataDir, RevokedFile))
	if err != nil {
		log.Warn("revocation list unreadable; starting empty", "err", err)
		revoked, _ = pki.LoadRevocationList("")
	}

	ctx, cancelCause := context.WithCancelCause(ctx)
	cancel := func() { cancelCause(nil) }
	defer cancel()

	perUser := cfg.Isolation.Mode == tenancy.PerUser
	if perUser {
		privateUmask() // longarmd's own files: LocalSystem / root only
	}
	st, err := openState(ctx, cfg, log)
	if err != nil {
		return err
	}
	policy, err := tenancy.LoadPolicy(filepath.Join(cfg.DataDir, tenancy.PolicyFile))
	if err != nil {
		log.Warn("tenancy policy unreadable; starting empty", "err", err)
		policy, _ = tenancy.LoadPolicy("")
	}
	sampler := &metrics.Sampler{Interval: 2 * time.Second, JobsFn: st.spaces.RunningJobs, DiskPath: cfg.DataDir}

	srv := &server.Server{
		Name: bundle.Identity.Name, Version: version.Version, DataDir: cfg.DataDir, StartedAt: time.Now(),
		Spaces: st.spaces, Policy: policy, LogMaxBytes: cfg.MaxLogBytes, Sampler: sampler,
		Audit: server.NewAudit(filepath.Join(cfg.DataDir, AuditFile)), Log: log, Shutdown: cancel,
		Revoked: revoked, Conns: conntrack.New(),
		ForwardTargets: cfg.Forward.Targets,
	}
	exe, _ := os.Executable()
	if runtime.GOOS == "windows" && exe != "" {
		srv.ExecWrapper = []string{exe, "conhost", "--"}
	}
	srv.GC = func(ctx context.Context, req *pb.GcRequest) (*pb.GcResponse, error) {
		o := cfg.gcOptions()
		if req.Keep != nil {
			o.Keep = req.Keep.AsDuration()
		}
		if req.MinFreePercent != 0 {
			o.MinFreePercent = req.MinFreePercent
		}
		o.DryRun = req.DryRun
		res := st.collect(ctx, o)
		return &pb.GcResponse{Removed: res.Removed, FreedBytes: res.Freed}, nil
	}

	var sshSrv *sshd.Server
	if !cfg.SSH.Disabled {
		sc := sshd.Config{
			HostKeyFile:        filepath.Join(cfg.DataDir, SSHHostKeyFile),
			AuthorizedKeysFile: filepath.Join(cfg.DataDir, AuthorizedKeysFile),
			Shell:              cfg.SSH.Shell,
			InteractiveShell:   server.DefaultShell,
			Audit: func(who, what string, fields map[string]any) {
				srv.Audit.LogAs(who, what, fields)
			},
			Log: log.With("component", "sshd"),
		}
		if runtime.GOOS == "windows" && exe != "" {
			sc.ConsoleHost = []string{exe, "conhost"}
		}
		sc.Forward = srv.ForwardTargetCheck
		if perUser {
			sc.Session = sshSession(srv, exe)
		}
		if sshSrv, err = sshd.New(sc); err != nil {
			return fmt.Errorf("sshd: %w", err)
		}
		defer sshSrv.Close()
		srv.SSH, srv.SSHHostKey = sshSrv, sshSrv.PublicKey()
	}

	type closer interface{ Close() error }
	var listeners, sshListeners []net.Listener
	var closers []closer
	var closersMu sync.Mutex
	longnets := map[string]*longnet.Transport{} // by relay address (closersMu): port links dial with them
	var tsLegs atomic.Pointer[tailnetLegs]      // set below; port links dial with its node
	linkDialer := client.NewDialerFor(&client.Config{Dir: filepath.Join(cfg.DataDir, linkDialDir)}, reloader.LiveBundle())
	linkDialer.LegTransport = func(e client.Endpoint) transport.Transport {
		switch e.Transport {
		case "longnet":
			closersMu.Lock()
			defer closersMu.Unlock()
			if lt := longnets[e.Address]; lt != nil {
				return lt
			}
		case "tailscale":
			if tr := tsLegs.Load().dialer(); tr != nil {
				return tr
			}
		}
		return nil // tcp; a relay or tailnet this worker doesn't use itself: a node of the dialer's own
	}
	defer linkDialer.Close()
	links := newLinkManager(ctx, cfg.DataDir, srv.Name, reloader.LiveBundle(), st.spaces, srv.Audit, log.With("component", "links"), linkDialer)
	links.maxConns = cfg.Links.MaxConns
	srv.Links = links
	defer func() {
		closersMu.Lock()
		defer closersMu.Unlock()
		for _, c := range closers {
			c.Close()
		}
	}()
	listen := func(u string) (net.Listener, string, error) {
		tr, err := transport.ParseListenEnv(u, transport.Env{StateDir: filepath.Join(cfg.DataDir, TSNetDir), Bundle: reloader.LiveBundle(), Logger: log})
		if err != nil {
			return nil, "", err
		}
		if c, ok := tr.(closer); ok {
			closersMu.Lock()
			closers = append(closers, c)
			closersMu.Unlock()
		}
		if lt, ok := tr.(*longnet.Transport); ok {
			if pu, err := url.Parse(u); err == nil {
				closersMu.Lock()
				longnets[pu.Host] = lt
				closersMu.Unlock()
			}
		}
		ln, err := tr.Listen(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("listen %s: %w", u, err)
		}
		if a, ok := tr.(transport.Advertiser); ok {
			return ln, a.AdvertiseURL(ln), nil
		}
		return ln, tr.Name() + "://" + ln.Addr().String(), nil
	}
	closeAll := func() {
		for _, l := range append(listeners, sshListeners...) {
			l.Close()
		}
	}
	for _, u := range cfg.Listen {
		if hasTailscaleListen([]string{u}) {
			continue // started in the background (tailnetLegs)
		}
		ln, name, err := listen(u)
		if err != nil {
			closeAll()
			return err
		}
		listeners = append(listeners, ln)
		srv.Listeners = append(srv.Listeners, name)
	}
	if sshSrv != nil && perUser && len(cfg.SSH.Listen) > 0 {
		log.Warn("per-user isolation: [ssh] listen (direct listeners with authorized_keys) is ignored; use longarm ssh (mTLS identities)")
	} else if sshSrv != nil {
		for _, u := range cfg.SSH.Listen {
			ln, name, err := listen(u)
			if err != nil {
				closeAll()
				return fmt.Errorf("ssh %w", err)
			}
			sshListeners = append(sshListeners, ln)
			srv.Listeners = append(srv.Listeners, "ssh+"+name)
		}
	}
	g := srv.GRPC(reloader.TLSConfig())

	// The local control API listens before longarmd.endpoints is written, so that
	// file's existence means "ready" for both channels.
	lln, laddr, lerr := localapi.Listen("longarmd", cfg.DataDir, filepath.Join(cfg.DataDir, LocalAddrFile))
	if lerr != nil {
		log.Warn("local control API unavailable", "err", lerr)
	} else {
		defer os.Remove(filepath.Join(cfg.DataDir, LocalAddrFile))
	}

	endpoints := filepath.Join(cfg.DataDir, EndpointsFile)
	writeEndpoints := func() error {
		return os.WriteFile(endpoints, []byte(strings.Join(srv.ListenerNames(), "\n")+"\n"), 0o600)
	}
	if err := writeEndpoints(); err != nil {
		closeAll()
		if lln != nil {
			lln.Close()
		}
		return err
	}
	defer os.Remove(endpoints)

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); <-ctx.Done(); st.spaces.Wait() }()
	go func() { defer wg.Done(); sampler.Run(ctx) }()
	go func() { defer wg.Done(); reloader.Watch(ctx, TLSReloadInterval) }()
	go func() { defer wg.Done(); st.gcLoop(ctx, cfg) }()

	// Hub session, toolchain reconciler and the local control API.
	agent := newHubAgent(cfg.Hub.Address, &hubAgent{
		dataDir: cfg.DataDir, reloader: reloader, live: reloader.LiveBundle(), spaces: st.spaces, srv: srv, policy: policy,
		sampler: sampler, revoked: revoked, labels: cfg.Hub.Labels, log: log.With("component", "hub"),
		audit: srv.Audit, remoteJobs: cfg.Hub.RemoteJobs, stop: cancel, logMaxBytes: cfg.MaxLogBytes,
		endpoints: func() []string {
			if len(cfg.Hub.Advertise) > 0 {
				return cfg.Hub.Advertise
			}
			return clientEndpoints(srv.ListenerNames())
		},
	})
	wg.Add(1)
	go func() { defer wg.Done(); agent.watchJobs(ctx) }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.Conns.Watch(ctx, revoked.Changed, revoked.Revoked, func(n int) {
			log.Info("closed connections of revoked certificates", "count", n)
		})
	}()
	hubOn := cfg.Hub.Address != "" && !cfg.Hub.Disabled
	legs := &tailnetLegs{ctx: ctx, log: log, dataDir: cfg.DataDir, live: reloader.LiveBundle(), srv: srv,
		serve: func(ln net.Listener, name string) {
			if err := g.Serve(ln); err != nil && ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				log.Debug("serve", "listener", name, "err", err)
			}
		},
		changed: func(endpoints bool) {
			if endpoints {
				writeEndpoints()
			}
			agent.rehello()
		}}
	legs.env = transport.Env{StateDir: filepath.Join(cfg.DataDir, TSNetDir), Bundle: reloader.LiveBundle(), Logger: log}
	if hubOn {
		legs.hubAddr = cfg.Hub.Address
		legs.env.TailnetKey = legs.hubKey
	}
	agent.tailnet = legs
	tsLegs.Store(legs)
	agent.links = links
	links.report = func(r *pb.PortLinkReport) {
		agent.send(&pb.NodeReport{Msg: &pb.NodeReport_PortLinks{PortLinks: r}}, false)
	}
	wg.Add(1)
	go func() { defer wg.Done(); links.run(ctx) }()
	agent.channel = cfg.Update.Channel
	if agent.channel == "" {
		agent.channel = "stable"
	}
	if hubOn && !cfg.Update.Disabled {
		agent.upd = newUpdater(cfg, agent, exe, log, func() { cancelCause(update.ErrRestart) })
		agent.upd.startup(ctx, hubOn)
		wg.Add(1)
		go func() { defer wg.Done(); agent.upd.Run(ctx) }()
	}
	legs.start(cfg.Listen, hubFile.Tailnet)
	wg.Add(1)
	go func() { defer wg.Done(); <-ctx.Done(); legs.wait() }()
	if hubOn && !cfg.Toolchain.Disabled {
		backend, err := toolchain.New(cfg.Toolchain.Backend, cfg.Toolchain.FakeState)
		if sys, ok := backend.(*toolchain.System); ok {
			sys.MachineWide = perUser
		}
		if err != nil {
			log.Warn("toolchain disabled", "err", err)
		} else {
			agent.tc = &toolchain.Reconciler{Backend: backend, Log: log.With("component", "toolchain"),
				Running: st.spaces.RunningJobs, Drain: agent.setToolDrain, Interval: cfg.Toolchain.Interval.Duration,
				Report: func(r *pb.ToolchainReport) {
					agent.send(&pb.NodeReport{Msg: &pb.NodeReport_Toolchain{Toolchain: r}}, false)
					agent.notify(&pb.LocalEvent{Kind: "toolchain", Message: r.State + " " + r.Error})
				}}
			wg.Add(1)
			go func() { defer wg.Done(); agent.tc.Run(ctx) }()
		}
	}
	if hubOn {
		wg.Add(1)
		go func() { defer wg.Done(); agent.Run(ctx) }()
	}
	lw := &localWorker{srv: srv, agent: agent, st: st}
	if lerr == nil {
		log.Info("local control API", "addr", laddr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := localapi.Serve(ctx, lln, lw.grpcServer(), lw.gateway()); err != nil {
				log.Warn("local control API", "err", err)
			}
		}()
	}

	serveErr := make(chan error, len(listeners))
	for _, ln := range listeners {
		go func() { serveErr <- g.Serve(ln) }()
	}
	for _, ln := range sshListeners {
		go func() {
			if err := sshSrv.Serve(ln); err != nil {
				log.Warn("ssh listener", "err", err)
			}
		}()
	}
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}
	if runtime.GOOS == "windows" && platform.RunningAsSystem() && !perUser {
		log.Warn("longarmd runs as LocalSystem: jobs run as SYSTEM with the system profile, not as a user (DESIGN §3.1); reinstall the service with a user account (MSI SERVICE_USER/SERVICE_PASSWORD, longarmd service install --user)")
	}
	log.Info("longarmd started", "name", srv.Name, "version", version.Version, "data", cfg.DataDir,
		"listen", srv.Listeners, "pid", os.Getpid(), "isolation", cfg.Isolation.Mode)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-serveErr:
		log.Error("serve", "err", runErr)
	}
	cancel()
	for _, l := range sshListeners {
		l.Close()
	}
	stopped := make(chan struct{})
	go func() { g.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second): // follow streams never end on their own
		g.Stop()
	}
	wg.Wait()
	if errors.Is(context.Cause(ctx), update.ErrRestart) {
		log.Info("longarmd stopped for a self-update; running jobs and sessions continue in their shims")
		return update.ErrRestart
	}
	log.Info("longarmd stopped; running jobs and sessions continue in their shims")
	return runErr
}

// TLSReloadInterval is how often the TLS bundle files are checked.
var TLSReloadInterval = 5 * time.Second

func lockDataDir(dataDir string) (func(), error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dataDir, LockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := platform.LockFile(lock); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another longarmd is already running on %s (%v)", dataDir, err)
	}
	return func() { lock.Close() }, nil
}

// state is the persistent part of longarmd: the users' spaces (job and
// session supervisors, workspace stores) and the garbage collection over
// them.
type state struct {
	spaces *userspace.Spaces
	log    *slog.Logger
}

func openState(ctx context.Context, cfg Config, log *slog.Logger) (*state, error) {
	shared := &workspace.Store{Root: cfg.DataDir}
	sp, err := userspace.Open(ctx, userspace.Config{
		Mode: cfg.Isolation.Mode, DataDir: cfg.DataDir, UsersDir: cfg.Isolation.UsersDir,
		Prefix: cfg.Isolation.AccountPrefix, CgroupRoot: cfg.Isolation.CgroupRoot, Log: log, Prepare: shared.PrepareJob,
	})
	if err != nil {
		return nil, err
	}
	return &state{spaces: sp, log: log}, nil
}

// collect runs a collection in every space.
func (s *state) collect(ctx context.Context, o gc.Options) gc.Result {
	var res gc.Result
	for _, sp := range s.spaces.All() {
		c := &gc.Collector{DataDir: sp.Root, Jobs: sp.Jobs, Sessions: sp.Sessions, WS: sp.WS, Log: s.log}
		r := c.Run(ctx, o)
		prefix := ""
		if sp.Owner != "" {
			prefix = "users/" + sp.Owner + "/"
		}
		for _, x := range r.Removed {
			res.Removed = append(res.Removed, prefix+x)
		}
		res.Freed += r.Freed
	}
	return res
}

func (c Config) gcOptions() gc.Options {
	return gc.Options{Keep: c.GC.Keep.Duration, MinFreePercent: *c.GC.MinFreePercent}
}

func (s *state) gcLoop(ctx context.Context, cfg Config) {
	if cfg.GC.Disabled {
		return
	}
	first := time.After(time.Minute)
	t := time.NewTicker(cfg.GC.Interval.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first:
		case <-t.C:
		}
		s.collect(ctx, cfg.gcOptions())
	}
}

// GC runs one collection with longarmd stopped (`longarmd gc`). It takes the data dir
// lock, so it refuses to run next to a live longarmd (use `longarm admin gc` then).
func GC(ctx context.Context, cfg Config, o gc.Options) (gc.Result, error) {
	unlock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return gc.Result{}, fmt.Errorf("%w; while longarmd runs use `longarm admin gc <worker>`", err)
	}
	defer unlock()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := openState(ctx, cfg, log)
	if err != nil {
		return gc.Result{}, err
	}
	return st.collect(ctx, o), nil
}

// bestEffort is a writer whose errors are ignored.
type bestEffort struct{ w io.Writer }

func (b bestEffort) Write(p []byte) (int, error) {
	b.w.Write(p)
	return len(p), nil
}
