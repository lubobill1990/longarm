package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Your own workers (docs/usage.md): register a machine as a worker that
// only you may use, see and share your workers.
//
//	longarm worker add [<name>] [--ttl 24h] [--label k=v]...   a worker code of yours + the longarmd join command
//	longarm worker ls [--json]                                 your workers, shared with you, the fleet
//	longarm worker share <worker> [--user u]... [--team t]... [--everyone]
//	longarm worker unshare <worker> [--user u]... [--team t]... [--everyone]
//	longarm worker remove <worker>                             revoke it

func workerCmd() *cobra.Command {
	c := &cobra.Command{Use: "worker", Short: "Your own workers: register a machine only you may use, list, share, remove"}

	var ttl time.Duration
	var labels []string
	add := &cobra.Command{
		Use:   "add [<name>]",
		Short: "Print a join code for a new worker of yours and the longarmd join command to run on it",
		Long: `Create a worker join code owned by you. The machine that joins with it is
approved at once and usable by you only (hidden from other users, never
placed for them) until you share it - which needs an admin's permission
(longarm admin user set <you> --can-share).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lm, err := sched.ParseLabels(labels, false)
			if err != nil {
				return err
			}
			req := &pb.CreateWorkerCodeRequest{Ttl: durationpb.New(ttl), Labels: lm}
			if len(args) == 1 {
				req.Name = args[0]
			}
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				jc, err := h.Fleet().CreateWorkerCode(ctx, req)
				if err != nil {
					return err
				}
				fmt.Println(jc.Code)
				join := fmt.Sprintf("longarmd join %s %s --ca-fingerprint %s", h.Addr, jc.Code, caFingerprint(s))
				what := "a worker"
				if jc.Name != "" {
					join += " --name " + jc.Name
					what = "worker " + jc.Name
				}
				fmt.Fprintf(os.Stderr, "code for %s owned by %s, expires %s\non the new worker (as the account the service runs as):\n  %s\n"+
					"then run it as a service: longarmd service install (docs/usage.md); it joins approved, usable by you only\n",
					what, jc.Owner, jc.ExpiresAt.AsTime().Local().Format("01-02 15:04"), join)
				return nil
			})
		},
	}
	add.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "validity of the code")
	add.Flags().StringArrayVar(&labels, "label", nil, "label for the worker: KEY=VALUE[,KEY=VALUE...] (repeatable)")

	var asJSON bool
	ls := &cobra.Command{
		Use: "ls", Short: "List your workers, workers shared with you and the fleet's", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				me, err := h.Fleet().Whoami(ctx, &pb.WhoamiRequest{})
				if err != nil {
					return err
				}
				resp, err := h.Fleet().ListNodes(ctx, &pb.ListNodesRequest{})
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(resp)
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "WORKER\tKIND\tOWNER\tSHARED WITH\tSTATE\tJOBS\tCPU%\tMEM GB")
				for _, kind := range []string{"mine", "shared", "fleet"} {
					for _, n := range resp.Nodes {
						if workerKind(n, me.User) != kind {
							continue
						}
						st := n.State
						if st == "approved" {
							st = map[bool]string{true: "online", false: "offline"}[n.Online]
						}
						cpu, mem := "-", "-"
						if m := n.Metrics; m != nil && n.Online {
							cpu = fmt.Sprintf("%.1f", m.CpuPercent)
							mem = humanBytes(m.MemUsedBytes) + "/" + humanBytes(m.MemTotalBytes)
						}
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", n.Name, kind, dash(n.Owner), sharesString(n.Shares), st,
							n.JobsActive, cpu, mem)
					}
				}
				return tw.Flush()
			})
		},
	}
	ls.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	c.AddCommand(add, ls, shareCmd(true), shareCmd(false))
	c.AddCommand(&cobra.Command{
		Use: "remove <worker>", Short: "Remove a worker of yours: revoke its certificates (admins: any worker)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				n, err := h.Fleet().RemoveWorker(ctx, &pb.NodeRequest{Name: args[0]})
				if err != nil {
					return err
				}
				fmt.Printf("%s: %s\n", n.Name, n.State)
				return nil
			})
		},
	})
	return c
}

func shareCmd(share bool) *cobra.Command {
	var users, teams []string
	var everyone bool
	use, short := "share", "Let users, teams or everyone use a worker of yours (needs an admin's permission: can_share)"
	if !share {
		use, short = "unshare", "Stop sharing a worker with users, teams or everyone"
	}
	c := &cobra.Command{
		Use: use + " <worker> [--user U]... [--team T]... [--everyone]", Short: short, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(users) == 0 && len(teams) == 0 && !everyone {
				return errors.New("name at least one --user, --team or --everyone")
			}
			req := &pb.ShareWorkerRequest{Worker: args[0]}
			if share {
				req.AddUsers, req.AddTeams = users, teams
			} else {
				req.RemoveUsers, req.RemoveTeams = users, teams
			}
			if everyone {
				req.SetEveryone, req.Everyone = true, share
			}
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				n, err := h.Fleet().ShareWorker(ctx, req)
				if err != nil {
					return err
				}
				fmt.Printf("%s (owner %s) shared with: %s\n", n.Name, n.Owner, sharesString(n.Shares))
				return nil
			})
		},
	}
	c.Flags().StringArrayVar(&users, "user", nil, "a user (repeatable)")
	c.Flags().StringArrayVar(&teams, "team", nil, "a team (repeatable)")
	c.Flags().BoolVar(&everyone, "everyone", false, "every user of the hub")
	return c
}

// workerKind: mine, shared (with me) or fleet (no owner).
func workerKind(n *pb.NodeInfo, me string) string {
	switch n.Owner {
	case "":
		return "fleet"
	case me:
		return "mine"
	}
	return "shared"
}

// sharesString summarizes whom a worker is shared with ("-" = nobody, or
// not shown to the caller).
func sharesString(s *pb.WorkerShares) string {
	var parts []string
	if s.GetEveryone() {
		parts = append(parts, "everyone")
	}
	parts = append(parts, s.GetUsers()...)
	for _, t := range s.GetTeams() {
		parts = append(parts, "team:"+t)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// caFingerprint of the hub CA this client trusts (for join commands).
func caFingerprint(s *session) string {
	if ca, err := os.ReadFile(filepath.Join(s.cfg.Dir, client.TLSDir, pki.CACertFile)); err == nil {
		if c, err := pki.ParseCertPEM(ca); err == nil {
			return pki.Fingerprint(c)
		}
	}
	return "<longarm-hub ca show>"
}
