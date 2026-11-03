package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/toolchain"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func workerToolchainCmd() *cobra.Command {
	var tools []string
	var clear, apply, asJSON bool
	c := &cobra.Command{
		Use:   "toolchain <worker> [--tools claude,copilot,uv,node@22] [--clear] [--apply] [--json]",
		Short: "Show or set desired tools on your worker (admins: fleet workers too)",
		Long: `Show desired tools and installed versions on your worker. --tools replaces
your desired tool list; the admin profile wins on conflicts. --clear removes
your desired list (not installed tools). Changes reconcile automatically when
idle; --apply requests a recheck now, without waiting for installation to finish.
Desired tools are stored even when the worker is offline.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			set := cmd.Flags().Changed("tools")
			if clear && set {
				return usagef("--tools and --clear are mutually exclusive")
			}
			specs, err := parseWorkerTools(tools)
			if err != nil {
				return usageErr(err)
			}
			req := &pb.WorkerToolchainRequest{Worker: args[0], Tools: specs, SetTools: set, Clear: clear, Apply: apply}
			return withHub(cmd, func(ctx context.Context, _ *session, h *hubclient.Conn) error {
				resp, err := h.Fleet().WorkerToolchain(ctx, req)
				if status.Code(err) == codes.Unimplemented {
					return usagef("this hub is too old for longarm worker toolchain (update it)")
				}
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(resp)
				}
				return writeWorkerToolchain(cmd.OutOrStdout(), resp)
			})
		},
	}
	c.Flags().StringSliceVar(&tools, "tools", nil, "replace desired tools (comma-separated, repeatable; NAME[@VERSION], default latest)")
	c.Flags().BoolVar(&clear, "clear", false, "clear owner desired tools, keeping the admin profile")
	c.Flags().BoolVar(&apply, "apply", false, "ask the worker to reconcile now (installs only when idle)")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return c
}

func writeWorkerToolchain(out io.Writer, resp *pb.WorkerToolchainResponse) error {
	state := resp.Toolchain.GetState()
	if state == "" {
		state = "unknown"
	}
	fmt.Fprintf(out, "%s: %s\n", resp.Worker, state)
	if resp.Toolchain == nil {
		fmt.Fprintln(out, "No toolchain report (worker may be offline or too old).")
	}
	if msg := resp.Toolchain.GetError(); msg != "" {
		fmt.Fprintln(out, msg)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tOWNER DESIRED\tEFFECTIVE\tINSTALLED")
	owner, desired := map[string]string{}, map[string]string{}
	for _, t := range resp.OwnerTools {
		owner[t.Name] = t.Version
	}
	for _, t := range resp.Desired.GetTools() {
		desired[t.Name] = t.Version
	}
	for _, name := range toolchain.KnownTools {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, dash(owner[name]), dash(desired[name]), dash(resp.Toolchain.GetVersions()[name]))
	}
	return tw.Flush()
}

func parseWorkerTools(items []string) ([]*pb.ToolSpec, error) {
	var out []*pb.ToolSpec
	seen := map[string]bool{}
	for _, item := range items {
		name, version, hasVersion := strings.Cut(strings.TrimSpace(item), "@")
		if !slices.Contains(toolchain.KnownTools, name) {
			return nil, fmt.Errorf("unknown tool %q (known: %s)", name, strings.Join(toolchain.KnownTools, ", "))
		}
		if seen[name] {
			return nil, fmt.Errorf("tool %q listed twice", name)
		}
		seen[name] = true
		if !hasVersion {
			version = "latest"
		}
		if !toolchain.ValidVersion(version) {
			return nil, fmt.Errorf("tool %s: invalid version %q", name, version)
		}
		out = append(out, &pb.ToolSpec{Name: name, Version: version})
	}
	return out, nil
}
