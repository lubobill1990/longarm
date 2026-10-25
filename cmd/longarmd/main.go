// Command longarmd is the Longarm worker daemon.
//
//	longarmd run [--data-dir D] [--listen URL]...     foreground (or under the service manager)
//	longarmd shim <dir>                               per-job / per-session supervisor (spawned by longarmd)
//	longarmd conhost [--pty] [--raw] -- cmd...         UTF-8 console helper (Windows, internal)
//	longarmd service install|uninstall|start|stop|status
//	longarmd gc [--keep D] [--min-free P] [--dry-run]     offline cleanup (longarmd stopped)
//	longarmd join <hub> <code> [--name N] [--ca-fingerprint FP] [--no-wait]   enroll with longarm-hub
//	longarmd status | drain [--reason R] | undrain | toolchain status|apply | events   (local control API)
//	longarmd leave [--kill-jobs] [--purge]                deregister from the hub, remove credentials
//	longarmd update status                            last self-update
//	longarmd version
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/gc"
	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/shim"
	"github.com/lubobill1990/longarm/internal/update"
	"github.com/lubobill1990/longarm/internal/version"
)

const defaultServiceName = "longarmd"

// exitRestart: longarmd exits with this code to be restarted (self-update).
const exitRestart = 75

// startExe is longarmd's own path, resolved before a self-update renames the
// running binary to *.old: on Linux os.Executable reads /proc/self/exe,
// which follows the rename, so resolving it after the update would relaunch
// the old version.
var startExe, _ = os.Executable()

func main() {
	root := &cobra.Command{
		Use:           "longarmd",
		Short:         "Longarm worker daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(localCmds()...)
	root.AddCommand(updateCmd(), updateWatchCmd(), installHelperCmd())
	root.AddCommand(uiCmds()...)
	root.AddCommand(runCmd(), joinCmd(), shimCmd(), conhostCmd(), serviceCmd(), gcCmd(), &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run:   func(*cobra.Command, []string) { fmt.Println(version.Version) },
	})
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "longarmd:", err)
		os.Exit(1)
	}
}

func runCmd() *cobra.Command {
	var dataDir, logFile, serviceName string
	var listen []string
	c := &cobra.Command{
		Use:   "run",
		Short: "Run the daemon in the foreground (also the service entry point)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, err := filepath.Abs(dataDir)
			if err != nil {
				return err
			}
			cfg, err := longarmd.LoadConfig(dataDir, listen)
			if err != nil {
				return err
			}
			cfg.LogFile = logFile
			cfg.ServiceName, cfg.Args = serviceName, os.Args[1:]
			cfg.UpdateMode = "exec"
			if os.Getppid() == 1 && (os.Getenv("INVOCATION_ID") != "" || os.Getenv("XPC_SERVICE_NAME") != "") { // systemd / launchd
				cfg.UpdateMode = "service"
			}
			run := func(ctx context.Context) error { return longarmd.Run(ctx, cfg) }
			// Under the Windows SCM the service handler drives the lifetime.
			svcCfg := cfg
			if svcCfg.LogFile == "" {
				svcCfg.LogFile = filepath.Join(dataDir, longarmd.LogFileName)
			}
			svcCfg.UpdateMode = "service"
			handled, err := platform.RunAsService(serviceName, func(ctx context.Context) error {
				err := longarmd.Run(ctx, svcCfg)
				if errors.Is(err, update.ErrRestart) {
					// Exit without reporting "stopped": the SCM's recovery
					// actions start the service again - with the new binary.
					os.Exit(exitRestart)
				}
				return err
			})
			if handled {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			err = run(ctx)
			if errors.Is(err, update.ErrRestart) {
				if cfg.UpdateMode == "service" {
					fmt.Fprintln(os.Stderr, "longarmd: exiting for a self-update; the service manager restarts it")
					os.Exit(exitRestart)
				}
				if err := update.Relaunch(startExe, os.Args[1:]); err != nil {
					return fmt.Errorf("relaunch after self-update: %w", err)
				}
				return nil
			}
			return err
		},
	}
	c.Flags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory (tls/, jobs/, longarmd.toml); env LONGARM_DATA_DIR")
	c.Flags().StringArrayVar(&listen, "listen", nil, "listen URL, repeatable: tcp://ADDR:PORT | tailscale://HOSTNAME[:PORT][?control=URL] | longnet://RELAY:PORT[?name=NAME] (default from longarmd.toml, else tcp://127.0.0.1:7701)")
	c.Flags().StringVar(&logFile, "log-file", "", "also write logs to this file (service mode default: <data-dir>/longarmd.log)")
	c.Flags().StringVar(&serviceName, "service-name", defaultServiceName, "service name when started by the service manager")
	return c
}

func shimCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "shim <jobdir>",
		Short:  "Per-job supervisor (internal; spawned by longarmd)",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		Run: func(_ *cobra.Command, args []string) {
			os.Exit(shim.Main(args[0], os.Stdin))
		},
	}
}

func conhostCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "conhost [--pty] -- command [args...]",
		Short:              "Run a command with a private hidden UTF-8 console (internal; used by Exec on Windows)",
		Hidden:             true,
		DisableFlagParsing: true,
		Run: func(_ *cobra.Command, args []string) {
			os.Exit(shim.ConsoleHost(args))
		},
	}
}

func serviceCmd() *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "service",
		Short: "Manage the longarmd system service (SCM / systemd / launchd)",
	}
	c.PersistentFlags().StringVar(&name, "name", defaultServiceName, "service name")

	var dataDir, user, passwordEnv string
	var listen []string
	install := &cobra.Command{
		Use:   "install",
		Short: "Install longarmd as a service running as a user account",
		Long: `Install longarmd as a system service that starts at boot and restarts on failure.

Windows: SCM service logging on as --user (".\name" or "DOMAIN\name"); the
password is read from the environment variable named by --password-env
(default LONGARM_SERVICE_PASSWORD). The account is granted "Log on as a service".
Linux: systemd unit with User= and KillMode=process (shims survive restarts).
macOS: LaunchDaemon with UserName and AbandonProcessGroup.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			dataDir, err := filepath.Abs(dataDir)
			if err != nil {
				return err
			}
			args := []string{"run", "--data-dir", dataDir, "--service-name", name}
			for _, l := range listen {
				args = append(args, "--listen", l)
			}
			cfg := platform.ServiceConfig{
				Name: name, DisplayName: "Longarm worker (" + name + ")",
				Description: "Longarm worker daemon: runs remote jobs for longarm clients",
				Exe:         exe, Args: args, User: user, Password: os.Getenv(passwordEnv),
			}
			if user == "" {
				fmt.Fprintln(os.Stderr, "warning: no --user given; the service runs as LocalSystem/root, which DESIGN §3.1 advises against")
			}
			if err := platform.InstallService(cfg); err != nil {
				return err
			}
			fmt.Printf("installed service %q (%s %v)\n", name, exe, args)
			serviceFirewall(name, exe, true)
			return nil
		},
	}
	install.Flags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory for the service")
	install.Flags().StringArrayVar(&listen, "listen", nil, "listen URL(s) (default: longarmd.toml in the data dir)")
	install.Flags().StringVar(&user, "user", "", "account to run as")
	install.Flags().StringVar(&passwordEnv, "password-env", "LONGARM_SERVICE_PASSWORD", "env var holding the Windows account password")

	simple := func(use, short string, fn func(string) error, done string) *cobra.Command {
		return &cobra.Command{
			Use: use, Short: short, Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				if err := fn(name); err != nil {
					return err
				}
				fmt.Printf("service %q %s\n", name, done)
				return nil
			},
		}
	}
	status := &cobra.Command{
		Use: "status", Short: "Show service status", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			st, err := platform.QueryService(name)
			if err != nil {
				return err
			}
			fmt.Println(st)
			if st == platform.ServiceNotInstalled {
				return errors.New("not installed")
			}
			return nil
		},
	}
	c.AddCommand(install,
		simple("uninstall", "Stop and remove the service", func(n string) error {
			err := platform.UninstallService(n)
			if err == nil {
				serviceFirewall(n, "", false)
			}
			return err
		}, "uninstalled"),
		simple("start", "Start the service", platform.StartService, "started"),
		simple("stop", "Stop the service (running jobs keep running)", platform.StopService, "stopped"),
		status)
	return c
}

func gcCmd() *cobra.Command {
	var dataDir string
	var keep time.Duration
	var minFree float64
	var dryRun bool
	c := &cobra.Command{
		Use:   "gc",
		Short: "Remove old job/session dirs, worktrees, staging data and unused mirrors (longarmd must be stopped; else use longarm admin gc)",
		Long: `Remove terminal jobs and sessions that ended longer ago than --keep (with their
git worktrees), then - while less than --min-free percent of the disk is free -
the oldest remaining terminal ones, plus stale staging uploads, orphaned
worktrees and long-unused mirrors. Running jobs and sessions are never touched.
Defaults come from the [gc] section of longarmd.toml. longarmd also runs this
automatically every [gc] interval (default 1h).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, err := filepath.Abs(dataDir)
			if err != nil {
				return err
			}
			cfg, err := longarmd.LoadConfig(dataDir, nil)
			if err != nil {
				return err
			}
			o := gc.Options{Keep: cfg.GC.Keep.Duration, MinFreePercent: *cfg.GC.MinFreePercent, DryRun: dryRun}
			if cmd.Flags().Changed("keep") {
				o.Keep = keep
			}
			if cmd.Flags().Changed("min-free") {
				o.MinFreePercent = minFree
			}
			res, err := longarmd.GC(cmd.Context(), cfg, o)
			if err != nil {
				return err
			}
			for _, r := range res.Removed {
				fmt.Println(r)
			}
			verb := "freed"
			if dryRun {
				verb = "would free"
			}
			fmt.Printf("%d removed, %s %.1f MiB\n", len(res.Removed), verb, float64(res.Freed)/(1<<20))
			return nil
		},
	}
	c.Flags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory")
	c.Flags().DurationVar(&keep, "keep", longarmd.DefaultGCKeep, "keep terminal jobs/sessions that ended within this long")
	c.Flags().Float64Var(&minFree, "min-free", longarmd.DefaultGCMinFree, "remove more (oldest first) while less than this % of the disk is free; <= 0 disables")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "only print what would be removed")
	return c
}
