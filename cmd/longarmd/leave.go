package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/longarmd"
)

// installHelperCmd is run by the MSI's custom actions (as SYSTEM).
func installHelperCmd() *cobra.Command {
	c := &cobra.Command{Use: "install-helper", Short: "Installer steps (internal; used by the MSI)", Hidden: true}
	var dataDir, user, hub, code, fp, logFile, nodeName, isolation string
	var listen []string
	var keep bool
	var loginMode string
	out := func() (io.Writer, func()) {
		if logFile == "" {
			return os.Stdout, func() {}
		}
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return os.Stdout, func() {}
		}
		return io.MultiWriter(os.Stdout, f), func() { f.Close() }
	}
	setup := &cobra.Command{
		Use: "setup", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, done := out()
			defer done()
			fmt.Fprintf(w, "%s longarmd install-helper setup --data-dir %s\n", time.Now().Format(time.RFC3339), dataDir)
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			err := longarmd.InstallSetup(ctx, dataDir, longarmd.SetupOptions{ServiceUser: user, Hub: hub, Code: code, Fingerprint: fp, Listen: listen, Name: nodeName, Out: w, Isolation: isolation, Login: loginMode == "1" || strings.EqualFold(loginMode, "true")})
			if err != nil {
				fmt.Fprintln(w, "error:", err)
			}
			return err
		},
	}
	setup.Flags().StringVar(&dataDir, "data-dir", "", "data directory (required)")
	setup.Flags().StringVar(&user, "user", "", "service account (\"\" or LocalSystem = LocalSystem, with a warning)")
	setup.Flags().StringVar(&hub, "hub", "", "hub address")
	setup.Flags().StringVar(&code, "code", "", "join code")
	setup.Flags().StringVar(&fp, "ca-fingerprint", "", "hub CA fingerprint")
	setup.Flags().StringVar(&nodeName, "name", "", "node name (default: hostname)")
	setup.Flags().StringVar(&logFile, "log", "", "append output here")
	setup.Flags().StringSliceVar(&listen, "listen", nil, "listen URLs for a new longarmd.toml (comma-separated)")
	setup.Flags().StringVar(&isolation, "isolation", "", "per-user: isolate users (the service runs as LocalSystem; docs/security.md)")
	setup.Flags().StringVar(&loginMode, "login-mode", "", "1: no join code; record --hub (and --ca-fingerprint) for a sign-in to come (the service waits for it; the MSI's LOGIN)")
	setup.MarkFlagRequired("data-dir")
	rollback := &cobra.Command{
		Use: "rollback", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, done := out()
			defer done()
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			if err := longarmd.InstallRollback(ctx, dataDir, w); err != nil {
				fmt.Fprintln(w, "warning:", err) // never block a rollback
			}
			return nil
		},
	}
	rollback.Flags().StringVar(&dataDir, "data-dir", "", "data directory (required)")
	rollback.Flags().StringVar(&logFile, "log", "", "append output here")
	rollback.MarkFlagRequired("data-dir")
	remove := &cobra.Command{
		Use: "remove", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, done := out()
			defer done()
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			if _, err := longarmd.Leave(ctx, dataDir, longarmd.LeaveOptions{Reason: "uninstall", Force: true, KillJobs: true, Purge: !keep, Out: w}); err != nil {
				// Never block an uninstall.
				fmt.Fprintln(w, "warning:", err)
			}
			return nil
		},
	}
	remove.Flags().StringVar(&dataDir, "data-dir", "", "data directory (required)")
	remove.Flags().BoolVar(&keep, "keep-data", false, "keep jobs and mirrors (credentials are removed)")
	remove.Flags().StringVar(&logFile, "log", "", "append output here")
	remove.MarkFlagRequired("data-dir")
	c.AddCommand(setup, rollback, remove)
	return c
}
