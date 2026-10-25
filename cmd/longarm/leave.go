package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func leaveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "leave",
		Short: "Deregister this client from the hub (revokes its certificate) and remove its credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			if h := s.hub(); h != nil {
				ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
				_, err := h.Node().Leave(ctx, &pb.LeaveRequest{})
				cancel()
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: hub %s: %v\n", s.cfg.Hub.Address, oneLine(err))
				} else {
					fmt.Printf("left hub %s as %s\n", s.cfg.Hub.Address, s.dialer.Identity().Name)
				}
			}
			s.close()
			logoutTailnet(cmd.Context(), configDir)
			for _, p := range []string{client.TLSDir, hubclient.File, client.CacheFile, hubCacheFile} {
				os.RemoveAll(filepath.Join(configDir, p))
			}
			hubclient.RemoveTailnetState(configDir)
			fmt.Printf("removed the credentials from %s\n", configDir)
			return nil
		},
	}
}
