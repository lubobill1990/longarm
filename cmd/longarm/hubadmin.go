package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func joinCmd() *cobra.Command {
	var name, fp string
	var force bool
	c := &cobra.Command{
		Use:   "join <hub[:port]> <code>",
		Short: "Enroll this machine's longarm with longarm-hub using a one-time client (or admin) join code",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			tlsDir := filepath.Join(configDir, client.TLSDir)
			if _, err := os.Stat(filepath.Join(tlsDir, pki.NodeCertFile)); err == nil && !force {
				return fmt.Errorf("%s already holds a certificate (use --force to enroll again)", tlsDir)
			}
			addr := hubclient.NormalizeAddr(args[0])
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			// The certificate binds this device's node key from the start, so
			// an endorsement later only needs a renewal (longarm trust).
			tr, err := client.TrustStore(configDir).CSRTrust()
			if err != nil {
				return fmt.Errorf("end-to-end trust: %w", err)
			}
			r, err := hubclient.Join(ctx, addr, args[1], hubclient.JoinOptions{Name: name, Fingerprint: fp, Trust: tr})
			if err != nil {
				return err
			}
			if r.Resp.Role == string(pki.RoleWorker) {
				return errors.New("this join code is for a worker: use `longarmd join` on the worker")
			}
			return installEnrollment(addr, r, tr, "joined")
		},
	}
	c.Flags().StringVar(&name, "name", "", "client name (default: hostname)")
	c.Flags().StringVar(&fp, "ca-fingerprint", "", "expected sha256 fingerprint of the hub CA (longarm-hub ca show)")
	c.Flags().BoolVar(&force, "force", false, "enroll again although a certificate exists")
	return c
}

// installEnrollment stores what join / login got from the hub: the
// certificate bundle, a tailnet key and hub.toml.
func installEnrollment(addr string, r *hubclient.JoinResult, tr *pki.CSRTrust, verb string) error {
	if !r.Pinned {
		fmt.Fprintf(os.Stderr, "warning: hub CA not pinned; trusting %s on first use (pass --ca-fingerprint to verify)\n", r.CAFingerprint)
	}
	var notes []string
	if err := client.InstallEnrollment(configDir, addr, r, tr, func(s string) { notes = append(notes, s) }); err != nil {
		return err
	}
	fmt.Printf("%s %s as %s/%s (config %s)\n", verb, addr, r.Resp.Role, r.Resp.Name, configDir)
	// after the result line, as before: warnings to stderr, the rest (the
	// tailnet key) to stdout
	for _, n := range notes {
		if strings.HasPrefix(n, "warning:") {
			fmt.Fprintln(os.Stderr, n)
		} else {
			fmt.Println(n)
		}
	}
	return nil
}

// withHub runs fn with a hub connection (admin commands need the hub).
func withHub(cmd *cobra.Command, fn func(ctx context.Context, s *session, h *hubclient.Conn) error) error {
	s, err := openSession()
	if err != nil {
		return err
	}
	defer s.close()
	h := s.hub()
	if h == nil {
		return fmt.Errorf("no hub configured (longarm join <hub> <code>, or [hub] address in %s)", filepath.Join(s.cfg.Dir, client.WorkersFile))
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	return fn(ctx, s, h)
}

func printNode(n *pb.NodeInfo) {
	b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(n)
	fmt.Println(string(b))
}

func labelString(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}

func ago(t interface{ AsTime() time.Time }, isNil bool) string {
	if isNil {
		return "-"
	}
	return humanDuration(time.Since(t.AsTime())) + " ago"
}

func hubAdminCmds() []*cobra.Command {
	var all, asJSON bool
	nodes := &cobra.Command{
		Use:   "nodes",
		Short: "List your nodes as the hub shows them to your user (pending ones too); --all includes clients and revoked nodes",
		Long: `List the nodes your user may see: your own workers and devices, the
workers shared with you and the fleet workers (no owner), pending ones too.
With multi-tenancy an admin sees no more than any user here: other users'
machines are only counted, anonymously, by "longarm admin summary".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				resp, err := h.Fleet().ListNodes(ctx, &pb.ListNodesRequest{All: all})
				if err != nil {
					return err
				}
				if asJSON {
					b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(resp)
					fmt.Println(string(b))
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tROLE\tSTATE\tONLINE\tDRAIN\tJOBS\tLABELS\tPROFILE\tVERSION\tLAST SEEN\tCERT EXPIRES")
				for _, n := range resp.Nodes {
					drain := "-"
					if n.Draining {
						drain = n.DrainReason
					}
					prof := n.Profile
					if tc := n.Toolchain; tc != nil && prof != "" {
						prof += fmt.Sprintf(" r%d %s", tc.Revision, tc.State)
					}
					exp := "-"
					if n.CertNotAfter != nil {
						exp = n.CertNotAfter.AsTime().Local().Format("2006-01-02 15:04")
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", n.Name, n.Role, n.State, n.Online, drain,
						n.JobsActive, labelString(n.Labels), prof, n.Version, ago(n.LastSeen, n.LastSeen == nil), exp)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
				// the rest of the fleet: counts only
				if sum, err := h.Admin().FleetSummary(ctx, &pb.FleetSummaryRequest{}); err == nil && sum.Tenancy {
					t := sum.Totals
					fmt.Fprintf(os.Stderr, "fleet: %d users, %d workers (%d online), %d admin workers, %d devices; other users' machines are counted, not listed (longarm admin summary)\n",
						len(sum.Users), t.GetWorkers(), t.GetWorkersOnline(), t.GetAdminWorkers(), t.GetClients())
				}
				return nil
			})
		},
	}
	nodes.Flags().BoolVar(&all, "all", false, "include clients, admins and revoked nodes")
	nodes.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	var role, profile, codeName, codeUser, codeOwner string
	var ttl time.Duration
	var labels []string
	joinCode := &cobra.Command{
		Use:   "join-code",
		Short: "Create a one-time 6-character join code (default: worker, 10 minutes)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lm, err := sched.ParseLabels(labels, false)
			if err != nil {
				return err
			}
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				jc, err := h.Admin().CreateJoinCode(ctx, &pb.CreateJoinCodeRequest{Role: role, Ttl: durationpb.New(ttl), Labels: lm,
					Profile: profile, Name: codeName, User: codeUser, Owner: codeOwner})
				if err != nil {
					return err
				}
				fmt.Println(jc.Code)
				fp := caFingerprint(s)
				join := "longarmd join"
				if jc.Role != "worker" {
					join = "longarm join"
				}
				owned := ""
				if jc.Owner != "" {
					owned = ", owned by " + jc.Owner
				}
				fmt.Fprintf(os.Stderr, "role %s%s, expires %s\non the new machine: %s %s %s --ca-fingerprint %s\n", jc.Role, owned,
					jc.ExpiresAt.AsTime().Local().Format("15:04:05"), join, h.Addr, jc.Code, fp)
				return nil
			})
		},
	}
	joinCode.Flags().StringVar(&role, "role", "worker", "worker | client | admin")
	joinCode.Flags().DurationVar(&ttl, "ttl", 10*time.Minute, "validity")
	joinCode.Flags().StringArrayVar(&labels, "labels", nil, "labels for the node: KEY=VALUE[,KEY=VALUE...] (repeatable)")
	joinCode.Flags().StringVar(&profile, "profile", "", "toolchain profile for a worker")
	joinCode.Flags().StringVar(&codeName, "name", "", "fixed node name")
	joinCode.Flags().StringVar(&codeUser, "user", "", "client/admin: the Longarm user the certificate belongs to (see longarm admin user)")
	joinCode.Flags().StringVar(&codeOwner, "owner", "", "worker: the user who owns it (it joins approved, usable by that user only; see longarm worker)")

	codes := &cobra.Command{
		Use: "join-codes", Short: "List recent join codes (not the codes themselves)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				resp, err := h.Admin().ListJoinCodes(ctx, &pb.ListJoinCodesRequest{})
				if err != nil {
					return err
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tROLE\tEXPIRES\tUSED BY\tCREATED BY\tLABELS")
				for _, c := range resp.Codes {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Id, c.Role, c.ExpiresAt.AsTime().Local().Format("01-02 15:04"),
						c.UsedBy, c.CreatedBy, labelString(c.Labels))
				}
				return tw.Flush()
			})
		},
	}

	nodeAction := func(use, short string, fn func(ctx context.Context, h *hubclient.Conn, name string) (*pb.NodeInfo, error)) *cobra.Command {
		return &cobra.Command{
			Use: use + " <node>", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
					n, err := fn(ctx, h, args[0])
					if err != nil {
						return err
					}
					fmt.Printf("%s: %s", n.Name, n.State)
					if n.Draining {
						fmt.Printf(" (draining: %s)", n.DrainReason)
					}
					fmt.Println()
					return nil
				})
			},
		}
	}
	approve := nodeAction("approve", "Approve a pending node", func(ctx context.Context, h *hubclient.Conn, n string) (*pb.NodeInfo, error) {
		return h.Admin().Approve(ctx, &pb.NodeRequest{Name: n})
	})
	revoke := nodeAction("revoke", "Revoke a node and all its certificates (workers reject them within a heartbeat)", func(ctx context.Context, h *hubclient.Conn, n string) (*pb.NodeInfo, error) {
		return h.Admin().Revoke(ctx, &pb.NodeRequest{Name: n})
	})
	drain := nodeAction("drain", "Send a worker no new jobs (running ones continue)", func(ctx context.Context, h *hubclient.Conn, n string) (*pb.NodeInfo, error) {
		return h.Admin().SetDrain(ctx, &pb.SetDrainRequest{Name: n, Draining: true})
	})
	undrain := nodeAction("undrain", "Let a drained worker get jobs again", func(ctx context.Context, h *hubclient.Conn, n string) (*pb.NodeInfo, error) {
		return h.Admin().SetDrain(ctx, &pb.SetDrainRequest{Name: n, Draining: false})
	})
	label := &cobra.Command{
		Use: "label <node> KEY=VALUE[,KEY=VALUE...]...", Short: "Set node labels (KEY= deletes)", Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := sched.ParseLabels(args[1:], true)
			if err != nil {
				return err
			}
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				n, err := h.Admin().SetLabels(ctx, &pb.SetLabelsRequest{Name: args[0], Labels: m})
				if err != nil {
					return err
				}
				fmt.Println(labelResult(n))
				return nil
			})
		},
	}

	prof := &cobra.Command{Use: "profile", Short: "Toolchain profiles (declarative tool versions; canary rollout)"}
	profSet := &cobra.Command{
		Use:   "set <name> TOOL=VERSION...",
		Short: "Create/update a profile, e.g. `set dev git=latest node=22 pnpm=latest claude=latest pwsh=7`",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := &pb.Profile{Name: args[0]}
			for _, kv := range args[1:] {
				k, v, _ := strings.Cut(kv, "=")
				if v == "" {
					v = "latest"
				}
				p.Tools = append(p.Tools, &pb.ToolSpec{Name: k, Version: v})
			}
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				res, err := h.Admin().PutProfile(ctx, p)
				if err != nil {
					return err
				}
				fmt.Printf("%s revision %d: %s %s\n", res.Name, res.Revision, res.Rollout, res.Message)
				return nil
			})
		},
	}
	profLs := &cobra.Command{
		Use: "ls", Short: "List profiles and their rollout", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				resp, err := h.Admin().ListProfiles(ctx, &pb.ListProfilesRequest{})
				if err != nil {
					return err
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tREV\tROLLOUT\tCANARY\tTOOLS\tMESSAGE")
				for _, p := range resp.Profiles {
					var tools []string
					for _, t := range p.Tools {
						tools = append(tools, t.Name+"="+t.Version)
					}
					fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\n", p.Name, p.Revision, p.Rollout, p.Canary, strings.Join(tools, " "), p.Message)
				}
				return tw.Flush()
			})
		},
	}
	profAssign := &cobra.Command{
		Use: "assign <node> <profile|->", Short: "Assign a profile to a worker (- = none)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := args[1]
			if p == "-" {
				p = ""
			}
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				n, err := h.Admin().AssignProfile(ctx, &pb.AssignProfileRequest{Node: args[0], Profile: p})
				if err != nil {
					return err
				}
				fmt.Printf("%s: profile %q\n", n.Name, n.Profile)
				return nil
			})
		},
	}
	prof.AddCommand(profSet, profLs, profAssign)

	var auditNode string
	var auditLimit int
	var auditJSON bool
	audit := &cobra.Command{
		Use: "audit", Short: "Show audit records forwarded by the workers", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				resp, err := h.Fleet().Audit(ctx, &pb.AuditRequest{Node: auditNode, Limit: int32(auditLimit)})
				if err != nil {
					return err
				}
				for i := len(resp.Records) - 1; i >= 0; i-- {
					r := resp.Records[i]
					if auditJSON {
						fmt.Println(r.Json)
						continue
					}
					fmt.Printf("%s %-10s %-22s %s\n", r.Time.AsTime().Local().Format("01-02 15:04:05"), r.Node, r.Who, r.Method)
				}
				return nil
			})
		},
	}
	audit.Flags().StringVar(&auditNode, "node", "", "only this worker")
	audit.Flags().IntVar(&auditLimit, "limit", 50, "newest N records")
	audit.Flags().BoolVar(&auditJSON, "json", false, "print the raw JSON records")
	return []*cobra.Command{nodes, summaryCmd(), joinCode, codes, approve, revoke, drain, undrain, label, prof, audit, releaseCmd()}
}

// summaryCmd is `longarm admin summary`: the admins' fleet-wide view (Admin.FleetSummary):
// per user the number of workers and devices and how many are online, the
// workers' versions; --rows adds one anonymous row per machine (role, state,
// version, OS, last seen). Never names, IDs, labels, metrics or jobs.
func summaryCmd() *cobra.Command {
	var asJSON, rows bool
	c := &cobra.Command{
		Use:   "summary",
		Short: "Fleet-wide counts per user: workers, devices, online, versions (no names; audited)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Admin().FleetSummary(ctx, &pb.FleetSummaryRequest{Rows: rows})
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(r)
				}
				printSummary(os.Stdout, r, rows)
				return nil
			})
		},
	}
	c.Flags().BoolVar(&rows, "rows", false, "one anonymous row per machine: role, state, version, OS, last seen")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return c
}

func printSummary(w io.Writer, r *pb.FleetSummaryResponse, rows bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tWORKERS\tONLINE\tADMIN WORKERS\tONLINE\tDEVICES\tPENDING\tLAST SEEN")
	row := func(name string, c *pb.FleetCounts, seen *timestamppb.Timestamp) {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n", name, c.GetWorkers(), c.GetWorkersOnline(), c.GetAdminWorkers(),
			c.GetAdminWorkersOnline(), c.GetClients(), c.GetPending(), ago(seen, seen == nil))
	}
	for _, u := range r.Users {
		name := u.User
		if name == "" {
			name = "(fleet)"
		}
		row(name, u.Counts, u.LastSeen)
	}
	row("TOTAL", r.Totals, nil)
	tw.Flush()
	if len(r.WorkerVersions) > 0 {
		fmt.Fprintln(w)
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "VERSION\tWORKERS\tONLINE")
		for _, v := range r.WorkerVersions {
			name := v.Version
			if name == "" {
				name = "-"
			}
			fmt.Fprintf(tw, "%s\t%d\t%d\n", name, v.Workers, v.Online)
		}
		tw.Flush()
	}
	if !rows {
		return
	}
	for _, u := range r.Users {
		if len(u.Machines) == 0 {
			continue
		}
		name := u.User
		if name == "" {
			name = "(fleet)"
		}
		fmt.Fprintf(w, "\n%s:\n", name)
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  ROLE\tSTATE\tVERSION\tOS\tLAST SEEN")
		for _, m := range u.Machines {
			st := "offline"
			switch {
			case m.Online:
				st = "online"
			case m.Role == "client" || m.Role == "admin":
				st = "-"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", m.Role, st, m.Version, m.Os, ago(m.LastSeen, m.LastSeen == nil))
		}
		tw.Flush()
	}
}

// labelResult is the answer to a label change: the worker's labels, or -
// for another user's worker, whose details the hub shows its owner only
// (no machine, no labels in the answer) - just that it worked.
func labelResult(n *pb.NodeInfo) string {
	if n.Role == "worker" && n.Machine == "" {
		return n.Name + ": labels set (another user's worker: the hub shows its labels to its owner)"
	}
	return n.Name + ": " + labelString(n.Labels)
}
