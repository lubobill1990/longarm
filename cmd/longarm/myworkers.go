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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
//	longarm worker label <worker> KEY=VALUE... [KEY-]...       set / delete labels of your worker
//	longarm worker remove <worker>                             revoke it

func workerCmd() *cobra.Command {
	c := &cobra.Command{Use: "worker", Short: "Your own workers: register a machine only you may use, list, share, label, remove"}

	var ttl time.Duration
	var labels []string
	add := &cobra.Command{
		Use:   "add [<name>]",
		Short: "Make a machine your worker: install Longarm there and sign in (or use a join code this prints)",
		Long: `Make a machine your own worker: usable by you only (hidden from other
users, never placed for them) until you share it - which needs an admin's
permission (longarm admin user set <you> --can-share).

The easy way, on that machine - it signs in with your Microsoft account:
  Windows: install the Longarm MSI and choose "This machine runs jobs"
           (the tray signs it in), or run: longarmd login <hub>
  Linux / macOS: longarmd login <hub>, as the account the worker runs as

Where nobody can sign in with a browser (a headless box), use the join
code this command prints, with the longarmd join command it shows. Then
start the worker: on Windows longarmd session-mode install (runs while
you are logged on; no password), elsewhere longarmd service install.`,
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
				fmt.Fprint(os.Stderr, workerAddHelp(h.Addr, jc.Code, caFingerprint(s), jc.Name, jc.Owner,
					jc.ExpiresAt.AsTime().Local().Format("01-02 15:04")))
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
				short := shortIDsOf(resp.Nodes)
				fmt.Fprintln(tw, "WORKER\tID\tKIND\tOWNER\tSHARED WITH\tSTATE\tJOBS\tCPU%\tMEM GB")
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
						k := kind
						if n.AdminWorker {
							k += " (admin)" // runs commands as SYSTEM; owner-only, never placed
						}
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", n.Name, dash(short[n.Id]), k, dash(n.Owner), sharesString(n.Shares), st,
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
		Use:   "label <worker> KEY=VALUE[,KEY=VALUE...]... [KEY-]...",
		Short: "Set labels of a worker of yours (KEY- or KEY= deletes; admins: any worker)",
		Long: `Set or delete labels of your own worker - scheduler constraints, e.g.
longarm run --require pool=devbox - like longarm worker add --label at
registration. KEY- (or KEY=) deletes a label. The hub checks that the
worker is yours (admins: any worker) and audits the change. Labels the
worker reports itself (longarmd.toml [hub] labels) are changed there.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := parseLabelEdits(args[1:])
			if err != nil {
				return err
			}
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				n, err := h.Fleet().SetWorkerLabels(ctx, &pb.SetLabelsRequest{Name: args[0], Labels: m})
				if status.Code(err) == codes.Unimplemented {
					return errors.New("this hub is too old for longarm worker label (update it; admins: longarm admin label)")
				}
				if err != nil {
					return err
				}
				fmt.Printf("%s: %s\n", n.Name, labelString(n.Labels))
				for k, v := range m {
					if got, ok := n.Labels[k]; v == "" && ok {
						fmt.Fprintf(os.Stderr, "longarm: %s=%s stays: the worker reports it itself (longarmd.toml [hub] labels)\n", k, got)
					}
				}
				return nil
			})
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "rename <worker> <new-name>",
		Short: "Rename a worker of yours (admins: any worker); it keeps its ID",
		Long: `Rename a worker of yours (admins: any worker). It keeps its ID, so jobs,
links and shares stay with it; <worker> is a name, an ID, a unique ID prefix,
id:<prefix> or name:<name>. The new name must not be in use by another node
that isn't revoked and must not look like an ID. Audited.

With end-to-end trust, an endorsement made before node IDs is bound to the
old name: endorse the worker again (longarmd trust request / longarm trust
endorse), or target it by ID.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				n, err := h.Fleet().RenameWorker(ctx, &pb.RenameNodeRequest{Target: args[0], NewName: args[1]})
				if status.Code(err) == codes.Unimplemented {
					return errors.New("this hub is too old to rename workers (update it)")
				}
				if err != nil {
					return err
				}
				printRenamed(n)
				return nil
			})
		},
	})
	c.AddCommand(&cobra.Command{
		Use: "remove <worker>", Aliases: []string{"rm"}, Short: "Remove a worker of yours: revoke its certificates (admins: any worker)", Args: cobra.ExactArgs(1),
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

// workerAddHelp is what worker add prints on stderr: the sign-in way
// first, then the join code's.
func workerAddHelp(hub, code, fp, name, owner, expires string) string {
	join := fmt.Sprintf("longarmd join %s %s --ca-fingerprint %s", hub, code, fp)
	what := "a worker"
	if name != "" {
		join += " --name " + name
		what = "worker " + name
	}
	return fmt.Sprintf(`The easy way, on the new machine (signs in with your Microsoft account):
  Windows: install the Longarm MSI and choose "This machine runs jobs", or run: longarmd login %s
  Linux / macOS: longarmd login %s
Or with the code above (for %s, owned by %s, expires %s), on the new machine:
  %s
  then start it - Windows: longarmd session-mode install (runs while you are logged on)
                  Linux / macOS: sudo longarmd service install --user <account>
It joins approved, usable by you only.
`, hub, hub, what, owner, expires, join)
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

// parseLabelEdits parses `worker label` arguments: KEY=VALUE sets, KEY-
// and KEY= delete (an empty value in the result); comma lists allowed.
func parseLabelEdits(args []string) (map[string]string, error) {
	var norm []string
	for _, a := range args {
		items := strings.Split(a, ",")
		for i, it := range items {
			if k, ok := strings.CutSuffix(strings.TrimSpace(it), "-"); ok && !strings.Contains(it, "=") {
				items[i] = k + "="
			}
		}
		norm = append(norm, strings.Join(items, ","))
	}
	return sched.ParseLabels(norm, true)
}
