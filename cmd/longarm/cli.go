package main

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Command groups of `longarm --help`.
var commandGroups = []*cobra.Group{
	{ID: "start", Title: "Start here:"},
	{ID: "jobs", Title: "Jobs:"},
	{ID: "sessions", Title: "Sessions:"},
	{ID: "files", Title: "Files & ports:"},
	{ID: "workers", Title: "Your workers:"},
	{ID: "account", Title: "Account:"},
	{ID: "admin", Title: "Admin & troubleshooting:"},
}

const rootLong = `Longarm runs commands, jobs and interactive sessions on remote workers.

Getting started:
  longarm login                    sign this machine in (once)
  longarm ls                       the workers you may use
  longarm exec <worker> -- cmd     run a short command, output and exit code mirrored
  longarm run -- cmd               submit a job (the hub picks a worker); prints its id
  longarm logs last -f             follow your most recent job
  longarm attach <worker>          an interactive terminal (detach: Ctrl-] d)
  longarm doctor                   check sign-in, hub and workers; says what to fix

A job is named by its id, a unique prefix or suffix of it (the tray shows
the last 8 characters) or "last". Shell completion: longarm completion --help.
Reference: docs/usage.md.`

// newRootCmd builds the longarm command tree.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "longarm",
		Short:         "Longarm client: run commands and jobs on remote workers",
		Long:          rootLong,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}
	root.PersistentFlags().StringVar(&configDir, "config-dir", client.DefaultConfigDir(),
		"client config dir (workers.toml, tls/, ca/); env LONGARM_CONFIG_DIR")
	root.PersistentFlags().StringVar(&transportPolicy, "transport", "",
		"transport policy: race (default: all legs at once, first mTLS handshake wins) | tcp | tailscale | longnet (prefer); env LONGARM_TRANSPORT; persistent: longarm transport <policy>")
	root.PersistentFlags().BoolVar(&noHub, "no-hub", os.Getenv("LONGARM_NO_HUB") == "1", "ignore the hub; talk to workers directly (env LONGARM_NO_HUB=1)")
	root.PersistentFlags().BoolVar(&forceDial, "force-dial", os.Getenv("LONGARM_FORCE_DIAL") == "1",
		"dial a worker even when the hub reports it offline (env LONGARM_FORCE_DIAL=1)")
	root.AddGroup(commandGroups...)

	group := func(id string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = id
			root.AddCommand(c)
		}
	}
	group("start", loginCmd(), lsCmd(), execCmd(), runCmd(), doctorCmd())
	group("jobs", jobsCmd(), logsCmd(), waitCmd(), killCmd())
	group("sessions", attachCmd(), sessionCmd(), sessionsCmd())
	group("files", putCmd(), getCmd(), syncCmd(), tailCmd(), forwardCmd(), linkCmd(), sshCmd(), sshConfigCmd())
	group("workers", workerCmd(), infoCmd())
	group("account", whoamiCmd(), joinCmd(), leaveCmd(), logoutCmd(), versionCmd())
	group("admin", adminCmd(), diagCmd(), mcpCmd())
	// Plumbing: usable, not listed.
	for _, c := range []*cobra.Command{daemonCmd(), poolCmd(), proxyCmd(), transportCmd(), firewallCmd(), trustCmd(), telemetryCmd()} {
		c.Hidden = true
		root.AddCommand(c)
	}
	root.SetHelpCommandGroupID("admin")
	root.SetCompletionCommandGroupID("admin")
	root.CompletionOptions.HiddenDefaultCmd = false
	addCompletions(root)
	return root
}

// logoutCmd points at leave: signing out revokes this machine's sign-in,
// so it is not done under a name that sounds harmless.
func logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Sign this machine out: see `longarm leave` (revokes this machine's sign-in)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return usagef("%s", logoutMsg)
		},
	}
}

const logoutMsg = "there is no separate logout: `longarm leave` signs this machine out.\n" +
	"  Warning: it revokes this machine's sign-in (its certificate) at the hub and deletes it here;\n" +
	"  to use Longarm again you sign in anew with `longarm login`."

// ---------------------------------------------------------------- completion

// completionTimeout bounds what completion may ask the network (job ids,
// session names); worker names never leave the machine.
var completionTimeout = 3 * time.Second

// cachedWorkerNames are workers.toml's names and the hub's cached list
// (hub-cache.json): no network.
func cachedWorkerNames(dir string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if cfg, err := client.LoadConfig(dir); err == nil {
		for _, w := range cfg.Workers {
			add(w.Name)
		}
	}
	for _, n := range cachedNodesIn(dir) {
		if n.State != "revoked" && n.State != "pending" {
			add(n.Name)
		}
	}
	sort.Strings(out)
	return out
}

func filterPrefix(items []string, prefix string) []string {
	var out []string
	for _, it := range items {
		name, _, _ := strings.Cut(it, "\t")
		if strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
			out = append(out, it)
		}
	}
	return out
}

// completeWorkers completes worker names for the first n positional
// arguments (n < 0: every argument).
func completeWorkers(n int) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if n >= 0 && len(args) >= n {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return filterPrefix(cachedWorkerNames(configDir), toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// completeWorkerPath completes "<worker>:" for put / get / sync targets.
func completeWorkerColon(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if strings.Contains(toComplete, ":") {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, w := range cachedWorkerNames(configDir) {
		out = append(out, w+":")
	}
	return filterPrefix(out, toComplete), cobra.ShellCompDirectiveNoSpace | cobra.ShellCompDirectiveDefault
}

// recentJobs lists recent jobs for completion: the hub only (no worker
// dials), within completionTimeout.
var recentJobs = func(ctx context.Context) []*pb.FleetJob {
	quietHub = true
	s, err := openSession()
	if err != nil {
		return nil
	}
	defer s.close()
	h := s.optionalHub(ctx)
	if h == nil {
		return nil
	}
	rows, err := hubJobs(ctx, h, "", "", nil, 50)
	if err != nil {
		return nil
	}
	return rows
}

// jobCompletions are "id\tstate command" lines (newest first), after
// "last" when withLast.
func jobCompletions(rows []*pb.FleetJob, toComplete string, withLast bool) []string {
	var out []string
	if withLast {
		out = append(out, "last\tyour most recent job")
	}
	for _, fj := range rows {
		j := fj.GetJob()
		desc := stateName(j.GetState()) + " on " + fj.Worker + ": " + strings.Join(j.GetSpec().GetArgv(), " ")
		if len(desc) > 60 {
			desc = desc[:57] + "..."
		}
		out = append(out, j.GetSpec().GetId()+"\t"+desc)
	}
	return filterPrefix(out, toComplete)
}

// completeJobArgs completes "[<worker>] <job-id>": the first argument is a
// job id or a worker, the second a job id.
func completeJobArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) >= 2 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), completionTimeout)
	defer cancel()
	out := jobCompletions(recentJobs(ctx), toComplete, true)
	if len(args) == 0 {
		out = append(out, filterPrefix(cachedWorkerNames(configDir), toComplete)...)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// sessionNames lists session names for completion (of worker, else of every
// known worker), within completionTimeout.
var sessionNames = func(ctx context.Context, worker string) []string {
	quietHub = true
	s, err := openSession()
	if err != nil {
		return nil
	}
	defer s.close()
	var ws []string
	if worker != "" && worker != "auto" {
		ws = []string{worker}
	}
	rows, _, err := listSessions(ctx, s, ws, "", false, "")
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range rows {
		if !seen[r.s.Name] {
			seen[r.s.Name] = true
			out = append(out, r.s.Name+"\ton "+r.worker)
		}
	}
	return out
}

func completeSessionFlag(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	ctx, cancel := context.WithTimeout(cmd.Context(), completionTimeout)
	defer cancel()
	w := ""
	if len(args) > 0 {
		w = args[0]
	}
	if on := cmd.Flags().Lookup("on"); on != nil && on.Value.String() != "" {
		w = on.Value.String()
	}
	return filterPrefix(sessionNames(ctx, w), toComplete), cobra.ShellCompDirectiveNoFileComp
}

// addCompletions wires argument and flag completion into the tree.
func addCompletions(root *cobra.Command) {
	find := func(path ...string) *cobra.Command {
		c, _, err := root.Find(path)
		if err != nil || c == root {
			return nil
		}
		return c
	}
	set := func(fn cobra.CompletionFunc, paths ...string) {
		for _, p := range paths {
			if c := find(strings.Fields(p)...); c != nil && c.ValidArgsFunction == nil {
				c.ValidArgsFunction = fn
			}
		}
	}
	set(completeWorkers(1), "info", "attach", "forward", "tail", "proxy", "exec", "run", "sessions", "jobs",
		"worker share", "worker unshare", "worker remove", "worker label", "worker update", "session start", "session send", "session stop")
	set(completeWorkers(-1), "ls", "ssh-config")
	set(completeJobArgs, "logs", "wait", "kill")
	set(completeWorkerColon, "put", "get", "sync")
	flag := func(name string, fn cobra.CompletionFunc, paths ...string) {
		for _, p := range paths {
			if c := find(strings.Fields(p)...); c != nil && c.Flags().Lookup(name) != nil {
				_ = c.RegisterFlagCompletionFunc(name, fn)
			}
		}
	}
	flag("on", completeWorkers(-1), "exec", "run", "link create", "session start")
	flag("session", completeSessionFlag, "attach", "logs", "sessions", "exec", "run",
		"session send", "session stop")
	flag("id", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		ctx, cancel := context.WithTimeout(cmd.Context(), completionTimeout)
		defer cancel()
		return jobCompletions(recentJobs(ctx), toComplete, false), cobra.ShellCompDirectiveNoFileComp
	}, "jobs")
}
