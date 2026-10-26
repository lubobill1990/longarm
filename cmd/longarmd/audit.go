package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/server"
)

func auditCmd() *cobra.Command {
	c := &cobra.Command{Use: "audit", Short: "Audit log tools"}
	var dataDir, file string
	verify := &cobra.Command{
		Use:   "verify",
		Short: "Check the hash chain of audit.log (or of a copy made by an [audit] sink)",
		Long: `Check that the records of the audit log form an unbroken hash chain with
consecutive seq numbers: an edited, deleted or reordered line breaks it.
Prints "ok: N records" or the first broken record and exits 1. Changes to the
last line, or a rewritten log with a recomputed chain, can only be noticed
by comparing with an off-machine copy ([audit] sinks, the hub).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				file = filepath.Join(dataDir, longarmd.AuditFile)
			}
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			defer f.Close()
			v, err := server.VerifyAudit(f)
			if err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			if v.Broken != nil {
				return fmt.Errorf("%s: chain broken at %s", file, *v.Broken)
			}
			fmt.Printf("ok: %d records\n", v.Records)
			if v.Unchained > 0 {
				fmt.Printf("note: %d lines before the chain (written by an older longarmd)\n", v.Unchained)
			}
			if len(v.Starts) > 0 {
				lines := make([]string, len(v.Starts))
				for i, n := range v.Starts {
					lines[i] = fmt.Sprint(n)
				}
				fmt.Printf("note: the chain (re)starts at line %s; the reason is in the %s record\n", strings.Join(lines, ", "), server.AuditChainStart)
			}
			if v.LinksOut {
				fmt.Printf("note: the first record (seq %d) continues a chain that isn't in this file (a sink's copy, or the start was cut off)\n", v.FirstSeq)
			}
			if v.Partial {
				fmt.Println("note: the last line is incomplete (being written?)")
			}
			return nil
		},
	}
	verify.Flags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory")
	verify.Flags().StringVar(&file, "file", "", "audit log to check (default <data-dir>/audit.log)")
	c.AddCommand(verify)
	return c
}
