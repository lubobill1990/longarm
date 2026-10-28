package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/nodeid"
	"github.com/lubobill1990/longarm/internal/sched"
	"github.com/lubobill1990/longarm/internal/telemetry"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// longarm mcp: a stdio MCP server for Claude Code / Copilot CLI (DESIGN §3.2):
// the local agent keeps its "brain" here and uses workers as hands.
//
//	claude mcp add longarm -- longarm mcp
//
// Large outputs never flood the agent: commands run as jobs on the worker
// (output in the job log), and results carry the head and tail plus a job
// handle to page through the rest with fleet_job_logs.

const (
	defaultOutputBytes = 16 << 10
	maxOutputBytes     = 256 << 10
	maxReadBytes       = 1 << 20
	maxWriteBytes      = 16 << 20
)

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run the Longarm MCP server on stdio (tools for Claude Code / Copilot CLI)",
		Long: `Run the Longarm MCP server on stdin/stdout.

Register it with Claude Code:
  claude mcp add longarm -- longarm mcp
with Copilot CLI (~/.copilot/mcp-config.json):
  {"mcpServers": {"longarm": {"type": "local", "command": "longarm", "args": ["mcp"], "tools": ["*"]}}}

Tools: fleet_status, fleet_exec, fleet_job_submit, fleet_job_wait,
fleet_job_logs, fleet_job_kill, fleet_read, fleet_write, fleet_sync,
fleet_forward, fleet_link. Secrets for jobs are taken by name from this process's
environment (secret_env); only names listed in LONGARM_MCP_SECRETS may be used:
  claude mcp add longarm -e LONGARM_MCP_SECRETS=GH_TOKEN -- longarm mcp`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			s.dialer.OneShot = false // long-lived
			quietHub = true
			go s.dialer.WatchTrust(cmd.Context(), nil)
			// stdout belongs to the protocol: anything else printed by shared
			// code goes to stderr.
			out := os.Stdout
			os.Stdout = os.Stderr
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			pctx, pcancel := context.WithCancel(ctx)
			waitPush := runPusher(pctx, s)
			defer func() { pcancel(); waitPush() }()
			return newMCPServer(s).Run(ctx, &mcp.IOTransport{Reader: os.Stdin, Writer: out})
		},
	}
}

type mcpTools struct{ s *session }

// addTool registers a tool whose errors name the fix when a worker was
// refused by end-to-end trust (gRPC buries that in a connection error).
func addTool[In, Out any](srv *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(srv, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		// every tool call is an operation of its own (telemetry)
		sp := telemetry.NewSpan()
		ctx = telemetry.WithSpan(ctx, sp)
		start := time.Now()
		res, out, err := h(ctx, req, in)
		code := "ok"
		if err != nil {
			code = errType(err)
		} else if res != nil && res.IsError {
			code = "tool_error"
		}
		telemetry.RecordRequest(ctx, telemetry.Request{Name: "mcp " + t.Name, Span: sp, Duration: time.Since(start), Code: code,
			Success: err == nil, Always: true})
		if msg, ok := client.TrustRefusal(err); ok {
			err = errors.New(msg)
		}
		return res, out, err
	})
}

func newMCPServer(s *session) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "longarm", Version: version.Version}, &mcp.ServerOptions{
		Instructions: "Remote execution on the Longarm worker pool. Use fleet_status to see workers, fleet_exec for " +
			"commands (worker \"auto\" picks one), fleet_job_submit/wait/logs/kill for long jobs (e.g. whole claude " +
			"sessions), fleet_read/fleet_write/fleet_sync for files and code, fleet_forward to reach a worker port, fleet_link to let one worker reach another's port.",
	})
	t := &mcpTools{s: s}
	addTool(srv, &mcp.Tool{Name: "fleet_status", Description: "Overview of the worker pool: who is online, load, free memory, running jobs, repo caches, labels, owner (user-owned workers)."}, t.status)
	addTool(srv, &mcp.Tool{Name: "fleet_exec", Description: "Run a command on a worker (worker \"auto\" = scheduler's choice) and wait for it. Returns the exit code and the output, truncated to head+tail when large; the full output stays in the job log (see fleet_job_logs with the returned job handle)."}, t.exec)
	addTool(srv, &mcp.Tool{Name: "fleet_job_submit", Description: "Start a long-running job (build, test matrix, a whole `claude -p` / `copilot -p` session) on a worker and return its handle at once. With repo set, the job runs in a fresh git worktree of that local checkout (uncommitted changes included) or clone URL."}, t.submit)
	addTool(srv, &mcp.Tool{Name: "fleet_job_wait", Description: "Wait until a job ends (or timeout_seconds passes) and return its state and exit code."}, t.wait)
	addTool(srv, &mcp.Tool{Name: "fleet_job_logs", Description: "Read a job's output from a byte offset (negative = from the end). Returns the text and next_offset for paging."}, t.logs)
	addTool(srv, &mcp.Tool{Name: "fleet_job_kill", Description: "Kill a job's whole process tree."}, t.kill)
	addTool(srv, &mcp.Tool{Name: "fleet_read", Description: "Read a file on a worker (relative paths are relative to the worker account's home)."}, t.read)
	addTool(srv, &mcp.Tool{Name: "fleet_write", Description: "Write a file on a worker (atomically, sha256-verified)."}, t.write)
	addTool(srv, &mcp.Tool{Name: "fleet_sync", Description: "Mirror a local git checkout (commits, uncommitted and untracked files) into a worktree on a worker; only missing objects are sent."}, t.sync)
	addTool(srv, &mcp.Tool{Name: "fleet_forward", Description: "Forward a worker port to localhost (hosted by the longarm daemon, so it outlives this call) and return the local URL; action list/stop manages forwards."}, t.forward)
	addTool(srv, &mcp.Tool{Name: "fleet_link", Description: "Port links between workers (joint debugging: frontend + tests on worker A, backend on worker B): create makes target <worker>:<port> reachable as 127.0.0.1:<listen_port> on worker 'on' for your processes there - traffic goes worker to worker, not through this machine; jobs you start there get LONGARM_LINKS and LONGARM_LINK_<WORKER>_<PORT>. list shows your links with state and bytes; remove ends one."}, t.link)
	return srv
}

// ---------------------------------------------------------------- status

type statusIn struct {
	Worker string `json:"worker,omitempty" jsonschema:"only this worker (name, node id, or unique id prefix)"`
}

type workerStatus struct {
	Name       string            `json:"name"`
	ID         string            `json:"id,omitempty"` // the node ID (targets accept it, or a unique prefix)
	State      string            `json:"state"`
	CPUPercent float64           `json:"cpu_percent"`
	FreeGB     float64           `json:"free_gb"`
	TotalGB    float64           `json:"total_gb"`
	Jobs       int32             `json:"jobs"`
	ReservedGB float64           `json:"reserved_gb,omitempty"`
	Claude     int32             `json:"claude_sessions"`
	Copilot    int32             `json:"copilot_sessions"`
	DiskFreeGB float64           `json:"disk_free_gb"`
	Labels     map[string]string `json:"labels,omitempty"`
	Mirrors    []string          `json:"mirrors,omitempty"`
	Owner      string            `json:"owner,omitempty"` // a user's own worker ("" = the shared fleet)
	Error      string            `json:"error,omitempty"`
}

type statusOut struct {
	Source  string         `json:"source"` // hub | direct
	Workers []workerStatus `json:"workers"`
}

func fillMetrics(w *workerStatus, m *pb.MetricsSnapshot) {
	if m == nil {
		return
	}
	const gb = 1 << 30
	w.CPUPercent, w.TotalGB = m.CpuPercent, float64(m.MemTotalBytes)/gb
	w.FreeGB = float64(m.MemTotalBytes-min(m.MemUsedBytes, m.MemTotalBytes)) / gb
	w.Jobs, w.DiskFreeGB = m.JobsRunning, float64(m.DiskFreeBytes)/gb
	if a := m.Agents["claude"]; a != nil {
		w.Claude = a.Sessions
	}
	if a := m.Agents["copilot"]; a != nil {
		w.Copilot = a.Sessions
	}
}

func (t *mcpTools) status(ctx context.Context, _ *mcp.CallToolRequest, in statusIn) (*mcp.CallToolResult, statusOut, error) {
	out := statusOut{Workers: []workerStatus{}}
	if nodes, ok := t.s.hubNodes(ctx, false); ok {
		out.Source = "hub"
		var only *pb.NodeInfo
		if in.Worker != "" {
			t, err := nodeid.ParseTarget(in.Worker)
			if err != nil {
				return nil, out, err
			}
			if only, err = resolveNode(t, nodes, "worker"); err != nil {
				return nil, out, err
			}
		}
		for _, n := range nodes {
			if only != nil && n != only {
				continue
			}
			w := workerStatus{Name: n.Name, ID: n.Id, State: "online", Labels: n.Labels, Mirrors: n.Mirrors, ReservedGB: n.ReservedGb, Owner: n.Owner}
			switch {
			case n.State != "approved":
				w.State = n.State
			case !n.Online:
				w.State = "offline"
			case n.Draining:
				w.State = "draining: " + n.DrainReason
			}
			fillMetrics(&w, n.Metrics)
			if n.JobsActive > w.Jobs {
				w.Jobs = n.JobsActive
			}
			out.Workers = append(out.Workers, w)
		}
		return nil, out, nil
	}
	out.Source = "direct"
	for _, c := range t.s.candidates(ctx, "") {
		if in.Worker != "" && c.Name != in.Worker {
			continue
		}
		w := workerStatus{Name: c.Name, State: "online", Labels: c.Labels, CPUPercent: c.CPU, FreeGB: c.FreeGB, Jobs: int32(c.Running)}
		if !c.Online {
			w.State = "unreachable"
		}
		out.Workers = append(out.Workers, w)
	}
	return nil, out, nil
}

// ---------------------------------------------------------------- jobs

type cmdSpec struct {
	Worker    string            `json:"worker,omitempty" jsonschema:"worker name, node id or unique id prefix, or \"auto\" (default) to let the scheduler pick"`
	Command   string            `json:"command,omitempty" jsonschema:"shell command line (pwsh on Windows workers, sh elsewhere); or use argv"`
	Argv      []string          `json:"argv,omitempty" jsonschema:"program and arguments, run without a shell"`
	Cwd       string            `json:"cwd,omitempty" jsonschema:"working directory on the worker (default: home; with repo: relative to the worktree)"`
	Env       map[string]string `json:"env,omitempty" jsonschema:"environment variables"`
	SecretEnv []string          `json:"secret_env,omitempty" jsonschema:"names of environment variables of the longarm mcp process to pass as secrets (NAME or NAME=LOCALVAR); only names the operator allowed in LONGARM_MCP_SECRETS work; values are redacted from logs and never stored on the worker's disk"`
	Require   map[string]string `json:"require,omitempty" jsonschema:"labels the worker must have, e.g. {\"os\":\"linux\"} (auto placement)"`
}

type execIn struct {
	cmdSpec
	TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"stop waiting after this long (default 600); the job keeps running and its handle is returned"`
	MaxOutputBytes int `json:"max_output_bytes,omitempty" jsonschema:"output returned inline (default 16384, max 262144); more is truncated to head and tail"`
}

type jobOut struct {
	Worker    string `json:"worker"`
	WorkerID  string `json:"worker_id,omitempty"` // the worker's node ID
	JobID     string `json:"job_id"`
	State     string `json:"state"`
	ExitCode  *int32 `json:"exit_code,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Output    string `json:"output,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	LogSize   int64  `json:"log_size,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

// argvFor builds the argv of a command for the worker's OS.
func (t *mcpTools) argvFor(ctx context.Context, conn *client.Conn, spec cmdSpec) ([]string, error) {
	if len(spec.Argv) > 0 {
		return spec.Argv, nil
	}
	if strings.TrimSpace(spec.Command) == "" {
		return nil, errors.New("give command or argv")
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	info, err := conn.Node().Info(cctx, &pb.InfoRequest{})
	if err != nil {
		return nil, err
	}
	if info.Os == "windows" {
		return []string{"pwsh", "-NoProfile", "-NonInteractive", "-Command", spec.Command}, nil
	}
	return []string{"sh", "-c", spec.Command}, nil
}

func stateString(s pb.JobState) string { return stateName(s) }

func toJobOut(worker string, j *pb.Job) jobOut {
	o := jobOut{Worker: worker, JobID: j.Spec.GetId(), State: stateString(j.State), Reason: j.Reason, LogSize: j.LogSize}
	if j.HasExitCode {
		c := j.ExitCode
		o.ExitCode = &c
	}
	return o
}

// submit places (if needed) and submits a job.
func (t *mcpTools) submitJob(ctx context.Context, spec cmdSpec, name, repo, ref, typ string, memGB float64) (string, *client.Conn, *pb.Job, error) {
	sec, err := mcpSecretEnv(spec.SecretEnv)
	if err != nil {
		return "", nil, nil, err
	}
	id := ulid.Make().String()
	worker := spec.Worker
	po := &placeOpts{jobID: id, memGB: memGB, typ: typ, require: spec.Require}
	if repo != "" {
		po.repo = repoKeyFor(repo)
	}
	if worker == "" || worker == "auto" {
		if typ == "" && len(spec.Argv) > 0 {
			po.typ = sched.TypeOf(spec.Argv)
		}
		if worker, err = t.s.place(ctx, *po); err != nil {
			return "", nil, nil, err
		}
	}
	conn, err := t.s.connFor(worker, po)
	if err != nil {
		return "", nil, nil, err
	}
	argv, err := t.argvFor(ctx, conn, spec)
	if err != nil {
		conn.Close()
		return "", nil, nil, err
	}
	ws, err := jobWorkspace(ctx, conn, repo, ref, "", nil)
	if err != nil {
		conn.Close()
		return "", nil, nil, err
	}
	labels := map[string]string{sched.LabelMemGB: fmt.Sprint(sched.Defaults().MemFor(po.typ, memGB)), "longarm.via": "mcp"}
	if po.typ != "" {
		labels[sched.LabelType] = po.typ
	}
	if po.repo != "" {
		labels[sched.LabelRepo] = po.repo
	}
	req := &pb.SubmitRequest{Spec: &pb.JobSpec{Id: id, Name: name, Argv: argv, Cwd: spec.Cwd, Env: spec.Env, Labels: labels, Workspace: ws}, SecretEnv: sec}
	var job *pb.Job
	err = client.Retry(ctx, 30*time.Second, nil, func(ctx context.Context, _ func()) error {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var err error
		job, err = conn.Jobs().Submit(c, req)
		return err
	})
	if err != nil {
		conn.Close()
		return "", nil, nil, err
	}
	return worker, conn, job, nil
}

// waitFor waits up to d for a job; returns the latest snapshot.
func waitUpTo(parent context.Context, conn *client.Conn, id string, d time.Duration) (*pb.Job, error) {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	var last *pb.Job
	err := client.Retry(ctx, d, nil, func(ctx context.Context, progress func()) error {
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
	// The worker enforces the propagated deadline too: its RST_STREAM can
	// beat our own timer and arrive as status DeadlineExceeded while ctx is
	// still live.
	timedOut := ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded
	if last != nil && (err == nil || timedOut) {
		return last, nil
	}
	if last == nil && timedOut && parent.Err() == nil {
		// The wait ran out before the first state arrived (a slow connect):
		// the current state, as promised.
		gctx, gcancel := context.WithTimeout(parent, 10*time.Second)
		defer gcancel()
		if j, gerr := conn.Jobs().Get(gctx, &pb.GetJobRequest{Id: id}); gerr == nil {
			return j, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return last, nil
}

// readLog reads [off, off+n) of a job log (off < 0: from the end).
func readLog(ctx context.Context, conn *client.Conn, id string, off int64, n int) ([]byte, int64, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	st, err := conn.Jobs().Logs(cctx, &pb.LogsRequest{Id: id, Offset: off})
	if err != nil {
		return nil, 0, err
	}
	var buf []byte
	start := int64(-1)
	for len(buf) < n {
		c, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		if start < 0 {
			start = c.Offset
		}
		buf = append(buf, c.Data...)
	}
	if len(buf) > n {
		buf = buf[:n]
	}
	if start < 0 {
		start = max(off, 0)
	}
	return buf, start + int64(len(buf)), nil
}

func clampOutput(n int) int {
	if n <= 0 {
		return defaultOutputBytes
	}
	return min(n, maxOutputBytes)
}

// outputOf returns the job output inline, or head + tail when it is larger
// than limit.
func outputOf(ctx context.Context, conn *client.Conn, j *pb.Job, limit int) (string, bool, error) {
	id := j.Spec.Id
	if j.LogSize <= int64(limit) {
		b, _, err := readLog(ctx, conn, id, 0, limit)
		return toText(b), false, err
	}
	head, _, err := readLog(ctx, conn, id, 0, limit/4)
	if err != nil {
		return "", false, err
	}
	tail, _, err := readLog(ctx, conn, id, -int64(limit-limit/4), limit-limit/4)
	if err != nil {
		return "", false, err
	}
	omitted := j.LogSize - int64(len(head)) - int64(len(tail))
	return fmt.Sprintf("%s\n... [%d bytes omitted: fleet_job_logs job_id=%s offset=%d] ...\n%s", toText(head), omitted, id, len(head), toText(tail)), true, nil
}

func toText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

func (t *mcpTools) exec(ctx context.Context, _ *mcp.CallToolRequest, in execIn) (*mcp.CallToolResult, jobOut, error) {
	worker, conn, job, err := t.submitJob(ctx, in.cmdSpec, "mcp exec", "", "", "", 0)
	if err != nil {
		return nil, jobOut{}, err
	}
	defer conn.Close()
	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	if j, err := waitUpTo(ctx, conn, job.Spec.Id, timeout); err == nil && j != nil {
		job = j
	}
	out := toJobOut(cmp.Or(conn.Worker.Name, worker), job)
	out.WorkerID = conn.Worker.ID
	if !terminalState(job.State) {
		out.Hint = "still running: use fleet_job_wait / fleet_job_logs with this job_id"
	}
	out.Output, out.Truncated, err = outputOf(ctx, conn, job, clampOutput(in.MaxOutputBytes))
	return nil, out, err
}

type submitIn struct {
	cmdSpec
	Name  string  `json:"name,omitempty" jsonschema:"human-readable job name"`
	Repo  string  `json:"repo,omitempty" jsonschema:"local git checkout path or clone URL: run in a fresh worktree of it"`
	Ref   string  `json:"ref,omitempty" jsonschema:"with repo: commit/branch (default HEAD)"`
	Type  string  `json:"type,omitempty" jsonschema:"claude, copilot, build or test: memory reservation by type"`
	MemGB float64 `json:"mem_gb,omitempty" jsonschema:"memory to reserve in GB"`
}

func (t *mcpTools) submit(ctx context.Context, _ *mcp.CallToolRequest, in submitIn) (*mcp.CallToolResult, jobOut, error) {
	ref := in.Ref
	if ref == "" {
		ref = "HEAD"
	}
	worker, conn, job, err := t.submitJob(ctx, in.cmdSpec, in.Name, in.Repo, ref, in.Type, in.MemGB)
	if err != nil {
		return nil, jobOut{}, err
	}
	conn.Close()
	out := toJobOut(cmp.Or(conn.Worker.Name, worker), job)
	out.WorkerID = conn.Worker.ID
	out.Hint = "use fleet_job_wait to block until it ends, fleet_job_logs to read output"
	return nil, out, nil
}

type jobRef struct {
	Worker string `json:"worker,omitempty" jsonschema:"the job's worker (found by id if omitted)"`
	JobID  string `json:"job_id" jsonschema:"job id"`
}

func (t *mcpTools) jobConn(ctx context.Context, r jobRef) (string, *client.Conn, error) {
	if r.JobID == "" {
		return "", nil, errors.New("job_id is required")
	}
	w := r.Worker
	if w == "" {
		var err error
		if w, err = t.s.findJob(ctx, r.JobID); err != nil {
			return "", nil, err
		}
	}
	conn, err := t.s.conn(w)
	return w, conn, err
}

type waitIn struct {
	jobRef
	TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"max wait (default 300, max 3600); returns the current state when it passes"`
	MaxOutputBytes int `json:"max_output_bytes,omitempty" jsonschema:"also return the output tail when the job ended (default 4096; 0 = default)"`
}

func (t *mcpTools) wait(ctx context.Context, _ *mcp.CallToolRequest, in waitIn) (*mcp.CallToolResult, jobOut, error) {
	w, conn, err := t.jobConn(ctx, in.jobRef)
	if err != nil {
		return nil, jobOut{}, err
	}
	defer conn.Close()
	d := time.Duration(in.TimeoutSeconds) * time.Second
	if d <= 0 {
		d = 5 * time.Minute
	}
	j, err := waitUpTo(ctx, conn, in.JobID, min(d, time.Hour))
	if err != nil {
		return nil, jobOut{}, err
	}
	if j == nil {
		return nil, jobOut{}, fmt.Errorf("no state for job %s", in.JobID)
	}
	out := toJobOut(w, j)
	if terminalState(j.State) {
		limit := in.MaxOutputBytes
		if limit <= 0 {
			limit = 4096
		}
		out.Output, out.Truncated, _ = outputOf(ctx, conn, j, clampOutput(limit))
	} else {
		out.Hint = "still running"
	}
	return nil, out, nil
}

type logsIn struct {
	jobRef
	Offset   int64 `json:"offset,omitempty" jsonschema:"byte offset; negative = that many bytes before the end (default: the last max_bytes)"`
	MaxBytes int   `json:"max_bytes,omitempty" jsonschema:"bytes to return (default 16384, max 262144)"`
}

type logsOut struct {
	Worker     string `json:"worker"`
	JobID      string `json:"job_id"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	LogSize    int64  `json:"log_size"`
	State      string `json:"state"`
	Text       string `json:"text"`
}

func (t *mcpTools) logs(ctx context.Context, _ *mcp.CallToolRequest, in logsIn) (*mcp.CallToolResult, logsOut, error) {
	w, conn, err := t.jobConn(ctx, in.jobRef)
	if err != nil {
		return nil, logsOut{}, err
	}
	defer conn.Close()
	n := clampOutput(in.MaxBytes)
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	j, err := conn.Jobs().Get(cctx, &pb.GetJobRequest{Id: in.JobID})
	cancel()
	if err != nil {
		return nil, logsOut{}, err
	}
	off := in.Offset
	if off == 0 && j.LogSize > int64(n) && in.MaxBytes == 0 {
		off = -int64(n)
	}
	b, next, err := readLog(ctx, conn, in.JobID, off, n)
	if err != nil {
		return nil, logsOut{}, err
	}
	return nil, logsOut{Worker: w, JobID: in.JobID, Offset: next - int64(len(b)), NextOffset: next, LogSize: j.LogSize,
		State: stateString(j.State), Text: toText(b)}, nil
}

func (t *mcpTools) kill(ctx context.Context, _ *mcp.CallToolRequest, in jobRef) (*mcp.CallToolResult, jobOut, error) {
	w, conn, err := t.jobConn(ctx, in)
	if err != nil {
		return nil, jobOut{}, err
	}
	defer conn.Close()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	j, err := conn.Jobs().Kill(cctx, &pb.KillRequest{Id: in.JobID})
	if err != nil {
		return nil, jobOut{}, err
	}
	return nil, toJobOut(w, j), nil
}

// ---------------------------------------------------------------- files

type readIn struct {
	Worker   string `json:"worker" jsonschema:"worker: name, node id or unique id prefix"`
	Path     string `json:"path" jsonschema:"file path on the worker"`
	Offset   int64  `json:"offset,omitempty" jsonschema:"byte offset"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"bytes to read (default 65536, max 1048576)"`
}

type readOut struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Offset    int64  `json:"offset"`
	Encoding  string `json:"encoding"` // text | base64
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (t *mcpTools) read(ctx context.Context, _ *mcp.CallToolRequest, in readIn) (*mcp.CallToolResult, readOut, error) {
	conn, err := t.s.conn(in.Worker)
	if err != nil {
		return nil, readOut{}, err
	}
	defer conn.Close()
	n := in.MaxBytes
	if n <= 0 {
		n = 64 << 10
	}
	n = min(n, maxReadBytes)
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	fi, err := conn.Files().Stat(cctx, &pb.FileStatRequest{Path: in.Path})
	if err != nil {
		return nil, readOut{}, err
	}
	if !fi.Exists || fi.IsDir {
		return nil, readOut{}, fmt.Errorf("%s is not a file on %s", in.Path, in.Worker)
	}
	st, err := conn.Files().Read(cctx, &pb.FileReadRequest{Path: in.Path, Offset: in.Offset})
	if err != nil {
		return nil, readOut{}, err
	}
	var buf []byte
	for len(buf) < n {
		c, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, readOut{}, err
		}
		buf = append(buf, c.Data...)
	}
	cancel()
	out := readOut{Path: fi.AbsPath, Size: fi.Size, Offset: in.Offset, Truncated: in.Offset+int64(min(len(buf), n)) < fi.Size}
	if len(buf) > n {
		buf = buf[:n]
	}
	if utf8.Valid(buf) && !bytes.ContainsRune(buf, 0) {
		out.Encoding, out.Content = "text", string(buf)
	} else {
		out.Encoding, out.Content = "base64", base64.StdEncoding.EncodeToString(buf)
	}
	return nil, out, nil
}

type writeIn struct {
	Worker   string `json:"worker" jsonschema:"worker: name, node id or unique id prefix"`
	Path     string `json:"path" jsonschema:"file path on the worker (parent directories are created)"`
	Content  string `json:"content" jsonschema:"file content"`
	Encoding string `json:"encoding,omitempty" jsonschema:"text (default) or base64"`
	Mode     uint32 `json:"mode,omitempty" jsonschema:"permission bits (default 0644)"`
}

type writeOut struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func (t *mcpTools) write(ctx context.Context, _ *mcp.CallToolRequest, in writeIn) (*mcp.CallToolResult, writeOut, error) {
	data := []byte(in.Content)
	if in.Encoding == "base64" {
		var err error
		if data, err = base64.StdEncoding.DecodeString(in.Content); err != nil {
			return nil, writeOut{}, err
		}
	}
	if len(data) > maxWriteBytes {
		return nil, writeOut{}, fmt.Errorf("content larger than %d bytes: use longarm put", maxWriteBytes)
	}
	conn, err := t.s.conn(in.Worker)
	if err != nil {
		return nil, writeOut{}, err
	}
	defer conn.Close()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if dir := parentDir(in.Path); dir != "" {
		conn.Files().Mkdir(cctx, &pb.MkdirRequest{Path: dir})
	}
	sum := sha256.Sum256(data)
	st, err := conn.Files().Write(cctx)
	if err != nil {
		return nil, writeOut{}, err
	}
	if err := st.Send(&pb.FileWriteRequest{Msg: &pb.FileWriteRequest_Header{Header: &pb.FileWriteHeader{
		Path: in.Path, Size: int64(len(data)), Sha256: sum[:], Mode: in.Mode}}}); err != nil {
		return nil, writeOut{}, err
	}
	for off := 0; off < len(data); off += 256 << 10 {
		end := min(off+256<<10, len(data))
		if err := st.Send(&pb.FileWriteRequest{Msg: &pb.FileWriteRequest_Data{Data: data[off:end]}}); err != nil {
			return nil, writeOut{}, err
		}
	}
	res, err := st.CloseAndRecv()
	if err != nil {
		return nil, writeOut{}, err
	}
	return nil, writeOut{Path: res.AbsPath, Size: res.Size}, nil
}

func parentDir(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return ""
}

type syncIn struct {
	Dir           string `json:"dir,omitempty" jsonschema:"local git checkout (default: the current directory)"`
	Worker        string `json:"worker" jsonschema:"worker: name, node id or unique id prefix"`
	Path          string `json:"path,omitempty" jsonschema:"worktree path on the worker (default <home>/longarm-sync/<repo>)"`
	Ref           string `json:"ref,omitempty" jsonschema:"commit to sync (default HEAD; uncommitted changes only with HEAD)"`
	CommittedOnly bool   `json:"committed_only,omitempty" jsonschema:"leave out uncommitted changes and untracked files"`
}

type syncOut struct {
	Worker         string `json:"worker"`
	Path           string `json:"path"`
	Commit         string `json:"commit"`
	Repo           string `json:"repo"`
	BundleBytes    int64  `json:"bundle_bytes"`
	PatchBytes     int64  `json:"patch_bytes"`
	UntrackedBytes int64  `json:"untracked_bytes"`
}

func (t *mcpTools) sync(ctx context.Context, _ *mcp.CallToolRequest, in syncIn) (*mcp.CallToolResult, syncOut, error) {
	dir := in.Dir
	if dir == "" {
		dir = "."
	}
	ref := in.Ref
	if ref == "" {
		ref = "HEAD"
	}
	conn, err := t.s.conn(in.Worker)
	if err != nil {
		return nil, syncOut{}, err
	}
	defer conn.Close()
	lr, err := openLocalRepo(dir, "")
	if err != nil {
		return nil, syncOut{}, err
	}
	dst := in.Path
	if dst == "" {
		dst = "longarm-sync/" + lr.key
	}
	res, err := lr.upload(ctx, conn, ref, "", !in.CommittedOnly, &pb.SyncHeader{Path: dst}, nil)
	if err != nil {
		return nil, syncOut{}, err
	}
	return nil, syncOut{Worker: in.Worker, Path: res.Path, Commit: res.Commit, Repo: lr.key, BundleBytes: res.BundleBytes,
		PatchBytes: res.PatchBytes, UntrackedBytes: res.UntrackedBytes}, nil
}

// ---------------------------------------------------------------- forward

type forwardIn struct {
	Action    string `json:"action,omitempty" jsonschema:"start (default), list or stop"`
	Worker    string `json:"worker,omitempty" jsonschema:"worker name (start)"`
	Port      string `json:"port,omitempty" jsonschema:"worker port or host:port as seen from the worker (start)"`
	LocalPort int    `json:"local_port,omitempty" jsonschema:"local port (default: the same if free, else any); stop: the port to stop"`
}

type forwardOut struct {
	Forwards []forwardInfo `json:"forwards"`
}

type forwardInfo struct {
	Worker      string `json:"worker"`
	Target      string `json:"target"`
	LocalAddr   string `json:"local_addr"`
	URL         string `json:"url"`
	Connections int64  `json:"connections"`
}

func (t *mcpTools) forward(ctx context.Context, _ *mcp.CallToolRequest, in forwardIn) (*mcp.CallToolResult, forwardOut, error) {
	c, closeFn, err := daemonClient(true)
	if err != nil {
		return nil, forwardOut{}, err
	}
	defer closeFn()
	conv := func(fs []*pb.ForwardInfo) forwardOut {
		out := forwardOut{Forwards: []forwardInfo{}}
		for _, f := range fs {
			out.Forwards = append(out.Forwards, forwardInfo{Worker: f.Worker, Target: f.Target, LocalAddr: f.LocalAddr, URL: f.Url, Connections: f.Connections})
		}
		return out
	}
	switch in.Action {
	case "", "start":
		if in.Worker == "" || in.Port == "" {
			return nil, forwardOut{}, errors.New("start needs worker and port")
		}
		f, err := c.StartForward(ctx, &pb.StartForwardRequest{Worker: in.Worker, Target: in.Port, LocalPort: int32(in.LocalPort)})
		if err != nil {
			return nil, forwardOut{}, err
		}
		return nil, conv([]*pb.ForwardInfo{f}), nil
	case "list":
		r, err := c.ListForwards(ctx, &pb.ListForwardsRequest{})
		if err != nil {
			return nil, forwardOut{}, err
		}
		return nil, conv(r.Forwards), nil
	case "stop":
		which := "all"
		if in.LocalPort != 0 {
			which = fmt.Sprint(in.LocalPort)
		}
		r, err := c.StopForward(ctx, &pb.StopForwardRequest{LocalAddr: which})
		if err != nil {
			return nil, forwardOut{}, err
		}
		return nil, conv(r.Forwards), nil
	}
	return nil, forwardOut{}, fmt.Errorf("unknown action %q", in.Action)
}

// mcpSecretEnv resolves secret_env for MCP tools. The model chooses the
// names, so only variables the operator allowed with LONGARM_MCP_SECRETS
// (comma-separated, set when registering the server) can be used: an agent
// can still misuse an allowed secret through the command it runs, but it
// cannot pull arbitrary variables out of this process.
func mcpSecretEnv(names []string) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	allowed := map[string]bool{}
	for _, n := range strings.Split(os.Getenv("LONGARM_MCP_SECRETS"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			allowed[n] = true
		}
	}
	for _, n := range names {
		local := n
		if _, v, ok := strings.Cut(n, "="); ok {
			local = v
		}
		if !allowed[local] {
			return nil, fmt.Errorf("secret %q is not allowed: list it in LONGARM_MCP_SECRETS when registering longarm mcp (e.g. claude mcp add longarm -e LONGARM_MCP_SECRETS=GH_TOKEN -- longarm mcp)", local)
		}
	}
	return secretEnv(names)
}

// ---------------------------------------------------------------- links

type linkIn struct {
	Action     string `json:"action,omitempty" jsonschema:"create (default), list or remove"`
	On         string `json:"on,omitempty" jsonschema:"create: the worker whose processes use the link (it listens on 127.0.0.1 there)"`
	Target     string `json:"target,omitempty" jsonschema:"create: <worker>:<port> to reach, e.g. backend-box:8080"`
	ListenPort int    `json:"listen_port,omitempty" jsonschema:"create: port on the 'on' worker (default: the target port)"`
	TTLSeconds int    `json:"ttl_seconds,omitempty" jsonschema:"create: lifetime (default: the hub's, 8h)"`
	JobID      string `json:"job_id,omitempty" jsonschema:"create: remove the link when this job on the 'on' worker ends"`
	ID         string `json:"id,omitempty" jsonschema:"remove: the link id"`
}

type linkInfo struct {
	ID         string `json:"id"`
	Owner      string `json:"owner"`
	On         string `json:"on"`
	Address    string `json:"address"` // on the 'on' worker
	Target     string `json:"target"`
	EnvVar     string `json:"env_var"`
	State      string `json:"state"`
	Error      string `json:"error,omitempty"`
	Path       string `json:"path,omitempty"`
	Conns      int64  `json:"connections"`
	BytesSent  int64  `json:"bytes_sent"`
	BytesRecvd int64  `json:"bytes_received"`
	ExpiresAt  string `json:"expires_at"`
	JobID      string `json:"job_id,omitempty"`
}

type linkOut struct {
	Links []linkInfo `json:"links"`
}

func toLinkInfo(l *pb.PortLink) linkInfo {
	s := l.Status
	return linkInfo{ID: l.Id, Owner: l.Owner, On: l.From, Address: fmt.Sprintf("127.0.0.1:%d", l.ListenPort),
		Target: fmt.Sprintf("%s:%d", l.To, l.Port), EnvVar: linkEnvName(l), State: s.GetState(), Error: s.GetError(), Path: s.GetPath(),
		Conns: s.GetConnections(), BytesSent: s.GetBytesSent(), BytesRecvd: s.GetBytesReceived(),
		ExpiresAt: l.ExpiresAt.AsTime().UTC().Format(time.RFC3339), JobID: l.JobId}
}

func (t *mcpTools) link(ctx context.Context, _ *mcp.CallToolRequest, in linkIn) (*mcp.CallToolResult, linkOut, error) {
	h := t.s.hub()
	if h == nil {
		return nil, linkOut{}, errors.New("port links need a hub (longarm join)")
	}
	out := linkOut{Links: []linkInfo{}}
	switch in.Action {
	case "", "create":
		to, port, err := parseLinkTarget(in.Target)
		if err != nil {
			return nil, out, err
		}
		if in.ListenPort < 0 || in.ListenPort > 65535 {
			return nil, out, fmt.Errorf("listen_port %d: want 1-65535", in.ListenPort)
		}
		if in.On == "" {
			return nil, out, errors.New("create needs on (the worker to listen on) and target (<worker>:<port>)")
		}
		req := &pb.CreatePortLinkRequest{From: in.On, To: to, Port: port, ListenPort: int32(in.ListenPort), JobId: in.JobID}
		if in.TTLSeconds > 0 {
			req.Ttl = durationpb.New(time.Duration(in.TTLSeconds) * time.Second)
		}
		if err := resolveLinkEnds(ctx, t.s, req); err != nil {
			return nil, out, err
		}
		if err := signLink(t.s.cfg.Dir, req); err != nil {
			return nil, out, err
		}
		l, err := h.Fleet().CreatePortLink(ctx, req)
		if err != nil {
			return nil, out, err
		}
		out.Links = append(out.Links, toLinkInfo(l))
	case "list":
		r, err := h.Fleet().ListPortLinks(ctx, &pb.ListPortLinksRequest{})
		if err != nil {
			return nil, out, err
		}
		for _, l := range r.Links {
			out.Links = append(out.Links, toLinkInfo(l))
		}
	case "remove":
		if in.ID == "" {
			return nil, out, errors.New("remove needs id")
		}
		l, err := h.Fleet().RemovePortLink(ctx, &pb.RemovePortLinkRequest{Id: in.ID})
		if err != nil {
			return nil, out, err
		}
		out.Links = append(out.Links, toLinkInfo(l))
	default:
		return nil, out, fmt.Errorf("unknown action %q", in.Action)
	}
	return nil, out, nil
}
