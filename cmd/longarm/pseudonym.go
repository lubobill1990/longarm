package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/pii"
	"github.com/lubobill1990/longarm/internal/pki"
)

// pseudonymCmd prints the pseudonyms telemetry and diagnostic bundles use
// for names (docs/troubleshooting.md): with them a maintainer finds a known
// node or user in Application Insights. They are keyed with the fleet's CA
// certificate, so only fleet members can compute them.
func pseudonymCmd() *cobra.Command {
	var kind string
	c := &cobra.Command{
		Use:   "pseudonym <name>...",
		Short: "Print the telemetry / diagnostics pseudonym of node, user or host names (<node:3f2a9c>)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			switch kind {
			case pii.KindNode, pii.KindUser, pii.KindHost:
			default:
				return fmt.Errorf("--kind %q: node, user or host", kind)
			}
			b, err := os.ReadFile(filepath.Join(configDir, client.TLSDir, pki.CACertFile))
			if err != nil {
				return fmt.Errorf("%w (join a hub first: longarm join)", err)
			}
			ca, err := pki.ParseCertPEM(b)
			if err != nil {
				return err
			}
			s := pii.New(pii.Options{Salt: pii.FleetSalt(ca.Raw), NoLocal: true})
			for _, n := range args {
				fmt.Printf("%s\t%s\n", n, s.Hash(kind, n))
			}
			return nil
		},
	}
	c.Flags().StringVar(&kind, "kind", pii.KindNode, "node, user or host")
	return c
}
