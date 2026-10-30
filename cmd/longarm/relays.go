package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/lubobill1990/longarm/internal/hubclient"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// relaysAdminCmd: longarm admin relays - the hub's relay set and how many
// workers follow it (docs/usage.md "External relays").
func relaysAdminCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "relays",
		Short: "Show the hub's relay set (external relays first, the embedded one last) and how many workers use it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Admin().Relays(ctx, &pb.RelaysRequest{})
				if err != nil {
					return err
				}
				if asJSON {
					b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(r)
					fmt.Println(string(b))
					return nil
				}
				printRelays(os.Stdout, r, time.Now())
				return nil
			})
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return c
}

func printRelays(w io.Writer, r *pb.RelaysResponse, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RELAY\tUDP\tTCP\tSTATE\tVERSION")
	for _, st := range r.Relays {
		e := st.Endpoint
		kind, state := e.GetName(), "connected"
		if e.GetEmbedded() {
			kind += " (embedded)"
			state = "restarts with the hub"
		} else if !st.Connected {
			state = "disconnected"
			if ls := st.LastSeen.AsTime(); st.LastSeen != nil && !ls.IsZero() {
				state += ", last seen " + humanDuration(now.Sub(ls)) + " ago"
			}
		}
		tcp := e.GetTcp()
		if tcp == "" {
			tcp = "-"
			if e.GetEmbedded() {
				tcp = "(the hub's port)"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", kind, e.GetUdp(), tcp, state, st.Version)
	}
	tw.Flush()
	total := r.WorkersRelaySet + r.WorkersLegacy
	fmt.Fprintf(w, "\nonline workers: %d use the relay set, %d only the embedded relay (older versions)", r.WorkersRelaySet, r.WorkersLegacy)
	if len(r.LegacyVersions) > 0 {
		var vs []string
		for v, n := range r.LegacyVersions {
			vs = append(vs, fmt.Sprintf("%s: %d", v, n))
		}
		sort.Strings(vs)
		fmt.Fprintf(w, " [%s]", strings.Join(vs, ", "))
	}
	fmt.Fprintln(w)
	if r.WorkersLegacy > 0 && total > 0 {
		fmt.Fprintln(w, "Workers on older versions lose their relayed connections while the hub restarts; they pick up the relay set after updating (longarm worker update).")
	}
}
