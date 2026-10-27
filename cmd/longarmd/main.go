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
//	longarmd audit verify [--file PATH]                check the hash chain of audit.log
//	longarmd diag [--upload] [--since 24h] [--include-audit] [--include-job ID]   diagnostic bundle for a bug report
//	longarmd version
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/gc"
	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/server"
	"github.com/lubobill1990/longarm/internal/shim"
	"github.com/lubobill1990/longarm/internal/update"
	"github.com/lubobill1990/longarm/internal/userspace"
	"github.com/lubobill1990/longarm/internal/version"
	"github.com/lubobill1990/longarm/internal/workspace"
)

const defaultServiceName = longarmd.DefaultServiceName

// exitRestart: longarmd exits with this code to be restarted (self-update).
const exitRestart = 75

// startExe is longarmd's own path, resolved before a self-update renames the
// running binary to *.old: on Linux os.Executable reads /proc/self/exe,
// which follows the rename, so resolving it after the update would relaunch
// the old version.
var startExe, _ = os.Executable()

func main() {
	// git's credential helper for a fetch with a forwarded credential: run
	// before anything else (git waits for it)
	if len(os.Args) > 1 && os.Args[1] == workspace.CredHelperArg {
		os.Exit(workspace.CredentialHelperMain(os.Args[2:], os.Stdin, os.Stdout))
	}
	root := &cobra.Command{
		Use:           "longarmd",
		Short:         "Longarm worker daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(localCmds()...)
	root.AddCommand(updateCmd(), updateWatchCmd(), installHelperCmd())
	root.AddCommand(uiCmds()...)
	root.AddCommand(userHelperCmd(), sftpServerCmd(), trustCmd(), diagCmd())
	root.AddCommand(runCmd(), joinCmd(), loginCmd(), shimCmd(), conhostCmd(), serviceCmd(), gcCmd(), auditCmd(), &cobra.Command{
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

// userHelperCmd is the per-user helper (internal): longarmd starts it as a
// user's OS account and talks gRPC over its stdin/stdout.
func userHelperCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:    userspace.HelperCommand + " --root DIR",
		Short:  "Serve file and git operations as the current account on stdin/stdout (internal; per-user isolation)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			return server.ServeUserHelper(root, os.Stdin, os.Stdout)
		},
	}
	c.Flags().StringVar(&root, "root", "", "the user's space")
	return c
}

// sftpServerCmd serves sftp on stdin/stdout (internal; the built-in sshd
// runs it as the caller's account with per-user isolation).
func sftpServerCmd() *cobra.Command {
	return &cobra.Command{
		Use:    longarmd.SFTPCommand,
		Short:  "Serve sftp on stdin/stdout (internal)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			srv, err := sftp.NewServer(stdio{})
			if err != nil {
				return err
			}
			if err := srv.Serve(); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			return nil
		},
	}
}

type stdio struct{}

func (stdio) Read(b []byte) (int, error)  { return os.Stdin.Read(b) }
func (stdio) Write(b []byte) (int, error) { return os.Stdout.Write(b) }
func (stdio) Close() error                { os.Stdin.Close(); return os.Stdout.Close() }

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

	var dataDir, user, passwordEnv, isolation string
	var listen []string
	var dryRun bool
	install := &cobra.Command{
		Use:   "install",
		Short: "Install longarmd as a service running as a user account",
		Long: `Install longarmd as a system service that starts at boot and restarts on failure.

Windows: SCM service logging on as --user (".\name" or "DOMAIN\name"); the
password is read from the environment variable named by --password-env
(default LONGARM_SERVICE_PASSWORD). The account is granted "Log on as a service".
Linux: systemd unit with User= and KillMode=process (shims survive restarts).
macOS: LaunchDaemon with UserName and AbandonProcessGroup.

Data dir without --data-dir / LONGARM_DATA_DIR: --isolation per-user uses the
system data dir (Linux /var/lib/longarmd, Windows C:\ProgramData\Longarm\longarmd)
and copies an enrollment from the default data dir / ~/.longarm into it when it
has none yet; --user NAME as root (Linux, macOS) uses NAME's ~/.longarm;
otherwise the default data dir (root on Linux: /var/lib/longarmd, else ~/.longarm).
Per-user isolation refuses a data dir whose users dir the accounts cannot reach
(e.g. below /root).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			return longarmd.InstallService(longarmd.ServiceInstallOptions{Name: name, Exe: exe, DataDir: dataDir,
				DataDirExplicit: cmd.Flags().Changed("data-dir") || os.Getenv(longarmd.DataDirEnv) != "",
				User:            user, Password: os.Getenv(passwordEnv), Isolation: isolation, Listen: listen, DryRun: dryRun,
				Out: os.Stdout, Warn: os.Stderr})
		},
	}
	install.Flags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory for the service (default: see above); env LONGARM_DATA_DIR")
	install.Flags().BoolVar(&dryRun, "dry-run", false, "check and print the data dir and service, change nothing")
	install.Flags().StringArrayVar(&listen, "listen", nil, "listen URL(s) (default: longarmd.toml in the data dir)")
	install.Flags().StringVar(&user, "user", "", "account to run as")
	install.Flags().StringVar(&passwordEnv, "password-env", "LONGARM_SERVICE_PASSWORD", "env var holding the Windows account password")
	install.Flags().StringVar(&isolation, "isolation", "", "per-user: run as LocalSystem / root and isolate users (writes [isolation] to longarmd.toml; docs/security.md)")

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
			cleanup := perUserLoopbackCleanup(n) // read the unit before it goes
			err := platform.UninstallService(n)
			if err == nil {
				serviceFirewall(n, "", false)
				if cleanup != nil {
					cleanup()
				}
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
