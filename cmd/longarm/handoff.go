package main

// longarm handoff / agent-sessions (docs/usage.md#hand-off-an-agent-session):
// move an in-progress Claude Code or Copilot CLI conversation, with the
// code it works on (uncommitted and untracked changes included), between
// this machine and workers, and resume it there. Orchestrated entirely by
// this client over the existing worker-direct channels (Files, Exec,
// Workspace, Sessions); the hub never sees a transcript.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Environment of every session a longarmd (with the "session-env"
// feature) starts: the worker's name and the session's, so that a longarm
// run inside it knows where it is (handoff's default --from).
const (
	envLongarmWorker  = "LONGARM_WORKER"
	envLongarmSession = "LONGARM_SESSION"
)

// maxRelayBundle caps a bundle of commits relayed from a worker.
const maxRelayBundle = 16 << 30

// defaultHandoffMax caps what a handoff moves of a session (transcript and state).
const defaultHandoffMax = "64M"

// handoffJSON is the output of handoff --json.
type handoffJSON struct {
	Agent       string `json:"agent"`
	SessionID   string `json:"sessionId"`
	From        string `json:"from"`
	To          string `json:"to"`
	SourceDir   string `json:"sourceProjectDir"`
	SourceTop   string `json:"sourceWorktree"`
	Worktree    string `json:"worktree"`
	Cwd         string `json:"cwd"`
	Branch      string `json:"branch"`
	Session     string `json:"session,omitempty"` // the longarm session on a worker target
	Snapshot    string `json:"snapshot"`
	SnapshotRef string `json:"snapshotRef"`
	Base        string `json:"base"`
	Files       int    `json:"files"`
	Bytes       int64  `json:"bytes"`
	RemoteFirst bool   `json:"remoteFirst"`
	// how the code went: upload (local -> worker), direct (worker -> worker),
	// relay (worker -> worker through this client), bundle (worker -> local),
	// remote (a temporary branch of the remote)
	Transfer     string   `json:"transfer"`
	RelayedBytes int64    `json:"relayedBytes"` // commit bytes this client relayed between workers
	TempBranch   string   `json:"tempBranch,omitempty"`
	Resume       []string `json:"resume"`
	Warnings     []string `json:"warnings"`
}

type handoffOpts struct {
	to, from, agent, id, name, worktreeDir, localRepo, gitAuth, sourceSession, repoPath string
	latest, noAttach, asJSON, force, fromImplicit                                       bool
	idle, idleTimeout, grace                                                            time.Duration
	maxBytes                                                                            int64
	via                                                                                 string // remote | relay | auto
	viaThreshold                                                                        int64
	allowSecrets, keepBranch, cleanup                                                   bool
}

func handoffCmd() *cobra.Command {
	var o handoffOpts
	var maxSize, viaThreshold string
	c := &cobra.Command{
		Use:   "handoff --to <worker|local> [--from <worker|local>] [--agent claude|copilot] (--session ID | --latest) [--name NAME] [--no-attach] [--json]",
		Short: "Move a Claude Code / Copilot CLI conversation and its code (uncommitted and untracked too) to another machine and resume it there",
		Long: `Move an in-progress Claude Code or Copilot CLI conversation between this
machine ("local") and workers - local -> worker, worker -> worker, worker
-> local - with the code state it works on, and resume it on the target
(docs/usage.md#hand-off-an-agent-session):

 1. find the session (--session ID, or --latest: the newest one of the
    current checkout locally, of --repo-path / the whole machine on a
    worker; longarm agent-sessions lists them);
 2. quiesce the source: a longarm session running the agent is stopped
    once it was idle for --idle (no input or output); a local agent that
    still has the session open is refused (exit 75: exit it first);
 3. snapshot the source worktree (HEAD + uncommitted + untracked, ignored
    files left out) as a commit on refs/longarm/handoff/<id> - the
    user's branch, index and files are not touched - and make a new
    worktree of it on the target on branch handoff/<id>, with the changes
    uncommitted again (a worker: <home>/longarm-handoff/<repo>-<id>, the
    base fetched by the worker with --git-auth forward; worker -> worker:
    only the delta is relayed through this machine; local: a new git
    worktree next to your checkout or under --worktree-dir);
 4. copy the transcript and its state to the target, absolute paths
    rewritten from the source worktree to the target's (same session id),
    and mark the source copy "handed off" (a sidecar file; the original
    stays as a backup);
 5. resume: on a worker in a longarm session (--name, default
    handoff-<id>) running "claude --resume <id>" / "copilot --resume=<id>",
    then attach (--no-attach: don't); locally the agent runs in this
    terminal.

Running processes (tool calls, background shells, MCP servers) are not
carried over: the agent starts them anew on the target.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n, err := parseSize(strings.NewReplacer("iB", "", "IB", "", "ib", "").Replace(maxSize)) // 64MiB = 64M
			if err != nil || n <= 0 {
				return usagef("--max-size %q: want a size such as 64M or 1G", maxSize)
			}
			o.maxBytes = n
			if o.cleanup {
				return runHandoffCleanup(cmd.Context(), o)
			}
			if err := resolveVia(&o, cmd.Flags().Changed("via"), viaThreshold, cmd.Flags().Changed("via-threshold")); err != nil {
				return err
			}
			return runHandoff(cmd.Context(), o)
		},
	}
	f := c.Flags()
	f.StringVar(&o.to, "to", "", "target: a worker or local (required)")
	f.StringVar(&o.from, "from", "", "source: a worker or local (default: the worker of the longarm session this runs in, else local)")
	f.StringVar(&o.agent, "agent", "", "claude or copilot (default: whichever has the session)")
	f.StringVar(&o.id, "session", "", "the agent's session id (longarm agent-sessions lists them)")
	f.BoolVar(&o.latest, "latest", false, "the most recently active session (locally: of the current checkout)")
	f.StringVar(&o.repoPath, "repo-path", "", "with --latest: only sessions whose project directory is in this directory (of the source)")
	f.StringVar(&o.name, "name", "", "the longarm session on a worker target (default handoff-<id>)")
	f.BoolVar(&o.noAttach, "no-attach", false, "do not attach (worker) / do not run the agent here (local): print how to resume")
	f.BoolVar(&o.asJSON, "json", false, "print the result as JSON (implies --no-attach)")
	f.StringVar(&o.worktreeDir, "worktree-dir", "", "--to local: directory for the new worktree (default: next to the checkout); env LONGARM_HANDOFF_DIR")
	f.StringVar(&o.localRepo, "local-repo", "", "--from <worker> --to local: your local checkout of the repository (default: the current directory's)")
	f.StringVar(&o.gitAuth, "git-auth", gitAuthNone, "forward: a worker target fetches the base commits from the remote with your credential (see run --git-auth); only the delta is shipped")
	f.StringVar(&o.sourceSession, "source-session", "", "--from <worker>: the longarm session that runs the agent (default: found by its directory and command)")
	f.DurationVar(&o.idle, "idle", 5*time.Second, "a longarm source session counts as idle after this long without input or output")
	f.DurationVar(&o.idleTimeout, "idle-timeout", 10*time.Minute, "give up (exit 75) when the source session is not idle within this long")
	f.DurationVar(&o.grace, "grace", 10*time.Second, "session stop --grace for the source session")
	f.StringVar(&maxSize, "max-size", defaultHandoffMax, "refuse sessions whose transcript and state are larger")
	f.BoolVar(&o.force, "force", false, "hand off even if the target's agent is older than the source's")
	f.StringVar(&o.via, "via", defaultHandoffVia, "how the code travels: relay (longarm's channels; worker -> worker directly), remote (a temporary branch of the repository's remote) or auto (relay below --via-threshold, else remote); [handoff] via in workers.toml")
	f.StringVar(&viaThreshold, "via-threshold", defaultViaThreshold, "--via auto: objects the target lacks from this size on go through the remote; [handoff] via_threshold")
	f.BoolVar(&o.allowSecrets, "allow-secrets", false, "--via remote: push even when the secret check finds credentials in changed or untracked files")
	f.BoolVar(&o.keepBranch, "keep-branch", false, "--via remote: keep the temporary branch on the remote")
	f.BoolVar(&o.cleanup, "cleanup", false, "list and delete your stale temporary branches (longarm/handoff/<you>/*) of the current checkout's remote")
	return c
}

// warnf prints a warning and keeps it for --json.
type warnings []string

func (w *warnings) add(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	*w = append(*w, msg)
	fmt.Fprintln(os.Stderr, "longarm: warning:", msg)
}

func notef(format string, a ...any) { fmt.Fprintf(os.Stderr, "longarm: "+format+"\n", a...) }

func tempFailf(format string, a ...any) error {
	return &codedError{code: exitTempFail, err: fmt.Errorf(format, a...)}
}

func noPermf(format string, a ...any) error {
	return &codedError{code: exitNoPerm, err: fmt.Errorf(format, a...)}
}

func runHandoff(ctx context.Context, o handoffOpts) error {
	if o.asJSON {
		o.noAttach = true
	}
	envWorker, envSession := os.Getenv(envLongarmWorker), os.Getenv(envLongarmSession)
	if o.to == "" {
		return usagef("--to <worker|local> is required")
	}
	if o.from == "" {
		o.fromImplicit = true
		o.from = localMachine
		if envWorker != "" {
			o.from = envWorker
		}
	}
	if o.to == o.from {
		return usagef("--from and --to are both %s", o.to)
	}
	if envWorker != "" && o.to == localMachine {
		return usagef("--to local inside a longarm session on %s would be that worker itself; run handoff on your client", envWorker)
	}
	if (o.id == "") == !o.latest {
		return usagef("give --session <agent session id> or --latest")
	}
	if err := checkGitAuthFlag(o.gitAuth, "-"); err != nil {
		return err
	}
	if o.worktreeDir == "" {
		o.worktreeDir = os.Getenv("LONGARM_HANDOFF_DIR")
	}
	var adapters []agentAdapter
	if o.agent != "" {
		ad, err := adapterFor(o.agent)
		if err != nil {
			return err
		}
		adapters = []agentAdapter{ad}
	} else {
		adapters = allAdapters
	}

	s, err := openSession()
	if err != nil {
		return err
	}
	defer s.close()
	src, err := openEndpoint(ctx, s, o.from)
	if err != nil {
		return err
	}
	defer src.close()
	dst, err := openEndpoint(ctx, s, o.to)
	if err != nil {
		return err
	}
	defer dst.close()
	var nodes []*pb.NodeInfo
	if !src.local() || !dst.local() {
		nodes, _ = s.hubNodes(ctx, false)
	}
	nodeOf := func(name string) *pb.NodeInfo {
		for _, n := range nodes {
			if n.Name == name {
				return n
			}
		}
		return nil
	}
	if n := nodeOf(src.name); n != nil && n.AdminWorker && o.fromImplicit {
		return usagef("%s is an admin worker: name it with --from to hand off from it", src.name)
	}

	// 1. locate
	ad, sess, err := locateSession(ctx, src, adapters, o)
	if err != nil {
		return err
	}
	notef("%s session %s (%s) in %s on %s", ad.name(), sess.ID, humanSize(sess.Size), sess.ProjectDir, src.name)

	// who may touch it
	var host *pb.Session
	if !src.local() {
		if host, err = findHostSession(ctx, src, ad, sess, o.sourceSession); err != nil {
			return err
		}
		if host != nil && host.Name == envSession && src.name == envWorker {
			return usagef("the agent runs in longarm session %s, the one this handoff runs in: stopping it would stop the handoff; run it from another terminal", host.Name)
		}
		if host != nil && src.callerUser != "" && host.Owner != "" && host.Owner != src.callerUser {
			return noPermf("longarm session %s on %s belongs to %s: you can only hand off your own sessions", host.Name, src.name, host.Owner)
		}
		if host == nil && !filesAreMine(src, nodeOf(src.name)) {
			return noPermf("%s keeps every user's agent state in one account (shared isolation) and the agent runs in no longarm session of yours there: "+
				"only sessions you own can be handed off from it", src.name)
		}
	}

	// the code's checkout (read-only so far)
	srcRepo, err := openHoRepo(ctx, src, sess.ProjectDir)
	if err != nil {
		return err
	}

	// 2. preflight
	var warn warnings
	if !dst.local() && !filesAreMine(dst, nodeOf(dst.name)) {
		warn.add("%s keeps the agent state of all its users in one account (shared isolation): other users' jobs there can read the transcript", dst.name)
	}
	dstCfg := ad.configDir(ctx, dst)
	if err := preflight(ctx, ad, src, dst, dstCfg, sess, srcRepo, o.force, &warn); err != nil {
		return err
	}
	if total := sessionBytes(sess); total > o.maxBytes {
		return usagef("the session's transcript and state are %s, over --max-size %s", humanSize(total), humanSize(o.maxBytes))
	}
	if err := checkSessionNames(ad, dst, sess); err != nil {
		return err
	}
	if !src.local() && !dst.local() {
		notef("relaying worker -> worker through this machine: keep it online until the handoff is done")
	}

	// 3. quiesce the source
	if host != nil {
		if err := quiesce(ctx, src, host, o); err != nil {
			return err
		}
	} else if who := ad.inUse(ctx, src, sess); who != "" {
		return tempFailf("%s on %s still has session %s open: %s, then run handoff again", who, src.name, sess.ID, ad.exitHint())
	}
	// it may have grown until it stopped
	if sess, err = relist(ctx, ad, src, sess); err != nil {
		return err
	}
	if total := sessionBytes(sess); total > o.maxBytes {
		return usagef("the session's transcript and state are %s, over --max-size %s", humanSize(total), humanSize(o.maxBytes))
	}
	if err := checkSessionNames(ad, dst, sess); err != nil {
		return err
	}

	// 4. the code
	short := shortID(sess.ID)
	hid := short + "-" + strconv.FormatInt(time.Now().Unix(), 36)
	ref := "refs/longarm/handoff/" + hid
	snap, base, err := srcRepo.snapshot(ctx, ref)
	if err != nil {
		return err
	}
	notef("snapshot %s of %s (%s) on %s", snap[:12], srcRepo.top, ref, src.name)
	out := handoffJSON{Agent: ad.name(), SessionID: sess.ID, From: src.name, To: dst.name, SourceDir: sess.ProjectDir,
		SourceTop: srcRepo.top, Snapshot: snap, SnapshotRef: ref, Base: base}
	code, err := moveCode(ctx, s, o, src, dst, srcRepo, ref, hid, short, snap, base)
	if err != nil {
		return err
	}
	defer code.temp.cleanup(context.WithoutCancel(ctx)) // also when a later step fails
	dstTop, branch := code.top, code.branch
	out.Worktree, out.Branch, out.RemoteFirst, out.Transfer, out.RelayedBytes = dstTop, branch, code.remoteFirst, code.transfer, code.relayed
	if code.temp != nil {
		out.TempBranch = code.temp.branch
	}
	dstCwd := dstTop
	if p := strings.TrimSuffix(srcRepo.prefix, "/"); p != "" {
		dstCwd = dst.join(dstTop, p)
	}
	out.Cwd = dstCwd
	notef("code at %s:%s (branch %s)", dst.name, dstTop, branch)

	// 5. the session
	nfiles, nbytes, err := moveSessionFiles(ctx, ad, src, dst, dstCfg, sess, srcRepo, dstTop, dstCwd, o.maxBytes)
	if err != nil {
		return err
	}
	out.Files, out.Bytes = nfiles, nbytes
	if o.name == "" && !dst.local() {
		o.name = "handoff-" + short
	}
	out.Session = o.name
	mark := handoffMark{To: dst.name, At: time.Now().UTC(), TargetWorktree: dstTop, TargetCwd: dstCwd, TargetSession: out.Session,
		Snapshot: snap, SnapshotRef: ref, Base: base, By: "longarm " + version.Version}
	mb, _ := json.MarshalIndent(mark, "", "  ")
	if err := src.writeFile(ctx, sess.marker, mb); err != nil {
		warn.add("could not mark the source copy as handed off (%s): %v", sess.marker, err)
	}

	// 6. resume
	argv := ad.resumeArgv(sess.ID)
	out.Resume = argv
	out.Warnings = warn
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	if dst.local() {
		if o.noAttach {
			return printHandoff(o, out, fmt.Sprintf("cd %s && %s", quoteArg(dstCwd), strings.Join(argv, " ")))
		}
		code.temp.cleanup(ctx) // the target has the code
		printHandoff(o, out, "")
		c := exec.CommandContext(ctx, argv[0], argv[1:]...)
		c.Dir, c.Stdin, c.Stdout, c.Stderr = dstCwd, os.Stdin, os.Stdout, os.Stderr
		err := c.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &exitError{code: ee.ExitCode()}
		}
		return err
	}
	if err := needFeature(ctx, dst.conn, jobs.FeatureSessionStart, "handoff (session start)"); err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	resp, err := dst.conn.Sessions().Start(sctx, &pb.StartSessionRequest{Name: o.name, Argv: argv, Cwd: dstCwd})
	cancel()
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return usagef("a session named %s runs on %s already: give another --name (the code and transcript are in place; resume with: longarm session start %s -s <name> --cwd %s -- %s)",
				o.name, dst.name, dst.name, quoteArg(dstCwd), strings.Join(argv, " "))
		}
		return tooOld(err, dst.conn, "session start")
	}
	if ss := resp.Session; terminalState(ss.State) {
		return sessionEnded(ss)
	}
	code.temp.cleanup(ctx) // resumed there
	attachCmd := fmt.Sprintf("longarm attach %s -s %s", dst.name, o.name)
	if o.noAttach {
		return printHandoff(o, out, attachCmd)
	}
	printHandoff(o, out, "")
	return runAttach(ctx, dst.conn, &pb.AttachOpen{Name: o.name, NoCreate: true})
}

func printHandoff(o handoffOpts, out handoffJSON, next string) error {
	if o.asJSON {
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	where := out.To
	if out.Session != "" {
		where += " (longarm session " + out.Session + ")"
	}
	fmt.Printf("%s session %s handed off %s -> %s: %s\n", out.Agent, out.SessionID, out.From, where, out.Cwd)
	if next != "" {
		fmt.Printf("resume: %s\n", next)
	}
	return nil
}

func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t\"'") {
		return strconv.Quote(s)
	}
	return s
}

func shortID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	return strings.ToLower(id)
}

func sessionBytes(s *agentSession) int64 {
	var n int64
	for _, f := range s.files {
		n += f.size
	}
	return n
}

// filesAreMine: agent state in the worker account's home is the caller's
// own - locally, with per-user isolation (the user's own account), on a
// worker the caller owns, or without a hub (workers.toml: one person's
// workers). A fleet worker or one shared with the caller keeps every
// user's state in one account (shared isolation).
func filesAreMine(ep *hoEndpoint, node *pb.NodeInfo) bool {
	if ep.local() || ep.isolation == "per-user" || ep.callerUser == "" || node == nil {
		return true
	}
	return node.Owner == ep.callerUser
}

// locateSession finds the session to hand off on the source.
func locateSession(ctx context.Context, src *hoEndpoint, adapters []agentAdapter, o handoffOpts) (agentAdapter, *agentSession, error) {
	filter := o.repoPath
	if filter == "" && o.latest && src.local() {
		filter, _ = os.Getwd()
		if r, err := openHoRepo(ctx, src, filter); err == nil {
			filter = r.top
		}
	}
	type found struct {
		ad agentAdapter
		s  *agentSession
	}
	var all []found
	for _, ad := range adapters {
		cfg := ad.configDir(ctx, src)
		list, err := ad.list(ctx, src, cfg, o.id, false)
		if err != nil {
			return nil, nil, fmt.Errorf("%s sessions on %s: %w", ad.name(), src.name, err)
		}
		for _, s := range list {
			if s.ProjectDir == "" || (filter != "" && !src.within(s.ProjectDir, filter)) {
				continue
			}
			all = append(all, found{ad, s})
		}
	}
	if len(all) == 0 {
		switch {
		case o.id != "":
			return nil, nil, notFoundf("no agent session %s on %s", o.id, src.name)
		case filter != "":
			return nil, nil, notFoundf("no agent session in %s on %s", filter, src.name)
		}
		return nil, nil, notFoundf("no agent session on %s", src.name)
	}
	// not handed off already, newest first
	sort.SliceStable(all, func(i, j int) bool {
		if (all[i].s.HandedOff == nil) != (all[j].s.HandedOff == nil) {
			return all[i].s.HandedOff == nil
		}
		return all[i].s.Modified.After(all[j].s.Modified)
	})
	pick := all[0]
	if pick.s.HandedOff != nil {
		return nil, nil, usagef("session %s on %s was handed off to %s at %s already (resume it there, or hand it off from there)",
			pick.s.ID, src.name, pick.s.HandedOff.To, pick.s.HandedOff.At.Local().Format(time.RFC3339))
	}
	if o.id != "" && len(all) > 1 && all[1].s.HandedOff == nil {
		notef("session %s is in several project directories on %s; taking the most recent (%s)", o.id, src.name, pick.s.ProjectDir)
	}
	full, err := relist(ctx, pick.ad, src, pick.s)
	return pick.ad, full, err
}

// relist lists the session again with every file a handoff moves.
func relist(ctx context.Context, ad agentAdapter, ep *hoEndpoint, s *agentSession) (*agentSession, error) {
	list, err := ad.list(ctx, ep, s.cfg, s.ID, true)
	if err != nil {
		return nil, err
	}
	for _, l := range list {
		if l.transcript == s.transcript {
			return l, nil
		}
	}
	return nil, notFoundf("session %s vanished from %s", s.ID, ep.name)
}

// findHostSession finds the running longarm session on the source worker
// that runs the agent session: --source-session, else one whose command
// names the session id, else the one running the agent in its directory.
func findHostSession(ctx context.Context, ep *hoEndpoint, ad agentAdapter, s *agentSession, want string) (*pb.Session, error) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if want != "" {
		ss, err := ep.conn.Sessions().Get(c, &pb.GetSessionRequest{Name: want})
		if status.Code(err) == codes.NotFound {
			return nil, notFoundf("no session %s on %s", want, ep.name)
		}
		if err != nil {
			return nil, err
		}
		if terminalState(ss.State) {
			return nil, nil
		}
		return ss, nil
	}
	resp, err := ep.conn.Sessions().List(c, &pb.ListSessionsRequest{})
	if err != nil {
		return nil, err
	}
	var byID, byDir []*pb.Session
	for _, ss := range resp.Sessions {
		if terminalState(ss.State) || len(ss.Argv) == 0 {
			continue
		}
		cmdline := strings.Join(ss.Argv, " ")
		prog := strings.ToLower(filepath.Base(strings.ReplaceAll(ss.Argv[0], `\`, "/")))
		prog = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(prog, ".exe"), ".cmd"), ".ps1")
		switch {
		case strings.Contains(cmdline, s.ID):
			byID = append(byID, ss)
		case prog == ad.name() && (ep.samePath(ss.Dir, s.ProjectDir) || ep.samePath(ss.Cwd, s.ProjectDir)):
			byDir = append(byDir, ss)
		}
	}
	for _, cands := range [][]*pb.Session{byID, byDir} {
		switch len(cands) {
		case 0:
			continue
		case 1:
			return cands[0], nil
		}
		var names []string
		for _, ss := range cands {
			names = append(names, ss.Name)
		}
		return nil, usagef("several longarm sessions on %s may run this agent session (%s): name it with --source-session", ep.name, strings.Join(names, ", "))
	}
	return nil, nil
}

// quiesce waits until the source session had no input or output for
// o.idle (as observed from here: no clock comparison with the worker),
// then stops it (session stop --grace).
func quiesce(ctx context.Context, ep *hoEndpoint, host *pb.Session, o handoffOpts) error {
	notef("waiting until longarm session %s on %s is idle for %s, then stopping it", host.Name, ep.name, o.idle)
	type activity struct {
		logSize, inputSeq int64
		out, in           int64 // UnixNano
	}
	act := func(ss *pb.Session) activity {
		return activity{ss.LogSize, int64(ss.InputSeq), ss.LastOutput.AsTime().UnixNano(), ss.LastInput.AsTime().UnixNano()}
	}
	deadline := time.Now().Add(o.idleTimeout)
	cur, since := act(host), time.Now()
	for {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		ss, err := ep.conn.Sessions().Get(c, &pb.GetSessionRequest{Name: host.Name})
		cancel()
		if err != nil && !client.Retryable(err) {
			return err
		}
		if err == nil {
			if terminalState(ss.State) {
				return nil
			}
			if a := act(ss); a != cur {
				cur, since = a, time.Now()
			}
		}
		if time.Since(since) >= o.idle {
			break
		}
		if time.Now().After(deadline) {
			return tempFailf("longarm session %s on %s was not idle for %s within %s: try again when the agent waits for input", host.Name, ep.name, o.idle, o.idleTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(o.idle/4+50*time.Millisecond, time.Second)):
		}
	}
	c, cancel := context.WithTimeout(ctx, o.grace+time.Minute)
	defer cancel()
	ss, err := ep.conn.Sessions().Stop(c, &pb.StopSessionRequest{Name: host.Name, Grace: durationpb.New(o.grace)})
	if err != nil {
		return tooOld(err, ep.conn, "session stop")
	}
	notef("stopped longarm session %s on %s (%s)", ss.Name, ep.name, stateName(ss.State))
	return nil
}

// preflight checks the target before anything changes: the agent is
// installed (exit 2) and not older than the source's (exit 2 unless
// --force), logged in, and has the MCP servers the session used (warnings).
func preflight(ctx context.Context, ad agentAdapter, src, dst *hoEndpoint, dstCfg string, sess *agentSession, srcRepo *hoRepo, force bool, warn *warnings) error {
	out, err := dst.run(ctx, "", nil, nil, 45*time.Second, ad.name(), "--version")
	switch {
	case errors.Is(err, errNoProgram):
		return usagef("%s is not installed on %s (or not on longarmd's PATH there)", ad.name(), dst.name)
	case err != nil:
		warn.add("%s --version on %s: %v", ad.name(), dst.name, err)
	default:
		have := versionRE.FindString(string(out))
		if sess.Version != "" && olderVersion(have, sess.Version) {
			if !force {
				return usagef("%s on %s is %s, older than the %s that wrote the session: update it there (or --force)", ad.name(), dst.name, have, sess.Version)
			}
			warn.add("%s on %s is %s, older than the session's %s", ad.name(), dst.name, have, sess.Version)
		}
	}
	if in, known := ad.loggedIn(ctx, dst, dstCfg); known && !in {
		warn.add("%s on %s does not look logged in: log in there before resuming", ad.name(), dst.name)
	}
	srcM := ad.mcpServers(ctx, src, sess.cfg, sess.ProjectDir)
	have := map[string]bool{}
	for _, n := range ad.mcpServers(ctx, dst, dstCfg, "") {
		have[n] = true
	}
	if ad.name() == "claude" { // .mcp.json travels with the code
		if b, err := src.readSmall(ctx, src.join(srcRepo.top, ".mcp.json"), 4<<20); err == nil {
			var mj struct {
				MCPServers map[string]json.RawMessage `json:"mcpServers"`
			}
			if json.Unmarshal(b, &mj) == nil {
				for n := range mj.MCPServers {
					have[n] = true
				}
			}
		}
	}
	var missing []string
	for _, n := range srcM {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		warn.add("MCP servers configured on %s but not on %s: %s (add them there for the same tools)", src.name, dst.name, strings.Join(missing, ", "))
	}
	if !dst.local() && !strings.Contains(strings.Join(dst.features, ","), jobs.FeatureSessionStart) {
		return usagef("handoff: worker %s (longarmd %s) does not support session start; update longarmd", dst.name, dst.version)
	}
	return nil
}

// freeWorkerDir picks longarm-handoff/<repo>-<short> (or -N) under the
// worker's home.
func freeWorkerDir(ctx context.Context, ep *hoEndpoint, key, short string) (string, error) {
	base := ep.join(ep.home, "longarm-handoff", key+"-"+short)
	p := base
	for i := 2; ; i++ {
		_, ok, err := ep.stat(ctx, p)
		if err != nil {
			return "", err
		}
		if !ok {
			return p, nil
		}
		p = fmt.Sprintf("%s-%d", base, i)
	}
}

// moveSessionFiles copies the session's files to the target (through a
// private temporary directory here, removed afterwards), with the paths
// rewritten; what the target has of the session already is moved to
// <config>/longarm-handoff-backup/ first, never overwritten.
func moveSessionFiles(ctx context.Context, ad agentAdapter, src, dst *hoEndpoint, dstCfg string, sess *agentSession, srcRepo *hoRepo,
	dstTop, dstCwd string, limit int64) (int, int64, error) {
	dstProject, err := ad.projectBase(dst, dstCfg, dstCwd)
	if err != nil {
		return 0, 0, err
	}
	maps := []pathMap{{from: srcRepo.top, to: dstTop, fromWin: src.windows(), toWin: dst.windows()}}
	// the project directory as the agent recorded it (may differ from git's spelling: symlinks, case)
	if p := strings.TrimSuffix(srcRepo.prefix, "/"); p != "" {
		if rel := src.native(p); len(sess.ProjectDir) > len(rel) && strings.HasSuffix(strings.ToLower(sess.ProjectDir), strings.ToLower(rel)) {
			maps = append(maps, pathMap{from: strings.TrimRight(sess.ProjectDir[:len(sess.ProjectDir)-len(rel)], `/\`), to: dstTop,
				fromWin: src.windows(), toWin: dst.windows()})
		}
	} else {
		maps = append(maps, pathMap{from: sess.ProjectDir, to: dstTop, fromWin: src.windows(), toWin: dst.windows()})
	}
	if dstProject != "" {
		maps = append(maps, pathMap{from: src.dir(sess.transcript), to: dstProject, fromWin: src.windows(), toWin: dst.windows()})
	}
	maps = append(maps, pathMap{from: sess.cfg, to: dstCfg, fromWin: src.windows(), toWin: dst.windows()})
	rw := newPathRewriter(maps)
	baseDir := func(b string) string {
		if b == "project" {
			return dstProject
		}
		return dstCfg
	}
	// the names come from the source: refuse the session before anything
	// is written if any could land outside its own files on the target
	if err := checkSessionFiles(ad, dst, sess, baseDir); err != nil {
		return 0, 0, err
	}
	// keep what the target has of this session id
	stamp := time.Now().UTC().Format("20060102T150405Z")
	for _, t := range ad.targetTops(sess.ID) {
		if t.base == "project" && dstProject == "" {
			continue
		}
		p := dst.join(baseDir(t.base), t.rel)
		if _, ok, err := dst.stat(ctx, p); err != nil {
			return 0, 0, err
		} else if ok {
			bk := dst.join(dstCfg, "longarm-handoff-backup", sess.ID+"-"+stamp, t.base, t.rel)
			if err := dst.rename(ctx, p, bk); err != nil {
				return 0, 0, fmt.Errorf("back up %s on %s: %w", p, dst.name, err)
			}
			notef("kept the earlier %s on %s as %s", p, dst.name, bk)
		}
	}
	for _, f := range sess.files { // single files outside those subtrees (Claude's todos)
		if _, isTop := topOf(ad.targetTops(sess.ID), f); isTop || (f.base == "project" && dstProject == "") {
			continue
		}
		p := dst.join(baseDir(f.base), f.rel)
		if _, ok, err := dst.stat(ctx, p); err != nil {
			return 0, 0, err
		} else if ok {
			bk := dst.join(dstCfg, "longarm-handoff-backup", sess.ID+"-"+stamp, f.base, f.rel)
			if err := dst.rename(ctx, p, bk); err != nil {
				return 0, 0, fmt.Errorf("back up %s on %s: %w", p, dst.name, err)
			}
		}
	}
	tmp, err := os.MkdirTemp("", "longarm-handoff-")
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(tmp) // transcripts are sensitive: no copy stays here
	var total, got int64
	rewritten := 0
	links := newLinkChecker(dst, dstCfg)
	for i, f := range sess.files {
		if f.base == "project" && dstProject == "" {
			continue
		}
		target := dst.join(baseDir(f.base), f.rel)
		if err := links.check(ctx, target); err != nil {
			return 0, 0, err
		}
		// named by index in the private directory: a source's names never reach a path here
		local := filepath.Join(tmp, strconv.Itoa(i))
		n, err := src.download(ctx, f.src, local, min(f.size, limit-got))
		if err != nil {
			return 0, 0, fmt.Errorf("copy %s from %s: %w", f.src, src.name, err)
		}
		got += n
		if f.rewrite != "" {
			b, err := os.ReadFile(local)
			if err != nil {
				return 0, 0, err
			}
			var nb []byte
			var n int
			if f.rewrite == "yaml" {
				nb, n = rw.rewriteYAMLPaths(b)
			} else {
				nb, n = rw.rewriteJSON(b)
			}
			clear(b)
			if n > 0 {
				rewritten += n
				if err := os.WriteFile(local, nb, 0o600); err != nil {
					return 0, 0, err
				}
			}
			clear(nb)
		}
		fi, err := os.Stat(local)
		if err != nil {
			return 0, 0, err
		}
		if err := dst.upload(ctx, local, target); err != nil {
			return 0, 0, fmt.Errorf("copy to %s on %s: %w", target, dst.name, err)
		}
		os.Remove(local)
		total += fi.Size()
	}
	notef("moved %d file(s), %s, of session %s (%d path(s) rewritten)", len(sess.files), humanSize(total), sess.ID, rewritten)
	return len(sess.files), total, nil
}

// ---------------------------------------------------------------- agent-sessions

func agentSessionsCmd() *cobra.Command {
	var repoPath, agent string
	var asJSON, all bool
	c := &cobra.Command{
		Use:   "agent-sessions [<worker>|local] [--repo-path DIR] [--agent claude|copilot] [--json]",
		Short: "List the resumable Claude Code / Copilot CLI sessions of a machine (what longarm handoff can move)",
		Long: `List the agent sessions of this machine (local, the default) or a worker:
agent, session id, project directory, last modified, title (the agent's
title or the first prompt), size, and whether it was handed off. --repo-path
keeps those whose project directory is in DIR. Handed-off copies are left
out unless --all. Reads the agents' state directories (worker: through
Files, worker-direct); prints titles only, never transcript contents.

--json: {"machine": "...", "sessions": [{"agent","id","projectDir","modified","title","size","version","handedOff"}]}`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			machine := localMachine
			if len(args) == 1 {
				machine = args[0]
			}
			adapters := allAdapters
			if agent != "" {
				ad, err := adapterFor(agent)
				if err != nil {
					return err
				}
				adapters = []agentAdapter{ad}
			}
			var s *session
			if machine != localMachine {
				var err error
				if s, err = openSession(); err != nil {
					return err
				}
				defer s.close()
			}
			ep, err := openEndpoint(ctx, s, machine)
			if err != nil {
				return err
			}
			defer ep.close()
			var rows []*agentSession
			for _, ad := range adapters {
				list, err := ad.list(ctx, ep, ad.configDir(ctx, ep), "", false)
				if err != nil {
					return fmt.Errorf("%s sessions on %s: %w", ad.name(), ep.name, err)
				}
				for _, s := range list {
					if repoPath != "" && (s.ProjectDir == "" || !ep.within(s.ProjectDir, repoPath)) {
						continue
					}
					if s.HandedOff != nil && !all {
						continue
					}
					rows = append(rows, s)
				}
			}
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].Modified.After(rows[j].Modified) })
			if asJSON {
				if rows == nil {
					rows = []*agentSession{}
				}
				b, _ := json.MarshalIndent(struct {
					Machine  string          `json:"machine"`
					Sessions []*agentSession `json:"sessions"`
				}{ep.name, rows}, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "AGENT\tID\tMODIFIED\tSIZE\tPROJECT\tTITLE")
			for _, r := range rows {
				title := r.Title
				if r.HandedOff != nil {
					title = "[handed off to " + r.HandedOff.To + "] " + title
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Agent, r.ID, r.Modified.Local().Format("01-02 15:04"), humanSize(r.Size),
					r.ProjectDir, title)
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&repoPath, "repo-path", "", "only sessions whose project directory is in this directory")
	c.Flags().StringVar(&agent, "agent", "", "claude or copilot (default: both)")
	c.Flags().BoolVar(&asJSON, "json", false, `{"machine": "...", "sessions": [...]}`)
	c.Flags().BoolVar(&all, "all", false, "also sessions handed off from here")
	return c
}
