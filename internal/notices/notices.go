// Package notices carries Longarm's license and the third-party notices
// (THIRD_PARTY_NOTICES.txt: written by `go run ./packaging/notices`, never by
// hand), which the binaries print (`licenses`) and the hub panel and the tray
// serve, as the licenses of the code they include ask for.
package notices

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"
)

//go:embed THIRD_PARTY_NOTICES.txt
var Text string

// Cmd is the `licenses` command of every binary.
func Cmd() *cobra.Command {
	return &cobra.Command{
		Use:   "licenses",
		Short: "Print Longarm's license and the licenses of the software it includes",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			_, err := fmt.Fprint(c.OutOrStdout(), Text)
			return err
		},
	}
}
