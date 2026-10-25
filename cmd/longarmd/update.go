package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/update"
	"github.com/lubobill1990/longarm/internal/version"
)

func updateCmd() *cobra.Command {
	c := &cobra.Command{Use: "update", Short: "Self-update state (updates are driven by the hub: longarm admin release push)"}
	var dataDir string
	status := &cobra.Command{
		Use: "status", Short: "Show the last self-update (update.json) and whether this build can update", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Printf("version     %s\nupdates     %s\n", version.Version, map[bool]string{true: "enabled (signed releases)", false: "disabled (no update key in this build)"}[update.Enabled()])
			st, err := update.LoadState(dataDir)
			if err != nil {
				return err
			}
			if st == nil {
				fmt.Println("last update none")
				return nil
			}
			b, _ := json.MarshalIndent(st, "", "  ")
			fmt.Printf("last update %s -> %s: %s %s\n%s\n", st.From, st.To, st.Phase, st.Error, b)
			return nil
		},
	}
	status.Flags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory")
	c.AddCommand(status)
	return c
}

// updateWatchCmd is the rollback watchdog, started by longarmd from the previous
// binary right before it restarts into a new version.
func updateWatchCmd() *cobra.Command {
	var dataDir string
	c := &cobra.Command{
		Use: "update-watch", Short: "Self-update watchdog (internal)", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := os.OpenFile(filepath.Join(dataDir, "update-watch.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			log := slog.New(slog.NewTextHandler(f, nil))
			return update.Watch(cmd.Context(), dataDir, log)
		},
	}
	c.Flags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory")
	return c
}
