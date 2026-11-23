package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"longarm/internal/hubclient"
	pb "longarm/proto/longarm/v1"
)

func displaySecurityMode(mode string) string {
	if mode == "" {
		return "legacy"
	}
	return mode
}

func validSecurityUpgrade(mode string) error {
	if mode != "shadow" && mode != "e2e" {
		return fmt.Errorf("security upgrade must be shadow or e2e")
	}
	return nil
}

func securityCmd() *cobra.Command {
	c := &cobra.Command{Use: "security", Short: "Security mode on the hub (recorded, not enforced in v1)"}
	status := &cobra.Command{
		Use: "status", Short: "Show your security mode and the hub's rollout stage", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, _ *session, h *hubclient.Conn) error {
				r, err := h.Fleet().Whoami(ctx, &pb.WhoamiRequest{})
				if err != nil {
					return err
				}
				mode := displaySecurityMode(r.SecurityMode)
				fmt.Printf("security: %s (hub stage %d)\n", mode, r.SecurityStage)
				if mode == "legacy" && r.SecurityStage >= 1 {
					fmt.Println("end-to-end trust is available: longarm security upgrade shadow (see docs)")
				} else if r.SecurityStage == 0 {
					fmt.Println("nothing to do at hub stage 0")
				} else {
					fmt.Println("mode recorded; enforcement comes with the next release (see docs)")
				}
				return nil
			})
		},
	}
	upgrade := &cobra.Command{
		Use: "upgrade shadow|e2e", Short: "Record a stricter security mode for your user",
		Args: cobra.ExactArgs(1), ValidArgs: []string{"shadow", "e2e"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validSecurityUpgrade(args[0]); err != nil {
				return err
			}
			return withHub(cmd, func(ctx context.Context, _ *session, h *hubclient.Conn) error {
				r, err := h.Fleet().SetSecurityMode(ctx, &pb.SetSecurityModeRequest{Mode: args[0]})
				if err != nil {
					return err
				}
				fmt.Printf("security: %s (recorded, not enforced in v1)\n", displaySecurityMode(r.SecurityMode))
				return nil
			})
		},
	}
	c.AddCommand(status, upgrade)
	return c
}
