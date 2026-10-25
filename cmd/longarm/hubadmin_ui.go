package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// hubUICmds are the CLI equivalents of the web panel's job actions, which
// go through the worker's hub session (longarmd [hub] remote_jobs decides what
// is allowed): longarm admin kill | logs | run | toolchain-apply | links.
func hubUICmds() []*cobra.Command {
	var worker string
	kill := &cobra.Command{
		Use: "kill <job>", Short: "Kill a job through the hub (as the web panel does)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				fj, err := h.Admin().KillJob(ctx, &pb.FleetJobRequest{Id: args[0], Worker: worker})
				if err != nil {
					return err
				}
				fmt.Println(fj.Worker, fj.Job.GetSpec().GetId(), stateName(fj.Job.GetState()))
				return nil
			})
		},
	}
	kill.Flags().StringVar(&worker, "worker", "", "the job's worker (default: from the hub's index)")

	var follow bool
	var offset int64
	logs := &cobra.Command{
		Use: "logs <job>", Short: "Print a job's output through the hub (as the web panel does)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			h := s.hub()
			if h == nil {
				return errors.New("no hub configured")
			}
			st, err := h.Admin().JobLogs(cmd.Context(), &pb.HubLogsRequest{Id: args[0], Worker: worker, Offset: offset, Follow: follow})
			if err != nil {
				return err
			}
			for {
				c, err := st.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				os.Stdout.Write(c.Data)
			}
		},
	}
	logs.Flags().StringVar(&worker, "worker", "", "the job's worker (default: from the hub's index)")
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "follow until the job ends")
	logs.Flags().Int64Var(&offset, "offset", 0, "start at this byte offset (negative = from the end)")

	var name, cwd string
	var memGB float64
	var envs, require []string
	run := &cobra.Command{
		Use:   "run <worker|auto> -- command [args...]",
		Short: "Run a command job through the hub (no secrets, no repo upload; the worker needs [hub] remote_jobs = \"all\")",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := parseEnv(envs)
			if err != nil {
				return err
			}
			req, err := sched.ParseLabels(require, false)
			if err != nil {
				return err
			}
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				fj, err := h.Admin().SubmitJob(ctx, &pb.HubSubmitRequest{Worker: args[0], Argv: args[1:], Name: name, Cwd: cwd,
					Env: env, Require: req, MemGb: memGB})
				if err != nil {
					return err
				}
				fmt.Println(fj.Worker, fj.Job.GetSpec().GetId(), stateName(fj.Job.GetState()))
				return nil
			})
		},
	}
	run.Flags().StringVar(&name, "name", "", "job name")
	run.Flags().StringVar(&cwd, "cwd", "", "working directory on the worker")
	run.Flags().StringArrayVarP(&envs, "env", "e", nil, "K=V environment (repeatable)")
	run.Flags().StringArrayVar(&require, "require", nil, "label constraints KEY=VALUE[,KEY=VALUE...] for auto placement (repeatable)")
	run.Flags().Float64Var(&memGB, "mem-gb", 0, "memory reservation (default by type)")

	tcApply := &cobra.Command{
		Use: "toolchain-apply <node>", Short: "Ask a worker to reconcile its toolchain profile now", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				n, err := h.Admin().ToolchainApply(ctx, &pb.NodeRequest{Name: args[0]})
				if err != nil {
					return err
				}
				fmt.Printf("%s: toolchain apply requested (%s)\n", n.Name, n.GetToolchain().GetState())
				return nil
			})
		},
	}

	var asJSON bool
	links := &cobra.Command{
		Use: "links", Short: "Show how each client reaches each worker (reported by longarm daemon): leg, path, RTT", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Fleet().Links(ctx, &pb.LinksRequest{})
				if err != nil {
					return err
				}
				if asJSON {
					b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(r)
					fmt.Println(string(b))
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "CLIENT\tWORKER\tSTATE\tACTIVE\tRTT\tLEGS\tREPORTED")
				for _, rep := range r.Reports {
					for _, l := range rep.Links {
						var legs []string
						for _, g := range l.Legs {
							st := "down"
							if g.Up {
								st = fmt.Sprintf("%.0fms", g.RttMs)
							}
							legs = append(legs, g.Label+" "+st)
						}
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", rep.Client, l.Worker, l.State, l.Active,
							fmtRTT(l.RttMs), strings.Join(legs, ", "), ago(rep.Time, rep.Time == nil))
					}
				}
				return tw.Flush()
			})
		},
	}
	links.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return []*cobra.Command{kill, logs, run, tcApply, links}
}

func fmtRTT(ms float64) string {
	if ms <= 0 {
		return "-"
	}
	if ms < 10 {
		return fmt.Sprintf("%.1fms", ms)
	}
	return fmt.Sprintf("%.0fms", ms)
}
