package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
)

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
	// stdout: the values once, in a form the deploy scripts can grep from
	// the container log (they prefer bootstrap.env on the share)
	fmt.Println(res.LogLine())
	switch {
	case res.AdminJoined:
		fmt.Fprintf(os.Stderr, "longarm-hub bootstrap: CA %s; an admin has joined (new codes: longarm admin join-code)\n", res.Fingerprint)
	default:
		addr := res.HubAddress
		if addr == "" {
			addr = "<hub>:7443"
		}
		fmt.Fprintf(os.Stderr, "longarm-hub bootstrap: admin join code valid until %s:\n  longarm join %s %s --ca-fingerprint %s\n",
			res.AdminCodeExpires.UTC().Format(time.RFC3339), addr, res.AdminCode, res.Fingerprint)
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
Without either variable this is plain 'serve' (docker compose setups).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runContainer()
		},
	}
}

func runContainer() error {
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
	lsConfig := os.Getenv(envLitestreamConfig)
	var ls string
	if lsConfig != "" {
		if ls, err = exec.LookPath(cmp.Or(os.Getenv(envLitestreamBin), "litestream")); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "longarm-hub container: restoring %s from the replica (if it has one)\n", db)
		rc := exec.CommandContext(context.Background(), ls, "restore", "-config", lsConfig, "-if-db-not-exists", "-if-replica-exists", db)
		rc.Stdout, rc.Stderr = os.Stderr, os.Stderr
		if err := rc.Run(); err != nil {
			return fmt.Errorf("litestream restore: %w (not starting: the replica may hold the hub's data)", err)
		}
	}
	if os.Getenv(envBootstrap) == "1" {
		ttl := 24 * time.Hour
		if v := os.Getenv(envBootstrapTTL); v != "" {
			if ttl, err = time.ParseDuration(v); err != nil {
				return fmt.Errorf("%s: %w", envBootstrapTTL, err)
			}
		}
		if err := runBootstrap(ttl); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}
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
