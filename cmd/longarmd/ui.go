package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/lubobill1990/longarm/internal/longarmd"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// uiCmds are the CLI equivalents of longarm-tray's worker tab (local control
// API): limits, jobs, logs, kill, leave.
func uiCmds() []*cobra.Command {
	var dataDir string
	var asJSON bool
	client := func(cmd *cobra.Command) (pb.LocalWorkerClient, func(), error) {
		return localClient(dataDir)
	}

	var maxJobs int32
	var reserveGB float64
	limits := &cobra.Command{
		Use:   "limits",
		Short: "Show or set local limits: --max-jobs N (0 = none), --reserve-mem GB kept free for the desktop",
		Long: `Local limits are reported to the hub: its scheduler places at most
--max-jobs active jobs on this worker and keeps --reserve-mem GB of memory
free. Like drain, they are scheduler signals; an explicit "longarm run <worker>"
still runs. They persist across restarts (limits.json).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, closeFn, err := client(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			s, err := c.Status(ctx, &pb.LocalStatusRequest{})
			if err != nil {
				return err
			}
			l := s.Limits
			if l == nil {
				l = &pb.LocalLimits{}
			}
			if cmd.Flags().Changed("max-jobs") || cmd.Flags().Changed("reserve-mem") {
				if cmd.Flags().Changed("max-jobs") {
					l.MaxJobs = maxJobs
				}
				if cmd.Flags().Changed("reserve-mem") {
					l.ReservedMemGb = reserveGB
				}
				if s, err = c.SetLimits(ctx, l); err != nil {
					return err
				}
				l = s.Limits
			}
			if asJSON {
				b, _ := protojson.MarshalOptions{EmitUnpopulated: true, UseProtoNames: true}.Marshal(l)
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("max jobs         %s\nreserved memory  %.1f GB\n", map[bool]string{true: "no limit", false: fmt.Sprint(l.GetMaxJobs())}[l.GetMaxJobs() == 0], l.GetReservedMemGb())
			return nil
		},
	}
	limits.Flags().Int32Var(&maxJobs, "max-jobs", 0, "at most this many active jobs (0 = no limit)")
	limits.Flags().Float64Var(&reserveGB, "reserve-mem", 0, "GB of memory the scheduler keeps free")

	var states []string
	var limit int32
	jobsCmd := &cobra.Command{
		Use: "jobs", Short: "List this worker's jobs (local control API)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, closeFn, err := client(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			req := &pb.ListJobsRequest{Limit: limit}
			for _, s := range states {
				v, ok := pb.JobState_value["JOB_STATE_"+strings.ToUpper(s)]
				if !ok {
					return fmt.Errorf("unknown state %q", s)
				}
				req.States = append(req.States, pb.JobState(v))
			}
			r, err := c.ListJobs(cmd.Context(), req)
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(r)
				fmt.Println(string(b))
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATE\tEXIT\tBY\tCREATED\tCOMMAND")
			for _, j := range r.Jobs {
				exit := ""
				if j.HasExitCode {
					exit = fmt.Sprint(j.ExitCode)
				}
				name := j.Spec.Name
				if name == "" {
					name = strings.Join(j.Spec.Argv, " ")
				}
				if len(name) > 60 {
					name = name[:57] + "..."
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", j.Spec.Id, strings.ToLower(strings.TrimPrefix(j.State.String(), "JOB_STATE_")),
					exit, j.Spec.SubmittedBy, j.CreatedAt.AsTime().Local().Format("01-02 15:04:05"), name)
			}
			return tw.Flush()
		},
	}
	jobsCmd.Flags().StringSliceVar(&states, "state", nil, "only these states (running, done, failed, ...)")
	jobsCmd.Flags().Int32Var(&limit, "limit", 50, "newest N jobs (0 = all)")

	var follow bool
	var offset int64
	logs := &cobra.Command{
		Use: "logs <job>", Short: "Print a job's output (local control API)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, closeFn, err := client(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			st, err := c.JobLogs(cmd.Context(), &pb.LogsRequest{Id: args[0], Offset: offset, Follow: follow})
			if err != nil {
				return err
			}
			for {
				ch, err := st.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				os.Stdout.Write(ch.Data)
			}
		},
	}
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "follow until the job ends")
	logs.Flags().Int64Var(&offset, "offset", 0, "start at this byte offset (negative = from the end)")

	kill := &cobra.Command{
		Use: "kill <job>", Short: "Kill one of this worker's jobs (local control API)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, closeFn, err := client(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			j, err := c.KillJob(ctx, &pb.KillRequest{Id: args[0]})
			if err != nil {
				return err
			}
			fmt.Println(j.Spec.Id, strings.ToLower(strings.TrimPrefix(j.State.String(), "JOB_STATE_")))
			return nil
		},
	}

	var reason string
	var force, killJobs, purge bool
	leave := &cobra.Command{
		Use:   "leave",
		Short: "Unregister this worker from the hub (its certificates are revoked) and stop longarmd",
		Long: `Leave the fleet: the hub revokes this worker's certificates, then tls/,
hub.toml and the tailnet state are removed. A running longarmd does this through
its local control API and then stops; otherwise it is done directly.
Running jobs keep running in their shims (--kill-jobs ends them; longarmd must
be stopped). --purge removes the whole data dir. Join again with "longarmd join".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if c, closeFn, err := client(cmd); err == nil {
				defer closeFn()
				cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				_, serr := c.Status(cctx, &pb.LocalStatusRequest{})
				cancel()
				if serr == nil {
					if killJobs || purge {
						return fmt.Errorf("longarmd is running: stop it first for --kill-jobs / --purge")
					}
					r, err := c.Leave(ctx, &pb.LeaveRequest{Reason: reason, Force: force})
					if err != nil {
						return err
					}
					fmt.Println("left the fleet:", r.Message, "(longarmd stops)")
					return nil
				}
			}
			dir, err := filepath.Abs(dataDir)
			if err != nil {
				return err
			}
			msg, err := longarmd.Leave(ctx, dir, longarmd.LeaveOptions{Reason: reason, Force: force, KillJobs: killJobs, Purge: purge, Out: os.Stdout})
			if err != nil {
				return err
			}
			fmt.Println("left the fleet:", msg)
			return nil
		},
	}
	leave.Flags().StringVar(&reason, "reason", "", "why (recorded by the hub)")
	leave.Flags().BoolVar(&force, "force", false, "leave locally even if the hub is unreachable")
	leave.Flags().BoolVar(&killJobs, "kill-jobs", false, "kill running jobs and sessions first (longarmd stopped)")
	leave.Flags().BoolVar(&purge, "purge", false, "remove the whole data dir: jobs, mirrors, logs (longarmd stopped)")

	cmds := []*cobra.Command{limits, jobsCmd, logs, kill, leave}
	for _, c := range cmds {
		c.Flags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory of the longarmd; env LONGARM_DATA_DIR")
	}
	limits.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	jobsCmd.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return cmds
}
