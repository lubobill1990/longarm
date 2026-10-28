package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/sched"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func execCmd() *cobra.Command {
	var env, secrets []string
	var cwd string
	var timeout time.Duration
	var combined bool
	var maxOut int64
	var on, inSession string
	c := &cobra.Command{
		Use:   "exec [<worker>] -- command [args...]",
		Short: "Run a short command synchronously (no job); exit code is mirrored. Without a worker (or --on auto) the least loaded passing worker is used",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			worker, argv, err := commandArgs(cmd, args)
			if err != nil {
				return err
			}
			envM, err := parseEnv(env)
			if err != nil {
				return err
			}
			sec, err := secretEnv(secrets)
			if err != nil {
				return err
			}
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			if inSession != "" {
				if worker, cwd, err = sessionTarget(cmd.Context(), s, worker, on, inSession, cwd); err != nil {
					return err
				}
			} else if worker, err = pickWorker(cmd.Context(), s, worker, on, nil); err != nil {
				return err
			}
			conn, err := s.conn(worker)
			if err != nil {
				return err
			}
			defer conn.Close()
			mode := pb.OutputMode_OUTPUT_MODE_SEPARATE
			if combined {
				mode = pb.OutputMode_OUTPUT_MODE_COMBINED
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout+30*time.Second)
			defer cancel()
			resp, err := conn.Exec().Run(ctx, &pb.ExecRequest{
				Argv: argv, Cwd: cwd, Env: envM, SecretEnv: sec, Timeout: durationpb.New(timeout),
				Output: mode, MaxOutputBytes: maxOut,
			})
			if err != nil {
				return err
			}
			os.Stdout.Write(resp.Stdout)
			os.Stdout.Write(resp.Combined)
			os.Stderr.Write(resp.Stderr)
			if resp.Truncated {
				fmt.Fprintln(os.Stderr, "longarm: output truncated (raise --max-output or use longarm run)")
			}
			if resp.Error != "" {
				fmt.Fprintln(os.Stderr, "longarm:", resp.Error+traceNote())
			}
			if resp.ExitCode == 127 && resp.Error == "" {
				// on Windows longarmd starts programs through its conhost
				// wrapper, which reports "failed to start" in the program's
				// stderr and exits 127: name the trace for the bug report
				fmt.Fprintln(os.Stderr, "longarm: exit status 127: the program was not found or could not start"+traceNote())
			}
			if resp.ExitCode != 0 {
				code := int(resp.ExitCode)
				if code < 0 || code > 255 {
					code = exitLongarmError
				}
				return &exitError{code: code}
			}
			return nil
		},
	}
	c.Flags().StringArrayVarP(&env, "env", "e", nil, "KEY=VALUE environment variable (repeatable)")
	c.Flags().StringArrayVar(&secrets, "secret-env", nil, "NAME or NAME=LOCALVAR: pass a local env var as a secret (redacted from output, never stored)")
	c.Flags().StringVar(&cwd, "cwd", "", "working directory on the worker (default: home)")
	c.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "kill the process tree after this long")
	c.Flags().BoolVar(&combined, "combined", false, "interleave stderr into stdout")
	c.Flags().Int64Var(&maxOut, "max-output", 0, "max bytes captured per stream (default 1 MiB)")
	c.Flags().StringVar(&on, "on", "", "worker name or \"auto\" (default when no worker is given)")
	c.Flags().StringVarP(&inSession, "session", "s", "", "run on the worker of this session, in its working directory (--cwd: relative to it)")
	return c
}

// sessionTarget resolves --session of run / exec: the session's worker
// (which a given worker must be) and its directory.
func sessionTarget(ctx context.Context, s *session, worker, on, name, cwd string) (string, string, error) {
	if worker == "" {
		worker = on
	} else if on != "" && on != worker {
		return "", "", usagef("give the worker either as an argument or with --on, not both")
	}
	if worker == "auto" {
		worker = ""
	}
	return sessionDirFor(ctx, s, worker, name, cwd)
}

func runCmd() *cobra.Command {
	var env, secrets, labels, require, share []string
	var cwd, name, id, repo, ref, lfs, on, typ, stdinFile, gitAuth, inSession string
	var wait, follow, noQueue bool
	var memLimit string
	var memGB, cpus float64
	c := &cobra.Command{
		Use:   "run [<worker>] -- command [args...]",
		Short: "Submit a job; prints the job id (with --wait/--follow, exits with the job's code). Without a worker (or --on auto) the scheduler places it",
		Long: `Submit a job; longarm prints its id (follow it: longarm logs <id> -f).

Without a worker (or with --on auto) the hub places the job on a worker
that passes the gate - free memory at least min(20 GB, 25 % of its RAM)
and CPU below 70 % - with the best score (free memory minus reserved,
fewer running jobs, a mirror of --repo and warm dependencies count), and
reserves --mem-gb there (default by --type: claude/copilot 2, build 8,
test 4, else 2). When no worker passes, the job waits in the hub's queue
(--no-queue: fail instead). Without a reachable hub longarm picks among
the known workers itself.

Without --repo or --cwd the job runs in the worker's home directory;
--repo . runs it in a fresh copy of the local checkout instead.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			worker, argv, err := commandArgs(cmd, args)
			if err != nil {
				return err
			}
			envM, err := parseEnv(env)
			if err != nil {
				return err
			}
			labelM, err := parseEnv(labels)
			if err != nil {
				return err
			}
			reqM, err := sched.ParseLabels(require, false)
			if err != nil {
				return usageErr(err)
			}
			sec, err := secretEnv(secrets)
			if err != nil {
				return err
			}
			mem, err := parseSize(memLimit)
			if err != nil {
				return err
			}
			if err := checkGitAuthFlag(gitAuth, repo); err != nil {
				return err
			}
			var stdin []byte
			if stdinFile != "" {
				if stdin, err = readStdinFile(stdinFile); err != nil {
					return err
				}
			}
			// held here (like stdin) until it was sent worker-direct
			g, err := prepareGitForward(cmd.Context(), gitAuth, repo, ref)
			if err != nil {
				return err
			}
			defer g.clear()
			if id == "" {
				id = ulid.Make().String() // client-generated => Submit is idempotent
			}
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			if typ == "" {
				typ = sched.TypeOf(argv)
			}
			po := &placeOpts{jobID: id, memGB: memGB, typ: typ, require: reqM, noQueue: noQueue}
			if repo != "" {
				po.repo = repoKeyFor(repo)
			}
			if inSession != "" {
				if repo != "" {
					return usagef("--session runs in the session's directory: not with --repo")
				}
				if worker, cwd, err = sessionTarget(cmd.Context(), s, worker, on, inSession, cwd); err != nil {
					return err
				}
			} else if worker, err = pickWorker(cmd.Context(), s, worker, on, po); err != nil {
				return err
			}
			if labelM == nil {
				labelM = map[string]string{}
			}
			// The hub accounts the reservation of active jobs by these labels.
			labelM[sched.LabelMemGB] = strconv.FormatFloat(sched.Defaults().MemFor(typ, memGB), 'f', -1, 64)
			if typ != "" {
				labelM[sched.LabelType] = typ
			}
			if po.repo != "" {
				labelM[sched.LabelRepo] = po.repo
			}
			conn, err := s.conn(worker)
			if err != nil {
				return err
			}
			defer conn.Close()
			if stdin != nil {
				// an older worker would drop the field and run the job without it
				if err := needFeature(cmd.Context(), conn, jobs.FeatureStdin, "--stdin-file"); err != nil {
					return err
				}
			}
			var gitAuthPB *pb.GitAuth
			if g != nil && isURL(repo) {
				// an older worker would drop the field and fetch without it
				if err := needFeature(cmd.Context(), conn, jobs.FeatureGitAuth, "--git-auth forward"); err != nil {
					return err
				}
				gitAuthPB = g.pb()
			}
			ws, err := jobWorkspace(cmd.Context(), conn, repo, ref, lfs, g)
			if err != nil {
				return err
			}
			req := &pb.SubmitRequest{
				Spec: &pb.JobSpec{Id: id, Name: name, Argv: argv, Cwd: cwd, Env: envM, MemLimitBytes: mem, Labels: labelM,
					Workspace: ws, CpuLimit: cpus},
				// secrets and stdin are sent once, memory-only on the worker
				SecretEnv: sec,
				Stdin:     stdin,
				GitAuth:   gitAuthPB, // for the workspace fetch only
				Share:     share,
			}
			ctx := cmd.Context()
			// Retrying with the same id is safe: Submit is idempotent.
			var job *pb.Job
			err = client.Retry(ctx, 30*time.Second, notifier(worker), func(ctx context.Context, _ func()) error {
				c, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				var err error
				job, err = conn.Jobs().Submit(c, req)
				return err
			})
			clear(stdin) // sent (or given up): no copy of the payload stays here
			g.clear()
			if err != nil {
				return err
			}
			fmt.Println(job.Spec.Id)
			hints := stderrIsTerminal()
			if hints && repo == "" && cwd == "" && inSession == "" {
				if wd, err := os.Getwd(); err == nil && insideGitCheckout(wd) {
					fmt.Fprintln(os.Stderr, gitCheckoutHint(worker))
				}
			}
			if job.State == pb.JobState_JOB_STATE_FAILED && !wait && !follow {
				fmt.Fprintf(os.Stderr, "longarm: job failed to start: %s%s\n", job.Reason, traceNote())
			} else if hints && !wait && !follow {
				fmt.Fprintln(os.Stderr, followHint(job.Spec.Id))
			}
			if follow {
				if err := followLogs(ctx, conn, id, 0, os.Stdout); err != nil {
					return err
				}
				return waitJob(ctx, conn, id, true)
			}
			if wait {
				return waitJob(ctx, conn, id, false)
			}
			return nil
		},
	}
	c.Flags().StringArrayVarP(&env, "env", "e", nil, "KEY=VALUE environment variable (repeatable; stored in job.json)")
	c.Flags().StringArrayVar(&secrets, "secret-env", nil, "NAME or NAME=LOCALVAR: pass a local env var as a secret (memory-only on the worker, redacted from logs)")
	c.Flags().StringArrayVar(&labels, "label", nil, "KEY=VALUE label (repeatable)")
	c.Flags().StringVar(&cwd, "cwd", "", "working directory on the worker (default: home; with --repo: relative to the worktree)")
	c.Flags().StringVar(&name, "name", "", "human-readable job name")
	c.Flags().StringVar(&id, "id", "", "job id (default: new ULID)")
	c.Flags().StringVar(&memLimit, "mem-limit", "", "memory limit of the job's process tree, e.g. 8G (Windows; Linux with per-user isolation; ignored on macOS)")
	c.Flags().BoolVar(&wait, "wait", false, "wait for the job and exit with its exit code")
	c.Flags().BoolVarP(&follow, "follow", "f", false, "stream the log, then exit with the job's exit code")
	c.Flags().StringVar(&repo, "repo", "", "run in a fresh git worktree of this repo: a local checkout (its commits, uncommitted and untracked files are synced) or a clone URL fetched by the worker (with --git-auth forward, or the GH_TOKEN secret, if given)")
	c.Flags().StringVar(&ref, "ref", "HEAD", "with --repo: what to check out - HEAD, a branch, a tag or a full commit id")
	c.Flags().StringVar(&lfs, "lfs", lfsSkip, "with --repo: "+lfsFlagHelp)
	c.Flags().StringVar(&gitAuth, "git-auth", gitAuthNone, "with --repo: "+gitAuthFlagHelp)
	c.Flags().StringVar(&on, "on", "", "worker name or \"auto\" (default when no worker is given)")
	c.Flags().StringVar(&typ, "type", "", "job type for the memory reservation: claude, copilot, build, test (default: from the command)")
	c.Flags().Float64Var(&memGB, "mem-gb", 0, "memory to reserve on the worker in GB (default: by --type)")
	c.Flags().StringArrayVar(&require, "require", nil, "KEY=VALUE[,KEY=VALUE...] labels the worker must have, e.g. os=linux,region=eu (repeatable)")
	c.Flags().BoolVar(&noQueue, "no-queue", false, "fail instead of queueing when no worker passes the gate")
	c.Flags().StringArrayVar(&share, "share", nil, "let team members read the job (state, logs): team = all your teams, team:<name> = one")
	c.Flags().Float64Var(&cpus, "cpus", 0, "CPU limit of the job's process tree in cores (Windows; Linux with per-user isolation)")
	c.Flags().StringVarP(&inSession, "session", "s", "", "run on the worker of this session, in its working directory (e.g. its --repo worktree; --cwd: relative to it)")
	c.Flags().StringVar(&stdinFile, "stdin-file", "", "send this local file (- = longarm's stdin; at most 1 MiB) as the job's stdin: memory-only on the worker, never in job metadata, the hub or logs")
	return c
}

// readStdinFile reads run --stdin-file: a local file or ("-") longarm's own
// stdin, at most jobs.MaxStdinBytes.
func readStdinFile(name string) ([]byte, error) {
	var r io.Reader = os.Stdin
	if name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return nil, usagef("--stdin-file: %v", err)
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, jobs.MaxStdinBytes+1))
	if err != nil {
		return nil, usagef("--stdin-file: %v", err)
	}
	if len(b) > jobs.MaxStdinBytes {
		return nil, usagef("--stdin-file: larger than %d bytes", jobs.MaxStdinBytes)
	}
	if b == nil {
		b = []byte{} // empty, but given: the job's stdin is empty either way
	}
	return b, nil
}

// needFeature fails (exit 2) when the worker does not announce feature.
func needFeature(ctx context.Context, conn *client.Conn, feature, flag string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info, err := conn.Node().Info(ctx, &pb.InfoRequest{})
	if err != nil {
		return err
	}
	if !slices.Contains(info.Features, feature) {
		return usagef("%s: worker %s (longarmd %s) does not support it; update longarmd", flag, conn.Worker.Name, info.Version)
	}
	return nil
}

func parseSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	u := strings.ToUpper(strings.TrimSuffix(strings.ToUpper(s), "B"))
	switch {
	case strings.HasSuffix(u, "K"):
		mult, u = 1<<10, strings.TrimSuffix(u, "K")
	case strings.HasSuffix(u, "M"):
		mult, u = 1<<20, strings.TrimSuffix(u, "M")
	case strings.HasSuffix(u, "G"):
		mult, u = 1<<30, strings.TrimSuffix(u, "G")
	}
	var n int64
	if _, err := fmt.Sscanf(u, "%d", &n); err != nil || n <= 0 {
		return 0, usagef("bad size %q", s)
	}
	return n * mult, nil
}

func notifier(worker string) func(error, time.Duration) {
	return func(err error, wait time.Duration) {
		fmt.Fprintf(os.Stderr, "longarm: %s unreachable (%s); retrying in %s\n", worker, oneLine(err), wait)
	}
}

// logsFunc is Jobs.Logs or Sessions.Logs.
type logsFunc func(ctx context.Context, in *pb.LogsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[pb.LogChunk], error)

// noteGap tells the user when bytes before the returned offset were rotated
// away (out.log size cap).
func noteGap(want, got int64) {
	if want >= 0 && got > want {
		fmt.Fprintf(os.Stderr, "longarm: %d bytes of older output were rotated away (log size cap)\n", got-want)
	}
}

// followLogs streams out.log from offset until the job is terminal,
// transparently reconnecting (resuming at the last byte received) when the
// worker or the network goes away.
func followLogs(ctx context.Context, conn *client.Conn, id string, from int64, w io.Writer) error {
	return followLogsWith(ctx, conn, conn.Jobs().Logs, id, from, w)
}

func followLogsWith(ctx context.Context, conn *client.Conn, logs logsFunc, id string, from int64, w io.Writer) error {
	off := from
	return client.Retry(ctx, 10*time.Minute, notifier(conn.Worker.Name), func(ctx context.Context, progress func()) error {
		st, err := logs(ctx, &pb.LogsRequest{Id: id, Offset: off, Follow: true})
		if err != nil {
			return err
		}
		for {
			chunk, err := st.Recv()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			progress()
			noteGap(off, chunk.Offset)
			if _, err := w.Write(chunk.Data); err != nil {
				return err
			}
			off = chunk.Offset + int64(len(chunk.Data))
		}
	})
}

// waitJob blocks until the job is terminal (reconnecting as needed) and
// returns an exitError mirroring the job's outcome.
func waitJob(ctx context.Context, conn *client.Conn, id string, quiet bool) error {
	var last *pb.Job
	err := client.Retry(ctx, 10*time.Minute, notifier(conn.Worker.Name), func(ctx context.Context, progress func()) error {
		st, err := conn.Jobs().Wait(ctx, &pb.WaitRequest{Id: id})
		if err != nil {
			return err
		}
		for {
			j, err := st.Recv()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			progress()
			last = j
		}
	})
	if err != nil {
		return err
	}
	if last == nil {
		return &exitError{code: exitLongarmError, msg: "no job state received"}
	}
	return jobExit(last, quiet)
}

func jobExit(j *pb.Job, quiet bool) error {
	switch j.State {
	case pb.JobState_JOB_STATE_DONE:
		return nil
	case pb.JobState_JOB_STATE_FAILED:
		code := int(j.ExitCode)
		if code <= 0 || code > 255 {
			code = 1
		}
		msg := ""
		if !quiet {
			msg = fmt.Sprintf("job %s failed: %s", j.Spec.Id, j.Reason)
		}
		return &exitError{code: code, msg: msg}
	case pb.JobState_JOB_STATE_KILLED:
		return &exitError{code: exitKilled, msg: fmt.Sprintf("job %s was killed", j.Spec.Id)}
	case pb.JobState_JOB_STATE_LOST:
		return &exitError{code: exitLost, msg: fmt.Sprintf("job %s was lost: %s", j.Spec.Id, j.Reason)}
	}
	return &exitError{code: exitLongarmError, msg: fmt.Sprintf("job %s ended in unexpected state %s", j.Spec.Id, j.State)}
}

// jobTarget splits "[<worker>] <id>", resolves a short id or "last"
// (jobids.go) and finds the worker when omitted.
func jobTarget(ctx context.Context, s *session, args []string) (worker, id string, err error) {
	id = args[len(args)-1]
	if len(args) == 2 {
		worker = args[0]
	}
	if worker, id, err = s.resolveJobArg(ctx, worker, id); err != nil || worker != "" {
		return worker, id, err
	}
	worker, err = s.findJob(ctx, id)
	return worker, id, err
}

func logsCmd() *cobra.Command {
	var follow bool
	var from int64
	var owner, sessionFlag string
	c := &cobra.Command{
		Use:   "logs [<worker>] <job-id> | logs [<worker>] -s <session>",
		Short: "Print a job's (or with -s, a session's) output from a byte offset; -f follows until it ends",
		Long: `Print a job's output from a byte offset; -f follows it until the job ends.
The job is its id, a unique prefix or suffix of it, or "last" (your most
recent job); without a worker longarm finds it.

-s NAME (--session NAME) prints an interactive session's transcript instead
(raw terminal output); without a worker, the worker running a session of
that name. The older form "logs <worker> <session> --session" still works.`,
		Args: cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, name, args, err := logsSessionArgs(sessionFlag, args)
			if err != nil {
				return err
			}
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			var worker, id string
			if session {
				id = name
				if worker, err = sessionWorker(cmd.Context(), s, args, name); err != nil {
					return err
				}
			} else if worker, id, err = jobTarget(cmd.Context(), s, args); err != nil {
				return err
			}
			conn, err := s.conn(worker)
			if err != nil {
				return err
			}
			defer conn.Close()
			logs := logsFunc(conn.Jobs().Logs)
			if session {
				sl := conn.Sessions().Logs
				logs = func(ctx context.Context, in *pb.LogsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[pb.LogChunk], error) {
					in.Owner = owner
					return sl(ctx, in, opts...)
				}
			}
			if follow {
				return followLogsWith(cmd.Context(), conn, logs, id, from, os.Stdout)
			}
			st, err := logs(cmd.Context(), &pb.LogsRequest{Id: id, Offset: from})
			if err != nil {
				return err
			}
			for {
				chunk, err := st.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				noteGap(from, chunk.Offset)
				from = -1
				os.Stdout.Write(chunk.Data)
			}
		},
	}
	c.Flags().StringVarP(&sessionFlag, "session", "s", "", "show this interactive session's transcript (raw terminal output); a bare --session takes the name from the arguments")
	c.Flags().Lookup("session").NoOptDefVal = sessionFromArgs
	c.Flags().StringVar(&owner, "owner", "", "with --session, admins: another user's session")
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming until the job ends (reconnects automatically)")
	c.Flags().Int64Var(&from, "from", 0, "start at this byte offset (negative: that many bytes before the end)")
	return c
}

func waitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "wait [<worker>] <job-id>",
		Short: "Wait for a job to end; exit code mirrors the job's (killed=137, lost=254, longarm error=255)",
		Long: `Wait for a job to end. With a hub, longarm subscribes to the hub's job events
(the worker is found by the job id); if the hub is or becomes unreachable it
waits on the worker directly. The exit code mirrors the job's. The job is its
id, a unique prefix or suffix of it, or "last" (your most recent job).`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			ctx := cmd.Context()
			worker, id := "", args[len(args)-1]
			if len(args) == 2 {
				worker = args[0]
			}
			if worker, id, err = s.resolveJobArg(ctx, worker, id); err != nil {
				return err
			}
			if h := s.optionalHub(ctx); h != nil {
				last, err := waitViaHub(ctx, h, worker, id)
				switch {
				case err == nil && last != nil:
					return jobExit(last.Job, false)
				case err != nil && !hubUnavailable(err) && status.Code(err) != codes.NotFound:
					return err
				}
				if err == nil {
					err = errors.New("stream ended without a terminal state")
				}
				s.hubDown(err)
				if worker == "" && last != nil {
					worker = last.Worker
				}
			}
			if worker == "" {
				if worker, err = s.findJob(ctx, id); err != nil {
					return err
				}
			}
			conn, err := s.conn(worker)
			if err != nil {
				return err
			}
			defer conn.Close()
			return waitJob(ctx, conn, id, false)
		},
	}
}

// waitViaHub follows Fleet.WaitJob; it returns the terminal snapshot, or
// the last one seen and the error that broke the stream.
func waitViaHub(ctx context.Context, h *hubclient.Conn, worker, id string) (*pb.FleetJob, error) {
	st, err := h.Fleet().WaitJob(ctx, &pb.FleetJobRequest{Id: id, Worker: worker})
	if err != nil {
		return nil, err
	}
	var last *pb.FleetJob
	for {
		fj, err := st.Recv()
		if errors.Is(err, io.EOF) {
			if last == nil || !terminalState(last.Job.State) {
				return last, status.Error(codes.Unavailable, "hub stream ended early")
			}
			return last, nil
		}
		if err != nil {
			return last, err
		}
		last = fj
	}
}

func terminalState(s pb.JobState) bool {
	switch s {
	case pb.JobState_JOB_STATE_DONE, pb.JobState_JOB_STATE_FAILED, pb.JobState_JOB_STATE_KILLED, pb.JobState_JOB_STATE_LOST:
		return true
	}
	return false
}

func killCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kill [<worker>] <job-id>",
		Short: "Kill a job's whole process tree (job: its id, a unique prefix or suffix, or \"last\")",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			worker, id, err := jobTarget(cmd.Context(), s, args)
			if err != nil {
				return err
			}
			conn, err := s.conn(worker)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			j, err := conn.Jobs().Kill(ctx, &pb.KillRequest{Id: id})
			if err != nil {
				return err
			}
			fmt.Printf("%s %s\n", j.Spec.Id, stateName(j.State))
			return nil
		},
	}
}

func stateName(s pb.JobState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "JOB_STATE_"))
}

func jobsCmd() *cobra.Command {
	var states []string
	var limit int
	var asJSON, hubOnly, mine, all bool
	var jobID string
	c := &cobra.Command{
		Use:   "jobs [<worker>]",
		Short: "List jobs (newest first): fleet-wide from the hub's index, else from the workers directly",
		Long: `List jobs (newest first): fleet-wide from the hub's index; when the hub is
unreachable, from the known workers directly (a note on stderr says so).

--json prints {"jobs": [...], "source": "hub" | "workers"} (and, from the
workers, "unreachable": [workers that did not answer]). Only "source": "hub"
makes a missing job authoritative. --hub-only never falls back: no hub
configured or no answer exits 69. --id <job> lists that job alone and exits
3 when it is not known (to the hub; with the fallback: to the workers that
answered). See docs/usage.md#jobs---json.

The table shows the newest 20 jobs (--limit N; 0 = all). Admins see
everybody's jobs, so for an admin certificate it shows only yours unless
--all; --mine shows only yours for anyone (without it you also see jobs
shared with your teams). --json lists every job you may see unless
--limit, --mine or --all are given (as before).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if mine && all {
				return usagef("--mine and --all exclude each other")
			}
			var want []pb.JobState
			for _, s := range states {
				v, ok := pb.JobState_value["JOB_STATE_"+strings.ToUpper(s)]
				if !ok {
					return usagef("unknown state %q", s)
				}
				want = append(want, pb.JobState(v))
			}
			worker := ""
			if len(args) == 1 {
				worker = args[0]
			}
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			ctx := cmd.Context()
			if jobID != "" && !hubOnly {
				if _, jobID, err = s.resolveJobArg(ctx, worker, jobID); err != nil {
					return err
				}
			}
			v := planJobsView(jobsViewOpts{limit: limit, limitSet: cmd.Flags().Changed("limit"), json: asJSON, id: jobID != "",
				mine: mine, all: all, admin: s.dialer.Identity().Role == pki.RoleAdmin})
			var rows []*pb.FleetJob
			var unreachable []string
			source := ""
			var h *hubclient.Conn
			if hubOnly {
				if h, err = s.requireHub(ctx); err != nil {
					return err
				}
			} else {
				h = s.optionalHub(ctx)
			}
			if h != nil {
				rows, err = hubJobs(ctx, h, worker, jobID, want, v.fetch)
				switch {
				case err == nil:
					source = "hub"
				case hubUnavailable(err) && hubOnly:
					return &exitError{code: exitHubUnavailable, msg: fmt.Sprintf("hub %s did not answer: %s", s.cfg.Hub.Address, oneLine(err))}
				case hubUnavailable(err):
					s.hubDown(err)
				default:
					return err
				}
			}
			if source == "" {
				source = "workers"
				if rows, unreachable, err = workerJobs(ctx, s, worker, jobID, want, v.fetch); err != nil {
					return err
				}
			}
			// --id: whether the job exists, then whether it passes --state
			// (a job that exists but doesn't match is an empty list, exit 0)
			if jobID != "" && len(rows) > 0 && len(want) > 0 {
				rows = slices.DeleteFunc(rows, func(fj *pb.FleetJob) bool { return !slices.Contains(want, fj.Job.GetState()) })
			} else if jobID != "" && len(rows) == 0 {
				if asJSON {
					printJobsJSON(nil, source, unreachable)
				}
				if source == "hub" {
					return notFoundf("job %s not known to the hub", jobID)
				}
				return notFoundf("job %s not found on the workers that answered", jobID)
			}
			rows, more := v.apply(rows, s.dialer.Identity().Owner())
			if asJSON {
				printJobsJSON(rows, source, unreachable)
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "WORKER\tID\tSTATE\tEXIT\tSTARTED\tDURATION\tLOG\tOWNER\tNAME\tCOMMAND")
			for _, fj := range rows {
				j := fj.Job
				exit := "-"
				if j.HasExitCode {
					exit = fmt.Sprint(j.ExitCode)
				}
				started, dur := "-", "-"
				if j.StartedAt != nil {
					started = j.StartedAt.AsTime().Local().Format("01-02 15:04:05")
					end := time.Now()
					if j.EndedAt != nil {
						end = j.EndedAt.AsTime()
					}
					dur = humanDuration(end.Sub(j.StartedAt.AsTime()))
				}
				st := stateName(j.State)
				if j.Orphaned {
					st += "(orphan)"
				}
				if !fj.WorkerOnline && !terminalState(j.State) {
					st += "(worker offline)"
				}
				cmdline := strings.Join(j.Spec.Argv, " ")
				if len(cmdline) > 60 {
					cmdline = cmdline[:57] + "..."
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", fj.Worker, j.Spec.Id, st, exit, started, dur,
					j.LogSize, dash(j.Spec.Owner), j.Spec.Name, cmdline)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if note := v.note(more); note != "" {
				fmt.Fprintln(os.Stderr, note)
			}
			return nil
		},
	}
	c.Flags().StringSliceVar(&states, "state", nil, "filter by state (running, done, failed, killed, lost, ...)")
	c.Flags().IntVar(&limit, "limit", defaultJobsLimit, "max jobs (0 = all; --json without --limit: all)")
	c.Flags().BoolVar(&mine, "mine", false, "only your own jobs (the default for admin certificates)")
	c.Flags().BoolVar(&all, "all", false, "every job you may see (admins: everybody's)")
	c.Flags().BoolVar(&asJSON, "json", false, `JSON output: {"jobs": [...], "source": "hub"|"workers"}`)
	c.Flags().BoolVar(&hubOnly, "hub-only", false, "ask only the hub: exit 69 when there is none or it does not answer (no fallback to the workers)")
	c.Flags().StringVar(&jobID, "id", "", "only this job; exit 3 when it is not known")
	return c
}

// hubJobs lists jobs from the hub's index (id: that job only, whatever
// its state; a NotFound answer is an empty list).
func hubJobs(ctx context.Context, h *hubclient.Conn, worker, id string, want []pb.JobState, limit int) ([]*pb.FleetJob, error) {
	ctx, cancel := context.WithTimeout(ctx, hubTimeout)
	defer cancel()
	if id != "" {
		fj, err := h.Fleet().GetJob(ctx, &pb.FleetJobRequest{Id: id, Worker: worker})
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []*pb.FleetJob{fj}, nil
	}
	resp, err := h.Fleet().ListJobs(ctx, &pb.FleetJobsRequest{Worker: worker, States: want, Limit: int32(limit)})
	if err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}

// workerJobs lists jobs from the workers directly (worker "" = every known
// worker); unreachable names the workers that did not answer. With one
// worker its failure is the error.
func workerJobs(ctx context.Context, s *session, worker, id string, want []pb.JobState, limit int) (rows []*pb.FleetJob, unreachable []string, err error) {
	var targets []string
	if worker != "" {
		targets = []string{worker}
	} else {
		for _, w := range s.knownWorkers(ctx) {
			targets = append(targets, w.Name)
		}
	}
	if len(targets) == 0 {
		return nil, nil, noWorkersError(s)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for _, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := s.conn(t)
			if err == nil {
				defer conn.Close()
				ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				var js []*pb.Job
				if id != "" {
					var j *pb.Job
					if j, err = conn.Jobs().Get(ctx, &pb.GetJobRequest{Id: id}); err == nil {
						js = append(js, j)
					} else if status.Code(err) == codes.NotFound {
						err = nil
					}
				} else {
					var resp *pb.ListJobsResponse
					if resp, err = conn.Jobs().List(ctx, &pb.ListJobsRequest{States: want, Limit: int32(limit)}); err == nil {
						js = resp.Jobs
					}
				}
				if err == nil {
					mu.Lock()
					for _, j := range js {
						rows = append(rows, &pb.FleetJob{Worker: t, Job: j, WorkerOnline: true})
					}
					mu.Unlock()
					return
				}
			}
			mu.Lock()
			if firstErr == nil || len(targets) == 1 {
				firstErr = err
			}
			unreachable = append(unreachable, t)
			mu.Unlock()
			if len(targets) > 1 {
				fmt.Fprintf(os.Stderr, "longarm: %s: %s\n", t, oneLine(err))
			}
		}()
	}
	wg.Wait()
	if len(targets) == 1 && firstErr != nil {
		return nil, nil, firstErr
	}
	sort.Strings(unreachable)
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Job.GetCreatedAt().AsTime().After(rows[j].Job.GetCreatedAt().AsTime())
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, unreachable, nil
}

// jobsJSON is the output of jobs --json (docs/usage.md#jobs---json): the
// fields of FleetJobsResponse plus where the list came from.
type jobsJSON struct {
	Jobs        []json.RawMessage `json:"jobs"`
	Source      string            `json:"source"`                // "hub" | "workers"
	Unreachable []string          `json:"unreachable,omitempty"` // source "workers": workers that did not answer
}

func jobsJSONBytes(rows []*pb.FleetJob, source string, unreachable []string) []byte {
	out := jobsJSON{Jobs: []json.RawMessage{}, Source: source, Unreachable: unreachable}
	for _, fj := range rows {
		b, err := protojson.Marshal(fj)
		if err != nil {
			continue
		}
		out.Jobs = append(out.Jobs, b)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return b
}

func printJobsJSON(rows []*pb.FleetJob, source string, unreachable []string) {
	fmt.Println(string(jobsJSONBytes(rows, source, unreachable)))
}

// pickWorker resolves the target of exec/run: the positional worker, --on,
// or automatic placement (po == nil: a short command, no reservation).
func pickWorker(ctx context.Context, s *session, worker, on string, po *placeOpts) (string, error) {
	if worker != "" && on != "" && worker != on {
		return "", usagef("give the worker either as an argument or with --on, not both")
	}
	if worker == "" {
		worker = on
	}
	if worker != "" && worker != "auto" {
		return worker, nil
	}
	w, err := s.resolveTarget(ctx, "auto", po)
	if err != nil {
		return "", err
	}
	if po == nil {
		fmt.Fprintf(os.Stderr, "longarm: running on %s\n", w)
	}
	return w, nil
}

// repoKeyFor is the mirror key of --repo (for scheduler cache affinity).
func repoKeyFor(repo string) string {
	if isURL(repo) {
		return workspace.RepoKey(repo)
	}
	lr, err := openLocalRepo(repo, "")
	if err != nil {
		return ""
	}
	return lr.key
}
