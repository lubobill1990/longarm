package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/longarmd"
)

// sessionModeCmd manages the user-session worker (Windows): longarmd as the
// signed-in user, started at logon by a scheduled task - no service, no
// password (internal/longarmd/sessionmode.go).
func sessionModeCmd() *cobra.Command {
	var dataDir, sid, host string
	c := &cobra.Command{
		Use:   "session-mode",
		Short: "The user-session worker (Windows): longarmd runs as you, started at your logon (no service, no password)",
		Long: `The user-session worker (Windows; the MSI's default, RUN=session): longarmd
runs as the signed-in user, started at that user's logon by a scheduled task
(LongarmWorker-<your SID>) - no Windows service and no account password, so it
works on machines you sign in to with Windows Hello or a security key only.

The task starts a host (longarm-tray --worker-host, which has no window, or
longarmd session-mode run) that runs longarmd and restarts it when it exits;
the task itself brings a host back that died (every 5 minutes). Jobs and
sessions keep running while a remote desktop connection is disconnected, and
stop at logoff or reboot until you log on again. A worker that must run with
nobody logged on is a service: longarmd service install.

The data dir is yours (default ~\.longarm): sign in with the tray or
longarmd login, no elevation needed.`,
	}
	c.PersistentFlags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), `data directory (default ~\.longarm); env LONGARM_DATA_DIR`)

	run := &cobra.Command{
		Use:   "run",
		Short: "Host longarmd: run it and restart it when it exits (the scheduled task's action)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := filepath.Abs(dataDir)
			if err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			return runSessionHost(dir, exe)
		},
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "Register the scheduled task for you (or --user-sid) and start it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "windows" {
				return errors.New("user-session mode is Windows-only (Linux / macOS: longarmd service install --user you)")
			}
			if sid == "" {
				var err error
				if sid, err = longarmd.CurrentUserSID(); err != nil {
					return err
				}
			}
			exe, args, err := sessionHostCommand(host)
			if err != nil {
				return err
			}
			dir, err := filepath.Abs(dataDir)
			if err != nil {
				return err
			}
			return longarmd.InstallSessionTask(longarmd.SessionInstallOptions{UserSID: sid, HostExe: exe, HostArgs: args,
				Start: true, DataDir: dir, Sched: longarmd.Schtasks{}, Out: os.Stdout})
		},
	}
	install.Flags().StringVar(&sid, "user-sid", "", "the user whose logon starts the worker (default: you)")
	install.Flags().StringVar(&host, "host", "", "the host the task runs (default: longarm-tray.exe --worker-host next to longarmd, else longarmd session-mode run)")
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the scheduled task and stop the worker (running jobs keep running)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if sid == "" {
				var err error
				if sid, err = longarmd.CurrentUserSID(); err != nil {
					return err
				}
			}
			dir, err := filepath.Abs(dataDir)
			if err != nil {
				return err
			}
			return longarmd.RemoveSessionTask(longarmd.Schtasks{}, sid, dir, os.Stdout)
		},
	}
	uninstall.Flags().StringVar(&sid, "user-sid", "", "whose task (default: yours)")
	task := func(use, short string, fn func(longarmd.Schtasks, string) error, done string) *cobra.Command {
		return &cobra.Command{
			Use: use, Short: short, Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				me, err := longarmd.CurrentUserSID()
				if err != nil {
					return err
				}
				name := longarmd.SessionTaskName(me)
				if err := fn(longarmd.Schtasks{}, name); err != nil {
					return err
				}
				fmt.Printf("%s %s\n", name, done)
				return nil
			},
		}
	}
	start := task("start", "Start the worker now (the scheduled task)", func(s longarmd.Schtasks, n string) error { return s.Run(n) }, "started")
	stop := &cobra.Command{
		Use: "stop", Short: "Stop the worker until your next logon (running jobs keep running)", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			me, err := longarmd.CurrentUserSID()
			if err != nil {
				return err
			}
			longarmd.Schtasks{}.End(longarmd.SessionTaskName(me))
			longarmd.StopSessionProcesses(dataDir, os.Stdout)
			return nil
		},
	}
	status := &cobra.Command{
		Use: "status", Short: "Show the task, the host and longarmd", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if me, err := longarmd.CurrentUserSID(); err == nil {
				name := longarmd.SessionTaskName(me)
				if (longarmd.Schtasks{}).Exists(name) {
					fmt.Printf("task: %s registered\n", name)
				} else {
					fmt.Printf("task: %s not registered (longarmd session-mode install)\n", name)
				}
			}
			fmt.Print(longarmd.SessionStatus(dataDir))
			return nil
		},
	}
	c.AddCommand(run, install, uninstall, start, stop, status)
	return c
}

// runSessionHost runs the user-session worker's host for dir until
// interrupted.
func runSessionHost(dir, longarmdExe string) error {
	h, done, err := longarmd.NewSessionHost(dir, longarmdExe)
	if err != nil {
		return err
	}
	defer done()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = h.Run(ctx)
	if errors.Is(err, longarmd.ErrHostRunning) {
		fmt.Fprintln(os.Stderr, "longarmd:", err)
		return nil
	}
	return err
}

// sessionHostCommand is what the task runs: host (a path), else
// longarm-tray.exe --worker-host next to this longarmd (a windowless
// GUI binary: no console window at logon), else longarmd session-mode run.
func sessionHostCommand(host string) (string, []string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	if host == "" {
		if p := filepath.Join(filepath.Dir(self), "longarm-tray.exe"); fileExists(p) {
			host = p
		}
	}
	if host != "" {
		h, err := filepath.Abs(host)
		if err != nil {
			return "", nil, err
		}
		if strings.HasPrefix(strings.ToLower(filepath.Base(h)), "longarmd") {
			return h, []string{"session-mode", "run"}, nil
		}
		return h, []string{"--worker-host"}, nil
	}
	return self, []string{"session-mode", "run"}, nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
