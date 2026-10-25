package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/transport"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The hub's tailnet (Tailscale or Headscale) for longarm's embedded tailscale
// node: the hub tells every client its tailnet in Whoami (asked once per longarm
// run that lists nodes). When it differs from hub.toml [tailnet] - the hub
// switched providers - longarm records the new control server; the embedded node
// then runs from that server's state dir and gets a key from the hub when
// it has none. An admin re-key (longarm admin tailnet rekey) wipes the state and
// fetches a fresh key.

// syncTailnet applies the hub's tailnet info (from Whoami).
func (s *session) syncTailnet(ctx context.Context, h *hubclient.Conn, ti *pb.TailnetInfo) {
	if ti == nil { // a hub from before tailnet providers
		return
	}
	hf, err := hubclient.LoadFile(s.cfg.Dir)
	if err != nil || hf.Address == "" || hubclient.NormalizeAddr(hf.Address) != hubclient.NormalizeAddr(s.cfg.Hub.Address) {
		return // the hub comes from workers.toml: no tailnet bookkeeping
	}
	cur := hf.Tailnet
	if ti.Provider == "" || ti.ControlUrl == "" {
		if cur.ControlURL != "" {
			hf.Tailnet = hubclient.TailnetConfig{}
			if hubclient.SaveFile(s.cfg.Dir, hf) == nil {
				s.dialer.ResetTailnet(ctx, false)
				s.dialer.SetTailnet(hf.Tailnet)
				fmt.Fprintf(os.Stderr, "longarm: the hub has no tailnet any more (was %s)\n", cur.ControlURL)
			}
		}
		return
	}
	switched := !transport.SameControl(cur.ControlURL, ti.ControlUrl)
	if !switched && !ti.Rekey {
		if cur.Provider != ti.Provider {
			hf.Tailnet.Provider = ti.Provider
			hubclient.SaveFile(s.cfg.Dir, hf)
		}
		return
	}
	next := hubclient.TailnetConfig{Provider: ti.Provider, ControlURL: ti.ControlUrl, Hostname: cur.Hostname}
	if next.Hostname == "" {
		next.Hostname = "longarm-client-" + s.dialer.Identity().Name
	}
	// A running node (longarm daemon) stops first: logged out for a re-key, kept
	// (its state stays for switching back) for a switch.
	s.dialer.ResetTailnet(ctx, ti.Rekey && !switched)
	dir := hubclient.TailnetStateDir(s.cfg.Dir, ti.ControlUrl)
	if ti.Rekey {
		if err := transport.WipeState(dir, dir+".authkey"); err != nil {
			fmt.Fprintf(os.Stderr, "longarm: tailnet re-key: %v\n", err)
			return // the hub keeps the request: next time
		}
	}
	hf.Tailnet = next
	if err := hubclient.SaveFile(s.cfg.Dir, hf); err != nil {
		fmt.Fprintf(os.Stderr, "longarm: save hub.toml: %v\n", err)
		return
	}
	s.dialer.SetTailnet(next)
	if switched {
		fmt.Fprintf(os.Stderr, "longarm: the hub's tailnet is now %s (%s)\n", ti.Provider, ti.ControlUrl)
	}
	if ti.Rekey {
		// Fetch the key now: that also clears the hub's re-key request.
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		t, err := h.Fleet().TailnetKey(cctx, &pb.TailnetKeyRequest{Reason: "rekey"})
		if err != nil {
			fmt.Fprintf(os.Stderr, "longarm: tailnet re-key: %s\n", oneLine(err))
			return
		}
		if transport.SameControl(t.ControlUrl, ti.ControlUrl) {
			if err := hubclient.SaveTailnet(s.cfg.Dir, &hf, t); err == nil {
				hubclient.SaveFile(s.cfg.Dir, hf)
			}
		}
		fmt.Fprintf(os.Stderr, "longarm: tailnet re-keyed (%s): the embedded node enrols again on its next use\n", ti.ControlUrl)
	}
}

// logoutTailnet logs longarm's embedded tailscale nodes out (longarm leave; best
// effort, bounded). Ephemeral client nodes vanish anyway.
func logoutTailnet(ctx context.Context, dir string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	hf, _ := hubclient.LoadFile(dir)
	for _, sd := range transport.StateDirs(client.TSNetDirFor(dir)) {
		control, host := transport.JoinedControl(sd), ""
		if control == "" && hf.Tailnet.ControlURL != "" && sd == hubclient.TailnetStateDir(dir, hf.Tailnet.ControlURL) {
			control = hf.Tailnet.ControlURL
		}
		if transport.SameControl(control, hf.Tailnet.ControlURL) {
			host = hf.Tailnet.Hostname
		}
		if err := transport.LogoutDir(ctx, sd, control, host); err != nil {
			fmt.Fprintf(os.Stderr, "tailscale node (%s) not logged out: %v\n", strings.TrimPrefix(sd, dir), err)
		}
	}
}

func tailnetAdminCmd() *cobra.Command {
	c := &cobra.Command{Use: "tailnet", Short: "The hub's tailnet (Tailscale or Headscale): provider and per-node state; re-key a node"}
	var asJSON bool
	st := &cobra.Command{
		Use: "status", Short: "Show the tailnet provider, control URL and every node's tailnet state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Admin().TailnetStatus(ctx, &pb.TailnetStatusRequest{})
				if err != nil {
					return err
				}
				if asJSON {
					b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(r)
					fmt.Println(string(b))
					return nil
				}
				printTailnetStatus(r)
				return nil
			})
		},
	}
	st.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	rekey := &cobra.Command{
		Use:   "rekey <node>",
		Short: "Make a node log out of the tailnet and enrol again with a fresh key (now when online, else on its next hub contact)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				n, err := h.Admin().TailnetRekey(ctx, &pb.NodeRequest{Name: args[0]})
				if err != nil {
					return err
				}
				when := "on its next hub contact"
				if n.Online {
					when = "now (online)"
				}
				fmt.Printf("re-key requested for %s %s: it re-enrols %s\n", n.Role, n.Name, when)
				if n.Role == "worker" {
					fmt.Println("the old device stays in the tailnet (logged out): remove it in the admin console")
				}
				return nil
			})
		},
	}
	c.AddCommand(st, rekey)
	return c
}

func printTailnetStatus(r *pb.TailnetStatusResponse) {
	if r.Provider == "none" {
		fmt.Println("provider  none (no [tailnet] provider on the hub: nodes use only tcp/longnet)")
	} else {
		fmt.Printf("provider  %s\ncontrol   %s\n", r.Provider, r.ControlUrl)
		if r.Tailnet != "" {
			fmt.Printf("tailnet   %s\n", r.Tailnet)
		}
		eph := "not ephemeral"
		if r.EphemeralClients {
			eph = "ephemeral"
		}
		fmt.Printf("keys      workers %s, clients %s (%s); one-time, valid %s\n", strings.Join(r.WorkerTags, ","),
			strings.Join(r.ClientTags, ","), eph, r.KeyTtl.AsDuration())
	}
	tagsRejected := false
	if e := r.LastMintError; e != "" {
		if i := strings.Index(e, "; the Tailscale trust credential"); i >= 0 { // the hub's hint (hub.TagsHint): printed below, laid out
			e = e[:i] + ")"
		}
		fmt.Printf("key error %s, for %s: %s\n", ago(r.LastMintErrorAt, false), r.LastMintErrorNode, e)
		tagsRejected = strings.Contains(e, "requested tags")
	}
	for _, n := range r.Nodes {
		tagsRejected = tagsRejected || strings.Contains(n.State.GetError(), "requested tags")
	}
	if tagsRejected {
		fmt.Printf(`hint      Tailscale refused the key's tags. A trust credential (OAuth client) may only mint keys
          with exactly its own tags, or tags its tags own. Working setup:
            - policy tagOwners: "tag:longarm-hub": ["autogroup:admin"],
                                "%s": ["autogroup:admin", "tag:longarm-hub"],
                                "%s": ["autogroup:admin", "tag:longarm-hub"]
            - the credential: only tag:longarm-hub, scope auth_keys (write)
          See deploy/hub/README.md, "Tailnet provider".
`, strings.Join(r.WorkerTags, `", "`), strings.Join(r.ClientTags, `", "`))
	}
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tROLE\tONLINE\tTAILNET\tNAME\tIPS\tCONTROL\tLAST KEY")
	for _, n := range r.Nodes {
		state, name, ips, control := "-", "-", "-", "-"
		if t := n.State; t != nil {
			state = t.State
			if t.Since != nil {
				state += " " + ago(t.Since, false)
			}
			if t.DnsName != "" {
				name = t.DnsName
			} else if t.Hostname != "" {
				name = t.Hostname
			}
			if len(t.Ips) > 0 {
				ips = strings.Join(t.Ips, ",")
			}
			if t.ControlUrl != "" {
				control = t.ControlUrl
				if r.ControlUrl != "" && !transport.SameControl(t.ControlUrl, r.ControlUrl) {
					control += " (old)"
				}
			}
			if t.Error != "" && t.State != "running" {
				state += ": " + shortTailnetErr(t.Error)
			}
		}
		key := "-"
		if n.KeyMintedAt != nil {
			key = ago(n.KeyMintedAt, false)
			if n.KeyReason != "" {
				key += " (" + n.KeyReason + ")"
			}
		}
		if n.RekeyPending {
			key += ", re-key pending"
		}
		online := "-"
		if n.Role == "worker" {
			online = fmt.Sprint(n.Online)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", n.Name, n.Role, online, state, name, ips, control, key)
	}
	tw.Flush()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// shortTailnetErr keeps a node's tailnet error readable in the table: the
// last rpc layer, without the hub's tags hint (printed above), at most 120
// characters (--json has all of it).
func shortTailnetErr(s string) string {
	s = firstLine(s)
	if i := strings.Index(s, "; the Tailscale trust credential"); i >= 0 {
		s = s[:i] + ")"
	}
	if i := strings.LastIndex(s, "desc = "); i >= 0 {
		s = s[i+len("desc = "):]
	}
	if r := []rune(s); len(r) > 120 {
		s = string(r[:117]) + "..."
	}
	return s
}
