package main

// Sessions for programs (docs/usage.md#sessions-for-programs): longarm
// session start / send / stop, the fleet-wide longarm sessions, and the
// --session lookups of run and exec.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/attach"
	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/nodeid"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func sessionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "session",
		Short: "Drive interactive sessions from programs: start (detached), send input, stop",
		Long: `Drive interactive sessions without a terminal (docs/usage.md#sessions-for-programs).
A human can attach to the same session at any time (longarm attach <worker> -s NAME).`,
	}
	c.AddCommand(sessionStartCmd(), sessionSendCmd(), sessionStopCmd())
	return c
}

// tooOld turns a worker's Unimplemented answer into a usage error naming
// what the worker lacks.
func tooOld(err error, conn *client.Conn, what string) error {
	if status.Code(err) == codes.Unimplemented {
		return usagef("%s: worker %s does not support it; update longarmd", what, conn.Worker.Name)
	}
	return err
}

// sessionArgs splits "[<worker>] [-- command...]".
func sessionArgs(cmd *cobra.Command, args []string) (worker string, argv []string, err error) {
	dash := cmd.ArgsLenAtDash()
	switch {
	case dash < 0 && len(args) <= 1:
		if len(args) == 1 {
			worker = args[0]
		}
		return worker, nil, nil
	case dash == 0:
		return "", args, nil
	case dash == 1:
		return args[0], args[1:], nil
	}
	return "", nil, usagef("usage: longarm session start [<worker>|auto] -s NAME [-- command...]")
}

// startJSON is the output of session start --json.
type startJSON struct {
	Worker  string `json:"worker"`
	Session string `json:"session"`
	Cwd     string `json:"cwd"`
	Created bool   `json:"created"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
}

func sessionStartCmd() *cobra.Command {
	var name, cwd, repo, ref, lfs, typ, gitAuth string
	var env, secrets, require []string
	var noQueue, asJSON bool
	var cols, rows uint32
	c := &cobra.Command{
		Use:   "start [<worker>|auto] -s NAME [--cwd DIR | --repo PATH|URL [--ref REF] [--lfs skip|ship|fetch] [--git-auth forward]] [-- command [args...]]",
		Short: "Create a session without attaching (idempotent by name); prints the session and its worker",
		Long: `Create an interactive session without attaching to it and print
"<session> <worker>" (--json: {"worker","session","cwd","created","state"}).

Idempotent by name: a running session of that name with the same command,
cwd, env, secret names and workspace is reported with created=false (exit
0); another spec fails with exit 2. An ended session of that name is
replaced. Without a worker (or with auto) the hub places the session like
run (--require, --no-queue); a running session of that name on any of your
workers that answer is used instead.

--repo prepares a git worktree for the session like run --repo does for a
job; it lives as long as the session's data: never removed while the session
runs, and removed with the ended session ([gc] keep). start returns once the
session runs; when it ended before (workspace preparation failed: 126, the
program did not start: 127, or it exited at once), it exits with that code.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			worker, argv, err := sessionArgs(cmd, args)
			if err != nil {
				return err
			}
			if name == "" {
				return usagef("--session (-s) NAME is required")
			}
			envM, err := parseEnv(env)
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
			if repo == "" && (cmd.Flags().Changed("ref") || cmd.Flags().Changed("lfs")) {
				return usagef("--ref and --lfs go with --repo")
			}
			if err := checkGitAuthFlag(gitAuth, repo); err != nil {
				return err
			}
			// held here until it was sent worker-direct, as for run
			g, err := prepareGitForward(cmd.Context(), gitAuth, repo, ref)
			if err != nil {
				return err
			}
			defer g.clear()
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			ctx := cmd.Context()
			if worker == "" || worker == "auto" {
				if rows, _, _ := listSessions(ctx, s, nil, name, false, ""); len(rows) > 0 {
					worker = rows[0].worker
					fmt.Fprintf(os.Stderr, "longarm: session %s runs on %s\n", name, worker)
				} else {
					if typ == "" {
						typ = sched.TypeOf(argv)
					}
					po := &placeOpts{jobID: ulid.Make().String(), typ: typ, require: reqM, noQueue: noQueue}
					if repo != "" {
						po.repo = repoKeyFor(repo)
					}
					if worker, err = pickWorker(ctx, s, "", "auto", po); err != nil {
						return err
					}
				}
			}
			conn, err := s.conn(worker)
			if err != nil {
				return err
			}
			defer conn.Close()
			// before uploading a checkout: an older worker would refuse after it
			if err := needFeature(ctx, conn, jobs.FeatureSessionStart, "session start"); err != nil {
				return err
			}
			var gitAuthPB *pb.GitAuth
			if g != nil && isURL(repo) {
				// an older worker would drop the field and fetch without it
				if err := needFeature(ctx, conn, jobs.FeatureGitAuth, "--git-auth forward"); err != nil {
					return err
				}
				gitAuthPB = g.pb()
			}
			// the same workspace path as run (a local checkout: remote first
			// with --git-auth forward, then the unpushed delta)
			ws, err := jobWorkspace(ctx, conn, repo, ref, lfs, g)
			if err != nil {
				return err
			}
			if ws != nil && ws.Ref == "" {
				ws.Ref = ref // shown in sessions --json; a checkout's commit is what it runs
			}
			req := &pb.StartSessionRequest{Name: name, Argv: argv, Cwd: cwd, Env: envM, SecretEnv: sec, Workspace: ws,
				GitAuth: gitAuthPB} // for the workspace fetch only
			if cols > 0 && rows > 0 {
				req.Size = &pb.TermSize{Cols: cols, Rows: rows}
			}
			var resp *pb.AttachStarted
			err = client.Retry(ctx, 30*time.Second, notifier(worker), func(ctx context.Context, _ func()) error {
				c, cancel := context.WithTimeout(ctx, 10*time.Minute)
				defer cancel()
				var err error
				resp, err = conn.Sessions().Start(c, req)
				return err
			})
			clear(sec)
			g.clear()
			if gitAuthPB != nil {
				clear(gitAuthPB.Secret)
			}
			if err != nil {
				return tooOld(err, conn, "session start")
			}
			ss := resp.Session
			for ss.State == pb.JobState_JOB_STATE_PREPARING { // a workspace still being prepared
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
				c, cancel := context.WithTimeout(ctx, 30*time.Second)
				got, err := conn.Sessions().Get(c, &pb.GetSessionRequest{Name: name})
				cancel()
				if err != nil && !client.Retryable(err) {
					return err
				}
				if err == nil {
					ss = got
				}
			}
			out := startJSON{Worker: worker, Session: ss.Name, Cwd: ss.Dir, Created: resp.Created, State: stateName(ss.State), Reason: ss.Reason}
			if asJSON {
				b, _ := json.MarshalIndent(out, "", "  ")
				fmt.Println(string(b))
			} else {
				fmt.Printf("%s %s\n", ss.Name, worker)
			}
			if terminalState(ss.State) {
				return sessionEnded(ss)
			}
			return nil
		},
	}
	c.Flags().StringVarP(&name, "session", "s", "", "session name ([0-9A-Za-z_-], required)")
	c.Flags().StringVar(&cwd, "cwd", "", "working directory (default: home; with --repo: relative to the worktree)")
	c.Flags().StringArrayVarP(&env, "env", "e", nil, "KEY=VALUE environment variable (repeatable; stored in the session's metadata)")
	c.Flags().StringArrayVar(&secrets, "secret-env", nil, "NAME or NAME=LOCALVAR: local env var injected into the session (memory-only on the worker, redacted from the transcript)")
	c.Flags().StringVar(&repo, "repo", "", "prepare a git worktree for the session: a local checkout (synced like run --repo) or a clone URL fetched by the worker")
	c.Flags().StringVar(&ref, "ref", "HEAD", "with --repo: HEAD, a branch, a tag or a full commit id")
	c.Flags().StringVar(&lfs, "lfs", lfsSkip, "with --repo: "+lfsFlagHelp)
	c.Flags().StringVar(&gitAuth, "git-auth", gitAuthNone, "with --repo: "+gitAuthFlagHelp)
	c.Flags().StringArrayVar(&require, "require", nil, "auto placement: KEY=VALUE[,KEY=VALUE...] labels the worker must have (repeatable)")
	c.Flags().BoolVar(&noQueue, "no-queue", false, "auto placement: fail (exit 75) instead of queueing when no worker passes the gate")
	c.Flags().StringVar(&typ, "type", "", "auto placement: type for the memory reservation (default: from the command)")
	c.Flags().BoolVar(&asJSON, "json", false, `print {"worker","session","cwd","created","state"}`)
	c.Flags().Uint32Var(&cols, "cols", 0, "initial terminal width (with --rows; default the worker's)")
	c.Flags().Uint32Var(&rows, "rows", 0, "initial terminal height")
	return c
}

// sessionEnded is the exit of a command that found the session ended:
// its exit code like a job's (killed 137, lost 254).
func sessionEnded(ss *pb.Session) error {
	return jobExit(&pb.Job{Spec: &pb.JobSpec{Id: "session " + ss.Name}, State: ss.State, HasExitCode: ss.HasExitCode,
		ExitCode: ss.ExitCode, Reason: ss.Reason}, false)
}

// sendKeys are the names of --key.
var sendKeys = map[string]string{
	"enter": "\r", "tab": "\t", "shift-tab": "\x1b[Z", "esc": "\x1b", "escape": "\x1b", "backspace": "\x7f",
	"space": " ", "up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D",
	"home": "\x1b[H", "end": "\x1b[F", "pageup": "\x1b[5~", "pagedown": "\x1b[6~", "delete": "\x1b[3~",
}

// keyBytes resolves a --key name: the table above or ctrl-<letter>.
func keyBytes(k string) ([]byte, error) {
	k = strings.ToLower(k)
	if b, ok := sendKeys[k]; ok {
		return []byte(b), nil
	}
	if l, ok := strings.CutPrefix(k, "ctrl-"); ok && len(l) == 1 {
		switch c := l[0]; {
		case c >= 'a' && c <= 'z':
			return []byte{c - 'a' + 1}, nil
		case c == '[':
			return []byte{0x1b}, nil
		case c == ']':
			return []byte{0x1d}, nil
		case c == '\\':
			return []byte{0x1c}, nil
		}
	}
	return nil, usagef("--key %q: want enter, tab, shift-tab, esc, backspace, space, up, down, left, right, home, end, pageup, pagedown, delete or ctrl-<letter>", k)
}

// typed converts text as typed: a newline (LF or CRLF) is Enter (CR).
func typed(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i, c := range b {
		switch {
		case c == '\n' && i > 0 && b[i-1] == '\r':
		case c == '\n':
			out = append(out, '\r')
		default:
			out = append(out, c)
		}
	}
	return out
}

func sessionSendCmd() *cobra.Command {
	var name, text, file string
	var keys []string
	var raw, asJSON bool
	var ifIdle uint32
	c := &cobra.Command{
		Use:   "send [<worker>] -s NAME (--text TEXT | --stdin-file FILE | --key KEY...)",
		Short: "Type input into a running session without attaching (one contiguous write)",
		Long: `Write input to a running session's terminal, as if typed, without attaching
or reading its output. --text / --stdin-file bytes come first, then the
--key keys in order. A newline is Enter (CR), like piped attach input;
--raw sends the bytes unchanged. Each send is one contiguous write to the
terminal: it never interleaves with an attached client's keystrokes or
another send; sends are written in the order the worker receives them.

--if-idle-ms N writes only when no input (from an attached client or a
send) reached the terminal in the last N ms; otherwise nothing is written
and longarm exits 75 (retry later).

The input is not stored in the session's metadata or the audit log (its
size only). The session transcript (longarm logs --session) holds what the
terminal shows, which includes input the program echoes.

Exit codes: 0 written, 2 the session has ended (or a usage error), 3 no such
session, 75 --if-idle-ms: input arrived within the window.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return usagef("--session (-s) NAME is required")
			}
			if text != "" && file != "" {
				return usagef("give --text or --stdin-file, not both")
			}
			var data []byte
			switch {
			case file != "":
				b, err := readStdinFile(file)
				if err != nil {
					return err
				}
				data = b
			case cmd.Flags().Changed("text"):
				data = []byte(text)
			}
			if !raw {
				data = typed(data)
			}
			for _, k := range keys {
				b, err := keyBytes(k)
				if err != nil {
					return err
				}
				data = append(data, b...)
			}
			if len(data) == 0 {
				return usagef("nothing to send: give --text, --stdin-file or --key")
			}
			if len(data) > attach.MaxSend {
				return usagef("at most %d bytes per send", attach.MaxSend)
			}
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			worker, err := sessionWorker(cmd.Context(), s, args, name)
			if err != nil {
				return err
			}
			conn, err := s.conn(worker)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			resp, err := conn.Sessions().Send(ctx, &pb.SendSessionRequest{Name: name, Data: data, IfIdleMs: ifIdle})
			clear(data)
			if err != nil {
				return tooOld(err, conn, "session send")
			}
			if asJSON {
				b, _ := json.Marshal(map[string]any{"worker": worker, "session": name, "inputSeq": resp.InputSeq})
				fmt.Println(string(b))
			}
			return nil
		},
	}
	c.Flags().StringVarP(&name, "session", "s", "", "session name (required)")
	c.Flags().StringVar(&text, "text", "", "text to type (newline = Enter)")
	c.Flags().StringVar(&file, "stdin-file", "", "send this file's content (- = longarm's stdin; at most 256 KiB)")
	c.Flags().StringArrayVar(&keys, "key", nil, "a key after the text (repeatable): enter, tab, shift-tab, esc, backspace, space, up, down, left, right, home, end, pageup, pagedown, delete, ctrl-<letter>")
	c.Flags().BoolVar(&raw, "raw", false, "send the bytes unchanged (no newline to Enter conversion)")
	c.Flags().Uint32Var(&ifIdle, "if-idle-ms", 0, "send only if no input reached the terminal in the last N ms (else exit 75, nothing sent)")
	c.Flags().BoolVar(&asJSON, "json", false, `print {"worker","session","inputSeq"}`)
	return c
}

func sessionStopCmd() *cobra.Command {
	var name string
	var grace time.Duration
	var asJSON bool
	c := &cobra.Command{
		Use:   "stop [<worker>] -s NAME [--grace 10s]",
		Short: "End a session: Ctrl-C to its program, then kill the process tree after --grace",
		Long: `End a session gracefully: longarmd types Ctrl-C into its terminal (twice,
a second apart: TUIs such as Claude Code exit on the second; a Unix
terminal in cooked mode turns it into SIGINT), waits up to --grace for the
program to exit, then kills the whole process tree (like attach --kill).
--grace 0 kills at once. Prints "<session> <state>"; --json the session.
An ended session is reported as it is (exit 0); exit 3: no such session.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return usagef("--session (-s) NAME is required")
			}
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			worker, err := sessionWorker(cmd.Context(), s, args, name)
			if err != nil {
				return err
			}
			conn, err := s.conn(worker)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), grace+time.Minute)
			defer cancel()
			ss, err := conn.Sessions().Stop(ctx, &pb.StopSessionRequest{Name: name, Grace: durationpb.New(grace)})
			if err != nil {
				return tooOld(err, conn, "session stop")
			}
			if asJSON {
				fmt.Println(string(sessionsJSONBytes([]sessionRow{{worker: worker, s: ss}}, nil)))
				return nil
			}
			fmt.Printf("%s %s\n", ss.Name, stateName(ss.State))
			return nil
		},
	}
	c.Flags().StringVarP(&name, "session", "s", "", "session name (required)")
	c.Flags().DurationVar(&grace, "grace", 10*time.Second, "how long the program may take to exit after Ctrl-C before it is killed (0 = kill at once)")
	c.Flags().BoolVar(&asJSON, "json", false, "print the session as in sessions --json")
	return c
}

// sessionWorker is the worker of "[<worker>] -s NAME": the argument, else
// the one worker (of those that answer) with a running session of that
// name, else one with an ended one.
func sessionWorker(ctx context.Context, s *session, args []string, name string) (string, error) {
	if len(args) == 1 && args[0] != "auto" {
		return args[0], nil
	}
	rows, unreachable, err := listSessions(ctx, s, nil, name, true, "")
	if err != nil {
		return "", err
	}
	var live, ended []string
	for _, r := range rows {
		if terminalState(r.s.State) {
			ended = append(ended, r.worker)
		} else {
			live = append(live, r.worker)
		}
	}
	switch {
	case len(live) == 1:
		return live[0], nil
	case len(live) > 1:
		return "", usagef("session %s runs on several workers (%s): name one", name, strings.Join(live, ", "))
	case len(ended) == 1:
		return ended[0], nil
	case len(ended) > 1:
		return "", usagef("ended sessions named %s are on several workers (%s): name one", name, strings.Join(ended, ", "))
	}
	if len(unreachable) > 0 {
		return "", notFoundf("no session %s on the workers that answered (unreachable: %s)", name, strings.Join(unreachable, ", "))
	}
	return "", notFoundf("no session %s on any worker", name)
}

// sessionRow is one session of one worker.
type sessionRow struct {
	worker string
	s      *pb.Session
	id     string // the worker's node ID ("" = unknown: a workers.toml entry)
}

// listSessions asks workers (nil = every known worker) for their sessions:
// name "" = all (all: also ended ones), else that session in any state
// when all, running only otherwise. With one worker its failure is the
// error; else unreachable names the workers that did not answer.
func listSessions(ctx context.Context, s *session, workers []string, name string, all bool, owner string) (rows []sessionRow, unreachable []string, err error) {
	if workers == nil {
		for _, w := range s.knownWorkers(ctx) {
			workers = append(workers, w.Name)
		}
		if len(workers) == 0 {
			return nil, nil, errors.New("no workers known (configure workers.toml or a hub)")
		}
	}
	single := len(workers) == 1
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for _, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := workerSessions(ctx, s, w, name, all, owner)
			display, id := w, ""
			if wk, werr := s.worker(w); werr == nil { // the target may be an id: show the name
				display, id = wk.Name, wk.ID
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				unreachable = append(unreachable, w)
				if !single {
					fmt.Fprintf(os.Stderr, "longarm: %s: %s\n", w, oneLine(err))
				}
				return
			}
			for _, ss := range got {
				rows = append(rows, sessionRow{worker: display, s: ss, id: id})
			}
		}()
	}
	wg.Wait()
	if single && firstErr != nil {
		return nil, nil, firstErr
	}
	sort.Strings(unreachable)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].worker != rows[j].worker {
			return rows[i].worker < rows[j].worker
		}
		return rows[i].s.GetCreatedAt().AsTime().Before(rows[j].s.GetCreatedAt().AsTime())
	})
	return rows, unreachable, nil
}

// workerSessions lists one worker's sessions (see listSessions).
func workerSessions(ctx context.Context, s *session, worker, name string, all bool, owner string) ([]*pb.Session, error) {
	conn, err := s.conn(worker)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if name != "" {
		ss, err := conn.Sessions().Get(ctx, &pb.GetSessionRequest{Name: name, Owner: owner})
		switch {
		case status.Code(err) == codes.NotFound:
			return nil, nil
		case err == nil:
			if !all && terminalState(ss.State) {
				return nil, nil
			}
			return []*pb.Session{ss}, nil
		case status.Code(err) != codes.Unimplemented:
			return nil, err
		}
		// an older worker: filter its list
	}
	resp, err := conn.Sessions().List(ctx, &pb.ListSessionsRequest{All: all || name != "", Owner: owner})
	if err != nil {
		return nil, err
	}
	out := resp.Sessions
	if name != "" {
		out = slices.DeleteFunc(out, func(ss *pb.Session) bool { return ss.Name != name || (!all && terminalState(ss.State)) })
	}
	return out, nil
}

// sessionsJSONBytes is sessions --json (docs/usage.md#sessions---json):
// {"sessions": [<Session, every field, plus "worker">], "source":
// "workers", "unreachable": [...]}.
func sessionsJSONBytes(rows []sessionRow, unreachable []string) []byte {
	out := struct {
		Sessions    []map[string]any `json:"sessions"`
		Source      string           `json:"source"`
		Unreachable []string         `json:"unreachable,omitempty"`
	}{Sessions: []map[string]any{}, Source: "workers", Unreachable: unreachable}
	mo := protojson.MarshalOptions{EmitUnpopulated: true}
	for _, r := range rows {
		b, err := mo.Marshal(r.s)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		m["worker"] = r.worker
		if r.id != "" {
			m["workerId"] = r.id
		}
		out.Sessions = append(out.Sessions, m)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return b
}

func sessionsCmd() *cobra.Command {
	var all, asJSON bool
	var owner, name string
	c := &cobra.Command{
		Use:   "sessions [<worker>]",
		Short: "List interactive sessions: of one worker, or of all your workers",
		Long: `List interactive sessions of a worker, or (without one) of every known
worker. The hub does not index sessions: each worker answers for its own,
so with a <worker> its answer is authoritative; fleet-wide, workers that do
not answer are named on stderr and in "unreachable" (--json).

--json prints {"sessions": [...], "source": "workers", "unreachable": [...]}:
each session with every field (also those at their zero value) plus
"worker" (docs/usage.md#sessions---json). -s NAME lists that session alone
(any state with --all, else running) and exits 3 when there is none.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			var workers []string
			if len(args) == 1 {
				workers = args
			}
			rows, unreachable, err := listSessions(cmd.Context(), s, workers, name, all, owner)
			if err != nil {
				return err
			}
			if asJSON {
				fmt.Println(string(sessionsJSONBytes(rows, unreachable)))
			}
			if name != "" && len(rows) == 0 {
				if len(unreachable) > 0 {
					return notFoundf("no session %s on the workers that answered", name)
				}
				return notFoundf("no session %s", name)
			}
			if asJSON {
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fleet := len(workers) == 0
			var ids []string
			for _, r := range rows {
				ids = append(ids, r.id)
			}
			short := nodeid.ShortIDs(ids)
			if fleet {
				fmt.Fprint(tw, "WORKER\tNODE\t")
			}
			fmt.Fprintln(tw, "NAME\tOWNER\tSTATE\tEXIT\tPID\tATTACHED\tCREATED\tACTIVE\tBY\tCOMMAND")
			for _, r := range rows {
				ss := r.s
				exit := "-"
				if ss.HasExitCode {
					exit = fmt.Sprint(ss.ExitCode)
				}
				created, active := "-", "-"
				if ss.CreatedAt != nil {
					created = ss.CreatedAt.AsTime().Local().Format("01-02 15:04:05")
				}
				if ss.LastActivity != nil {
					active = humanDuration(time.Since(ss.LastActivity.AsTime())) + " ago"
				}
				cmdline := strings.Join(ss.Argv, " ")
				if len(cmdline) > 50 {
					cmdline = cmdline[:47] + "..."
				}
				if fleet {
					fmt.Fprintf(tw, "%s\t%s\t", r.worker, dash(short[r.id]))
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\n", ss.Name, ss.Owner, stateName(ss.State), exit, ss.Pid,
					ss.Attached, created, active, ss.CreatedBy, cmdline)
			}
			return tw.Flush()
		},
	}
	c.Flags().BoolVar(&all, "all", false, "include ended sessions")
	c.Flags().BoolVar(&asJSON, "json", false, `JSON output: {"sessions": [...], "source": "workers", "unreachable": [...]}`)
	c.Flags().StringVar(&owner, "owner", "", "only this user's sessions (admins see everybody's)")
	c.Flags().StringVarP(&name, "session", "s", "", "only this session; exit 3 when there is none")
	return c
}

// sessionDirFor resolves run / exec --session: the session's worker and
// working directory (cwd: relative to it unless absolute).
func sessionDirFor(ctx context.Context, s *session, worker, name, cwd string) (string, string, error) {
	var args []string
	if worker != "" {
		args = []string{worker}
	}
	w, err := sessionWorker(ctx, s, args, name)
	if err != nil {
		return "", "", err
	}
	rows, _, err := listSessions(ctx, s, []string{w}, name, true, "")
	if err != nil {
		return "", "", err
	}
	if len(rows) == 0 {
		return "", "", notFoundf("no session %s on %s", name, w)
	}
	dir := rows[0].s.Dir
	switch {
	case cwd == "":
	case dir == "" || strings.HasPrefix(cwd, "/") || strings.HasPrefix(cwd, `\`) || (len(cwd) > 1 && cwd[1] == ':'):
		dir = cwd
	default:
		dir = strings.TrimRight(dir, `/\`) + "/" + cwd
	}
	return w, dir, nil
}
