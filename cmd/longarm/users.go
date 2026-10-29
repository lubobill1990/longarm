package main

import (
	"context"
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
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Multi-tenancy administration (docs/security.md): Longarm users with
// their devices (certificates), teams and quotas.
//
//	longarm admin user add <name> [--team T]... [quota flags] [--device D] [--admin] [--ttl 24h] [--panel] [--no-code]
//	longarm admin user code <name> [--device D] [--admin] [--ttl 24h]   join code for another device
//	longarm admin user set <name> [--team T]... [quota flags] [--clear-quota] [--can-share[=false]]
//	longarm admin user ls [--json]
//	longarm admin user disable|enable <name>
//	longarm admin user remove <name> [--purge]
//	longarm admin team add|rm <team> <user>...
//	longarm admin team ls
//	longarm whoami

// quotaFlags binds the quota flags of user add / set.
type quotaFlags struct {
	q   pb.Quota
	set bool
}

func (f *quotaFlags) bind(c *cobra.Command) {
	fl := c.Flags()
	fl.Int32Var(&f.q.MaxJobs, "max-jobs", 0, "quota: active jobs in the whole fleet (0 = no limit)")
	fl.Float64Var(&f.q.MaxMemGb, "max-mem-gb", 0, "quota: memory reserved by active jobs in the fleet, GB")
	fl.Int32Var(&f.q.MaxJobsPerWorker, "max-jobs-per-worker", 0, "quota: active jobs per worker")
	fl.Float64Var(&f.q.MaxMemGbPerWorker, "max-mem-gb-per-worker", 0, "quota: reserved memory per worker, GB")
	fl.Int32Var(&f.q.MaxSessionsPerWorker, "max-sessions-per-worker", 0, "quota: interactive sessions per worker")
	fl.Float64Var(&f.q.JobMemGb, "job-mem-gb", 0, "default memory limit of a job's process tree, GB")
	fl.Float64Var(&f.q.MaxJobMemGb, "max-job-mem-gb", 0, "largest memory limit a job may ask for, GB")
	fl.Float64Var(&f.q.JobCpus, "job-cpus", 0, "default CPU limit of a job, cores")
	fl.Float64Var(&f.q.MaxJobCpus, "max-job-cpus", 0, "largest CPU limit a job may ask for, cores")
}

func (f *quotaFlags) changed(c *cobra.Command) bool {
	for _, n := range []string{"max-jobs", "max-mem-gb", "max-jobs-per-worker", "max-mem-gb-per-worker", "max-sessions-per-worker",
		"job-mem-gb", "max-job-mem-gb", "job-cpus", "max-job-cpus"} {
		if c.Flags().Changed(n) {
			return true
		}
	}
	return false
}

func quotaString(q *pb.Quota) string {
	if q == nil {
		return "-"
	}
	var parts []string
	add := func(name string, v float64) {
		if v != 0 {
			parts = append(parts, fmt.Sprintf("%s=%g", name, v))
		}
	}
	add("jobs", float64(q.MaxJobs))
	add("mem_gb", q.MaxMemGb)
	add("jobs/worker", float64(q.MaxJobsPerWorker))
	add("mem_gb/worker", q.MaxMemGbPerWorker)
	add("sessions/worker", float64(q.MaxSessionsPerWorker))
	add("job_mem_gb", q.JobMemGb)
	add("max_job_mem_gb", q.MaxJobMemGb)
	add("job_cpus", q.JobCpus)
	add("max_job_cpus", q.MaxJobCpus)
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

// printJoinCode prints a user's join code and how to use it.
func printJoinCode(s *session, h *hubclient.Conn, jc *pb.JoinCode) {
	fmt.Println(jc.Code)
	fp := "<longarm-hub ca show>"
	if ca, err := os.ReadFile(filepath.Join(s.cfg.Dir, client.TLSDir, pki.CACertFile)); err == nil {
		if c, err := pki.ParseCertPEM(ca); err == nil {
			fp = pki.Fingerprint(c)
		}
	}
	fmt.Fprintf(os.Stderr, "%s certificate %s for user %s, code expires %s\non the user's machine: longarm join %s %s --ca-fingerprint %s\n",
		jc.Role, jc.Name, jc.User, jc.ExpiresAt.AsTime().Local().Format("01-02 15:04"), h.Addr, jc.Code, fp)
}

func printPanel(r *pb.PutUserResponse) {
	if r.PanelPassword == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "web panel login %s (shown once): password %s\nTOTP secret %s\n%s\n", r.User.GetName(), r.PanelPassword,
		r.TotpSecret, r.Otpauth)
}

func userAdminCmd() *cobra.Command {
	c := &cobra.Command{Use: "user", Short: "Longarm users: devices (join codes), teams, quotas, disable, remove (multi-tenancy)"}

	var teams []string
	var qf quotaFlags
	var device string
	var admin, panel, noCode bool
	var ttl time.Duration
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a user and print a client join code for the user's first device",
		Long: `Add a Longarm user. The join code enrols a device (longarm join) with a
certificate of this user; every job, session, file and forward the user
creates belongs to the user and - on workers with [isolation] mode =
"per-user" - runs as the user's own OS account (la-<user>).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &pb.PutUserRequest{Name: args[0], Create: true, Teams: teams, SetTeams: true, JoinCode: !noCode,
				Ttl: durationpb.New(ttl), Device: device, Admin: admin, Panel: panel}
			if qf.changed(cmd) {
				req.Quota, req.SetQuota = &qf.q, true
			}
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Admin().PutUser(ctx, req)
				if err != nil {
					return err
				}
				if r.JoinCode != nil {
					printJoinCode(s, h, r.JoinCode)
				} else {
					fmt.Println("added", r.User.Name)
				}
				printPanel(r)
				return nil
			})
		},
	}
	add.Flags().StringArrayVar(&teams, "team", nil, "team the user belongs to (repeatable)")
	qf.bind(add)
	add.Flags().StringVar(&device, "device", "", "node name of the device (default: the user name, or <user>-N when taken)")
	add.Flags().BoolVar(&admin, "admin", false, "the device gets an admin certificate")
	add.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "validity of the join code")
	add.Flags().BoolVar(&panel, "panel", false, "also create a web panel login (prints a generated password and TOTP secret once)")
	add.Flags().BoolVar(&noCode, "no-code", false, "no join code")

	var cdevice string
	var cadmin bool
	var cttl time.Duration
	code := &cobra.Command{
		Use:   "code <name>",
		Short: "Print a join code for another device of a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Admin().PutUser(ctx, &pb.PutUserRequest{Name: args[0], JoinCode: true, Ttl: durationpb.New(cttl),
					Device: cdevice, Admin: cadmin})
				if err != nil {
					return err
				}
				printJoinCode(s, h, r.JoinCode)
				return nil
			})
		},
	}
	code.Flags().StringVar(&cdevice, "device", "", "node name of the device (default: <user>-N)")
	code.Flags().BoolVar(&cadmin, "admin", false, "the device gets an admin certificate")
	code.Flags().DurationVar(&cttl, "ttl", 24*time.Hour, "validity of the join code")

	var steams []string
	var sqf quotaFlags
	var clearQuota, spanel, canShare bool
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Change a user's teams (--team replaces them), quota (given flags; --clear-quota = the default) or --can-share",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &pb.PutUserRequest{Name: args[0], Panel: spanel}
			if cmd.Flags().Changed("can-share") {
				req.SetCanShare, req.CanShare = true, canShare
			}
			if cmd.Flags().Changed("team") {
				req.Teams, req.SetTeams = steams, true
			}
			if clearQuota {
				req.SetQuota = true
			} else if sqf.changed(cmd) {
				req.Quota, req.SetQuota = &sqf.q, true
			}
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Admin().PutUser(ctx, req)
				if err != nil {
					return err
				}
				fmt.Printf("%s: teams %s, quota %s, can share workers: %v\n", r.User.Name, strings.Join(r.User.Teams, ","),
					quotaString(r.User.Quota), r.User.CanShare)
				printPanel(r)
				return nil
			})
		},
	}
	set.Flags().StringArrayVar(&steams, "team", nil, "the user's teams (repeatable; replaces them)")
	sqf.bind(set)
	set.Flags().BoolVar(&clearQuota, "clear-quota", false, "drop the user's quota overrides")
	set.Flags().BoolVar(&spanel, "panel", false, "create or reset the user's web panel login")
	set.Flags().BoolVar(&canShare, "can-share", false, "may share own workers with other users, teams or everyone (longarm worker share); --can-share=false revokes")

	var asJSON bool
	ls := &cobra.Command{
		Use: "ls", Short: "List users with their number of devices and workers, teams, quotas and usage", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Admin().ListUsers(ctx, &pb.ListUsersRequest{})
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(r)
				}
				state := "off"
				if r.Tenancy {
					state = "on"
				}
				fmt.Printf("multi-tenancy %s; default quota: %s\n", state, quotaString(r.DefaultQuota))
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "USER\tSTATE\tTEAMS\tDEVICES\tWORKERS\tJOBS\tMEM GB\tQUOTA\tCAN SHARE")
				for _, u := range r.Users {
					st := "active"
					if u.Disabled {
						st = "disabled"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%.1f\t%s\t%v\n", u.Name, st, strings.Join(u.Teams, ","),
						u.DeviceCount, u.WorkerCount, u.Usage.GetJobs(), u.Usage.GetMemGb(), quotaString(u.Quota), u.CanShare)
				}
				return tw.Flush()
			})
		},
	}
	ls.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	toggle := func(use, short string, disabled bool) *cobra.Command {
		return &cobra.Command{
			Use: use + " <name>", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
					r, err := h.Admin().PutUser(ctx, &pb.PutUserRequest{Name: args[0], SetDisabled: true, Disabled: disabled})
					if err != nil {
						return err
					}
					fmt.Printf("%s %sd\n", r.User.Name, use)
					return nil
				})
			},
		}
	}

	var purge bool
	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a user: revoke the certificates of all its devices (--purge: workers also delete its jobs, data and OS account)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Admin().RemoveUser(ctx, &pb.RemoveUserRequest{Name: args[0], Purge: purge})
				if err != nil {
					return err
				}
				fmt.Printf("removed %s; revoked the certificates of %d device(s) and worker(s)\n", args[0], r.RevokedCount)
				if r.Purge {
					fmt.Println("workers delete the user's jobs, data and OS account (now, or when they next connect)")
				}
				return nil
			})
		},
	}
	remove.Flags().BoolVar(&purge, "purge", false, "also delete the user's data and OS account on every worker")

	c.AddCommand(add, code, set, ls, toggle("disable", "Refuse the user's calls (hub and workers) until enabled again", true),
		toggle("enable", "Allow a disabled user again", false), remove)
	return c
}

func teamAdminCmd() *cobra.Command {
	c := &cobra.Command{Use: "team", Short: "Teams: members may read jobs shared with the team (longarm run --share team)"}
	change := func(use, short string, add bool) *cobra.Command {
		return &cobra.Command{
			Use: use + " <team> <user>...", Short: short, Args: cobra.MinimumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				req := &pb.SetTeamRequest{Team: args[0]}
				if add {
					req.Add = args[1:]
				} else {
					req.Remove = args[1:]
				}
				return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
					r, err := h.Admin().SetTeam(ctx, req)
					if err != nil {
						return err
					}
					return printTeams(r)
				})
			},
		}
	}
	ls := &cobra.Command{
		Use: "ls", Short: "List teams and their members", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Admin().ListTeams(ctx, &pb.ListTeamsRequest{})
				if err != nil {
					return err
				}
				return printTeams(r)
			})
		},
	}
	c.AddCommand(change("add", "Add users to a team", true), change("rm", "Remove users from a team", false), ls)
	return c
}

func printTeams(r *pb.ListTeamsResponse) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TEAM\tMEMBERS")
	for _, t := range r.Teams {
		fmt.Fprintf(tw, "%s\t%s\n", t.Name, strings.Join(t.Members, ","))
	}
	return tw.Flush()
}

func whoamiCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "whoami",
		Short: "Show this machine's identity on the hub: certificate name, role, user, teams, quota and usage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Fleet().Whoami(ctx, &pb.WhoamiRequest{})
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(r)
				}
				fmt.Printf("name %s, role %s, user %s on hub %s (%s)\n", r.Name, r.Role, r.User, r.HubName, r.HubVersion)
				if r.Tenancy {
					fmt.Printf("teams: %s\nquota: %s\nusage: %d active jobs, %.1f GB reserved\n", strings.Join(r.Teams, ","),
						quotaString(r.Quota), r.Usage.GetJobs(), r.Usage.GetMemGb())
					fmt.Printf("can share own workers: %v\n", r.CanShare)
				}
				return nil
			})
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return c
}
