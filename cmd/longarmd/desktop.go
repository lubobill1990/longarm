package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"

	"longarm/internal/desktopsvc"
	"longarm/internal/platform"
)

// desktopServiceCmd: the Longarm Desktop service (internal/desktopsvc).
func desktopServiceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "desktop-service",
		Short: "The Longarm Desktop service (Windows): remote desktop for UAC prompts, the lock screen and service workers",
		Long: `The Longarm Desktop service lets the owner of this worker see and use the
screen someone at the machine sees - UAC prompts, the lock screen and
Ctrl+Alt+Del included - with longarm desktop. A LocalSystem service (the
MSI installs it with the worker role) keeps an agent running as SYSTEM in
the console session; longarmd relays its owner's stream there. Only
SYSTEM, administrators and the accounts given at install (the worker's)
may use the agent. docs/remote-desktop.md.`,
	}
	var clients []string
	install := &cobra.Command{
		Use:   "install",
		Short: "Install (or update) and start the service (elevated)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !platform.Elevated() {
				return errors.New("run it elevated (as administrator): the service runs as SYSTEM")
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if len(clients) == 0 {
				// The worker's accounts: this user, and longarmd's service account.
				if sid := desktopsvc.CurrentUserSID(); sid != "" {
					clients = append(clients, sid)
				}
			}
			var sids []string
			for _, c := range clients {
				if strings.HasPrefix(c, "S-1-") {
					sids = append(sids, c)
					continue
				}
				sid, err := desktopsvc.AccountSID(c)
				if err != nil {
					return fmt.Errorf("--client %s: %w", c, err)
				}
				sids = append(sids, sid)
			}
			if err := desktopsvc.Install(exe, sids); err != nil {
				return err
			}
			fmt.Printf("the %s service runs; its agent is for SYSTEM, administrators and %v\n", desktopsvc.ServiceName, sids)
			return nil
		},
	}
	install.Flags().StringSliceVar(&clients, "client", nil, "an account (or SID) that may use the agent: longarmd's (default: you)")
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the service (elevated)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !platform.Elevated() {
				return errors.New("run it elevated (as administrator)")
			}
			return desktopsvc.Uninstall()
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Whether the service is installed and its agent answers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !desktopsvc.Installed() {
				fmt.Println("not installed")
				return nil
			}
			_, ok := desktopsvc.Agent(cmd.Context())
			fmt.Printf("installed; agent: %v; clients: %v\n", map[bool]string{true: "answers", false: "not reachable from this account"}[ok], desktopsvc.Clients())
			return nil
		},
	}
	run := &cobra.Command{
		Use:    "run",
		Short:  "Run the service (the service manager does)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !desktopsvc.RunningAsSystem() {
				return errors.New("the Longarm Desktop service runs as LocalSystem")
			}
			log, done := desktopLog("service.log")
			defer done()
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			fn := func(ctx context.Context) error { return desktopsvc.RunService(ctx, exe, log) }
			if handled, err := platform.RunAsService(desktopsvc.ServiceName, fn); handled {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			return fn(ctx)
		},
	}
	c.AddCommand(install, uninstall, status, run)
	return c
}

// desktopAgentCmd: the agent the service starts in the console session.
func desktopAgentCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "desktop-agent",
		Short:  "The Longarm Desktop service's agent (SYSTEM in the console session)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !desktopsvc.RunningAsSystem() {
				return errors.New("the desktop agent runs as SYSTEM (the Longarm Desktop service starts it)")
			}
			log, done := desktopLog("agent.log")
			defer done()
			return desktopsvc.RunAgent(cmd.Context(), log)
		},
	}
}

func desktopLog(name string) (*slog.Logger, func()) {
	f, err := desktopsvc.LogFile(name)
	if err != nil {
		return slog.Default(), func() {}
	}
	return slog.New(slog.NewTextHandler(f, nil)), func() { f.Close() }
}
