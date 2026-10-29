package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/hub"
)

// Container entrypoint environment (deploy/azure, the image's CMD).
const (
	// envLitestreamConfig names a Litestream config replicating the database
	// (deploy/hub/litestream-abs.yml in the image); unset = no Litestream.
	envLitestreamConfig = "LONGARM_HUB_LITESTREAM_CONFIG"
	// envLitestreamBin is the litestream binary (default: litestream on PATH).
	envLitestreamBin = "LONGARM_HUB_LITESTREAM_BIN"
	// envBootstrap=1 runs `bootstrap` before serving.
	envBootstrap = "LONGARM_HUB_BOOTSTRAP"
	// envBootstrapTTL is the admin join code's lifetime (default 24h).
	envBootstrapTTL = "LONGARM_HUB_BOOTSTRAP_TTL"
	// envStartupTimeout bounds each start phase (restore, bootstrap, the
	// hub's open) for the liveness probe (default 60m): a phase running
	// longer stops refreshing the startup marker, so the probe fails and the
	// container is restarted (hub.StartupMarker).
	envStartupTimeout = "LONGARM_HUB_STARTUP_TIMEOUT"
)

// startupTimeout is $LONGARM_HUB_STARTUP_TIMEOUT, or 60m.
func startupTimeout() (time.Duration, error) {
	v := os.Getenv(envStartupTimeout)
	if v == "" {
		return time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: a positive duration like 60m, not %q", envStartupTimeout, v)
	}
	return d, nil
}

// restoreProgressEvery: how often a running restore logs that it still runs.
var restoreProgressEvery = time.Minute

// runRestore runs `litestream restore` of db with the startup marker in
// phase (refreshed until the next phase begins), with a progress line in the log every restoreProgressEvery
// (deploy.sh reads the lines: a hub still restoring is not a failed deploy).
func runRestore(ctx context.Context, mk *hub.StartupMarker, phase string, limit time.Duration, ls, lsConfig, db string) error {
	if mk != nil {
		if err := mk.Begin(phase, limit); err != nil {
			fmt.Fprintf(os.Stderr, "longarm-hub container: startup marker: %v\n", err)
		}
	}
	fmt.Fprintf(os.Stderr, "longarm-hub container: restoring %s from the replica (if it has one)\n", db)
	rc := exec.CommandContext(ctx, ls, "restore", "-config", lsConfig, "-if-db-not-exists", "-if-replica-exists", db)
	rc.Stdout, rc.Stderr = os.Stderr, os.Stderr
	t := time.Now()
	if err := rc.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- rc.Wait() }()
	tick := time.NewTicker(restoreProgressEvery)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				fmt.Fprintf(os.Stderr, "longarm-hub container: restore of %s failed after %s\n", db, time.Since(t).Round(time.Second))
				return err
			}
			fmt.Fprintf(os.Stderr, "longarm-hub container: restore took %s (%s)\n", time.Since(t).Round(time.Millisecond), db)
			return nil
		case <-tick.C:
			fmt.Fprintf(os.Stderr, "longarm-hub container: restore of %s still running after %s\n", db, time.Since(t).Round(time.Second))
		}
	}
}

func bootstrapCmd() *cobra.Command {
	var ttl time.Duration
	c := &cobra.Command{
		Use:   "bootstrap",
		Short: "First start without operator steps: CA, hub.toml template, a one-time admin join code (idempotent)",
		Long: `Prepare the hub for unattended starts (containers, deploy/azure):
  - writes a hub.toml template when --config / LONGARM_HUB_CONFIG names a missing file
  - creates the CA when the data dir has none
  - as long as no admin has joined: keeps one valid admin join code (a new
    one when the last one expired or was lost)
and prints the CA fingerprint and the code; the same values go to
<data_dir>/bootstrap.env. Runs on every start of the container entrypoint.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBootstrap(ttl)
		},
	}
	c.Flags().DurationVar(&ttl, "admin-code-ttl", 24*time.Hour, "lifetime of a new admin join code")
	return c
}

func runBootstrap(ttl time.Duration) error {
	if configPath != "" {
		wrote, err := hub.WriteConfigTemplate(configPath)
		if err != nil {
			return err
		}
		if wrote {
			fmt.Fprintf(os.Stderr, "longarm-hub bootstrap: wrote %s\n", configPath)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	res, err := hub.Bootstrap(cfg, ttl, log)
	if err != nil {
		return err
	}
	// stdout: one greppable line without the code (container logs are
	// readable by more people than the data dir)
	fmt.Println(res.LogLine())
	if res.AdminJoined {
		fmt.Fprintf(os.Stderr, "longarm-hub bootstrap: CA %s; an admin has joined (new codes: longarm admin join-code)\n", res.Fingerprint)
	} else {
		fmt.Fprintf(os.Stderr, "longarm-hub bootstrap: CA %s; the one-time admin join code (valid until %s) is in %s\n",
			res.Fingerprint, res.AdminCodeExpires.UTC().Format(time.RFC3339), filepath.Join(cfg.DataDir, hub.BootstrapFile))
	}
	return nil
}

func containerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "container",
		Short: "Container entrypoint: restore the database (Litestream), bootstrap, serve under litestream replicate",
		Long: `The image's default command. In order:
  1. with $LONGARM_HUB_LITESTREAM_CONFIG: litestream restore of the database
     (only when it is missing locally and a replica exists; any error stops
     here, so a hub never starts empty next to a replica it could not read)
  2. with $LONGARM_HUB_BOOTSTRAP=1: longarm-hub bootstrap
  3. with Litestream: exec litestream replicate -exec "longarm-hub serve"
     (Litestream is PID 1: on SIGTERM it stops the hub, then ships the last
     WAL frames), else exec longarm-hub serve.
Without either variable this is plain 'serve' (docker compose setups).
Until the hub listens, <database dir>/` + hub.StartupMarkerFile + ` is refreshed every
10 s while a step runs (each step at most $LONGARM_HUB_STARTUP_TIMEOUT,
default 60m): 'healthcheck --live' passes on it, so a liveness probe does
not kill a long restore but does restart a dead or hung one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runContainer()
		},
	}
}

func runContainer() error {
	// the hub logs its startup timing from here (internal/hub restart.go)
	start := time.Now()
	os.Setenv(hub.EnvContainerStart, strconv.FormatInt(start.UnixMilli(), 10))
	if os.Getenv(envBootstrap) == "1" && configPath != "" {
		// hub.toml doesn't exist before the first bootstrap
		if _, err := hub.WriteConfigTemplate(configPath); err != nil {
			return err
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	db := cfg.DatabasePath()
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		return err
	}
	if dir := cfg.Tailnet.Headscale.SidecarDir; dir != "" {
		// the Litestream config names it (both restores and replicate)
		os.Setenv(envHeadscaleDB, hub.HeadscaleDB(dir))
	}
	maxPhase, err := startupTimeout()
	if err != nil {
		return err
	}
	// the liveness probe (healthcheck --live) passes while the marker is
	// fresh: restore, bootstrap and the hub's open can take minutes
	mk := hub.NewStartupMarker(hub.StartupMarkerPath(cfg), 0)
	os.Setenv(hub.EnvStartupMarker, mk.Path())
	if err := mk.Begin("start", maxPhase); err != nil {
		fmt.Fprintf(os.Stderr, "longarm-hub container: startup marker: %v\n", err)
	}
	lsConfig := os.Getenv(envLitestreamConfig)
	var ls string
	if lsConfig != "" {
		if ls, err = exec.LookPath(cmp.Or(os.Getenv(envLitestreamBin), "litestream")); err != nil {
			return err
		}
		if err := runRestore(context.Background(), mk, "restore", maxPhase, ls, lsConfig, db); err != nil {
			return fmt.Errorf("litestream restore: %w (not starting: the replica may hold the hub's data)", err)
		}
	}
	if dir := cfg.Tailnet.Headscale.SidecarDir; dir != "" {
		prepareHeadscaleDB(dir, ls, lsConfig, mk, maxPhase)
	}
	if os.Getenv(envBootstrap) == "1" {
		ttl := 24 * time.Hour
		if v := os.Getenv(envBootstrapTTL); v != "" {
			if ttl, err = time.ParseDuration(v); err != nil {
				return fmt.Errorf("%s: %w", envBootstrapTTL, err)
			}
		}
		if err := mk.Begin("bootstrap", maxPhase); err != nil {
			fmt.Fprintf(os.Stderr, "longarm-hub container: startup marker: %v\n", err)
		}
		t := time.Now()
		if err := runBootstrap(ttl); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
		fmt.Fprintf(os.Stderr, "longarm-hub container: bootstrap took %s\n", time.Since(t).Round(time.Millisecond))
	}
	// the hand-over: a fresh marker, no longer refreshed by this process,
	// for Litestream's start; the hub refreshes it while it opens the
	// database and removes it once it listens (serve)
	if err := mk.Begin("serve", maxPhase); err == nil {
		mk.End()
	}
	fmt.Fprintf(os.Stderr, "longarm-hub container: starting the hub %s after the entrypoint started\n", time.Since(start).Round(time.Millisecond))
	serve := []string{self}
	if configPath != "" {
		serve = append(serve, "--config", configPath)
	}
	if dataDir != "" {
		serve = append(serve, "--data-dir", dataDir)
	}
	serve = append(serve, "serve")
	if lsConfig == "" {
		return execve(self, serve)
	}
	return execve(ls, []string{ls, "replicate", "-config", lsConfig, "-exec", shellJoin(serve)})
}

// envHeadscaleDB is the sidecar's database in the Litestream config
// (deploy/hub/litestream-abs-headscale.yml).
const envHeadscaleDB = "LONGARM_HUB_HEADSCALE_DB"

// prepareHeadscaleDB readies the database of a Headscale sidecar
// ([tailnet.headscale] sidecar_dir) before Litestream replicates it and the
// hub lets Headscale start (internal/hub/headscale_sidecar.go): restored
// from its replica when the shared directory has none (a new container
// group), else an empty file owned by the hub's user, so Litestream (this
// user) can write the WAL files Headscale (root, in its own container)
// creates - SQLite gives them the database file's owner. A failure keeps
// Headscale stopped (hub.EnvHeadscaleRestore), not the hub.
func prepareHeadscaleDB(dir, ls, lsConfig string, mk *hub.StartupMarker, maxPhase time.Duration) {
	fail := func(f string, a ...any) {
		msg := fmt.Sprintf(f, a...)
		fmt.Fprintf(os.Stderr, "longarm-hub container: headscale: %s\n", msg)
		os.Setenv(hub.EnvHeadscaleRestore, msg)
	}
	if err := mk.Begin("headscale", maxPhase); err != nil {
		fmt.Fprintf(os.Stderr, "longarm-hub container: startup marker: %v\n", err)
	}
	// the sidecar's start script hands the directory (an emptyDir volume,
	// root's) to this user
	deadline := time.Now().Add(3 * time.Minute)
	for {
		f, err := os.CreateTemp(dir, ".probe-*")
		if err == nil {
			f.Close()
			os.Remove(f.Name())
			break
		}
		if time.Now().After(deadline) {
			fail("%s is not writable (is the headscale container running?): %v", dir, err)
			return
		}
		time.Sleep(time.Second)
	}
	db := hub.HeadscaleDB(dir)
	if lsConfig != "" {
		if err := runRestore(context.Background(), mk, "restore-headscale", maxPhase, ls, lsConfig, db); err != nil {
			fail("litestream restore of %s: %v (Headscale stays stopped: the replica may hold its nodes)", db, err)
			return
		}
	}
	if _, err := os.Stat(db); errors.Is(err, os.ErrNotExist) {
		f, err := os.OpenFile(db, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			fail("create %s: %v", db, err)
			return
		}
		f.Close()
	}
}

// shellJoin quotes args for Litestream's -exec (parsed with shellwords).
func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"\\$`;&|<>(){}*?[]#~") {
			q[i] = a
			continue
		}
		q[i] = "'" + strings.ReplaceAll(a, "'", `'"'"'`) + "'"
	}
	return strings.Join(q, " ")
}

// Argument-free names of longarm-hub (symlinks in the image) for
// `az container exec`, which runs a single program without arguments
// (deploy/azure README "Operations").
var execAliases = map[string][]string{
	"longarm-hub-admin-code": {"join-code", "--role", "admin", "--ttl", "24h"},
	"longarm-hub-user-add":   {"user", "add"}, // + the name, asked for
	"longarm-hub-users":      {"user", "ls"},
	// the release certificate of CI (docs/ci-cd.md), as the
	// LONGARM_RELEASE_TLS secret wants it: valid 180 days, name ci-<date>
	"longarm-hub-release-cert": {"issue", "--role", "release", "--days", "180", "--b64"},
	// the certificate of the external relay deploy/azure runs next to the
	// hub (deploy.sh --relay): valid a year (the relay renews it through the
	// hub), name and host from the hub's advertised name (relayIdentity)
	"longarm-hub-relay-cert": {"issue", "--role", "relay", "--days", "365", "--b64"},
}

// relayIdentity is the external relay's certificate name and DNS name for
// a hub advertised as fqdn: <label>-relay and <label>-relay.<rest> (the DNS
// label deploy/azure gives the relay's container group). Without an fqdn:
// "relay" and no name.
func relayIdentity(fqdn string) (name, host string) {
	if h, _, err := net.SplitHostPort(fqdn); err == nil {
		fqdn = h
	}
	label, rest, _ := strings.Cut(fqdn, ".")
	if label == "" || net.ParseIP(fqdn) != nil {
		return "relay", ""
	}
	name = label + "-relay"
	if rest == "" {
		return name, name
	}
	return name, name + "." + rest
}

// aliasArgs maps argv[0] to subcommand arguments (nil: not an alias).
func aliasArgs(argv0 string) ([]string, error) {
	name := strings.TrimSuffix(filepath.Base(argv0), ".exe")
	args, ok := execAliases[name]
	if !ok {
		return nil, nil
	}
	args = append([]string(nil), args...)
	if name == "longarm-hub-user-add" {
		fmt.Fprint(os.Stderr, "panel user name: ")
		var user string
		if _, err := fmt.Fscanln(os.Stdin, &user); err != nil || user == "" {
			return nil, fmt.Errorf("no user name: %v", err)
		}
		args = append(args, user)
	}
	if name == "longarm-hub-relay-cert" {
		rn, host := relayIdentity(os.Getenv("LONGARM_HUB_ADVERTISE"))
		args = append(args, "--name", rn)
		if host != "" {
			args = append(args, "--host", host)
		}
	}
	return args, nil
}
