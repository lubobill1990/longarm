// Package server implements longarmd's gRPC API (proto/longarm/v1) on top of the
// supervisor, the metrics sampler and app-level mTLS.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/conntrack"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/metrics"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/supervisor"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// AddListener records a listener that came up after start (thread-safe).
func (s *Server) AddListener(name string) {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	s.late = append(s.late, name)
}

// RemoveListener forgets a listener added with AddListener (a tailscale
// leg that stopped or switched tailnets).
func (s *Server) RemoveListener(name string) {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	for i, l := range s.late {
		if l == name {
			s.late = append(s.late[:i:i], s.late[i+1:]...)
			return
		}
	}
}

// ListenerNames are Listeners plus the ones added later.
func (s *Server) ListenerNames() []string {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	return append(append([]string(nil), s.Listeners...), s.late...)
}

// Server holds everything the services need.
type Server struct {
	Name      string // worker name (from its certificate)
	Version   string
	DataDir   string
	StartedAt time.Time
	// Listeners are the listen URLs set before serving; listeners that come
	// up later (tailscale) are added with AddListener.
	Listeners []string
	Mgr       *supervisor.Manager // jobs
	Sessions  *supervisor.Manager // interactive PTY sessions
	Sampler   *metrics.Sampler
	Audit     *Audit
	Log       *slog.Logger
	// ExecWrapper is prepended to Exec argv (Windows: longarmd conhost --).
	ExecWrapper []string
	// Shutdown is invoked (asynchronously) by Admin.Shutdown.
	Shutdown func()
	// LogMaxBytes caps out.log of new jobs and sessions (0 = unlimited).
	LogMaxBytes int64
	// WS is the workspace store (mirrors, worktrees).
	WS *workspace.Store

	lmu  sync.Mutex
	late []string
	// SSH is the built-in sshd behind Forward.Ssh (nil = disabled) and
	// SSHHostKey its public key (authorized_keys format) for Node.Info.
	SSH        SSHHandler
	SSHHostKey string
	// Revoked lists revoked certificates (pushed by the hub); calls with
	// them are refused.
	Revoked *pki.RevocationList
	// Conns tracks client connections so revoked ones can be closed.
	Conns *conntrack.Tracker
	// GC runs a garbage collection for Admin.Gc.
	GC func(ctx context.Context, req *pb.GcRequest) (*pb.GcResponse, error)
}

// GRPC builds the gRPC server with mTLS and the authorization interceptors.
func (s *Server) GRPC(tlsCfg *tls.Config) *grpc.Server {
	creds := credentials.NewTLS(tlsCfg)
	if s.Conns != nil {
		creds = s.Conns.Wrap(creds)
	}
	g := grpc.NewServer(
		grpc.Creds(creds),
		grpc.ChainUnaryInterceptor(s.unaryAuth),
		grpc.ChainStreamInterceptor(s.streamAuth),
		// Long-lived Wait/Logs streams cross NATs: keep them warm and allow
		// client pings.
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 20 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	pb.RegisterNodeServer(g, &nodeServer{s: s})
	pb.RegisterExecServer(g, &execServer{s: s})
	pb.RegisterJobsServer(g, &jobsServer{s: s})
	pb.RegisterMetricsServer(g, &metricsServer{s: s})
	pb.RegisterAdminServer(g, &adminServer{s: s})
	pb.RegisterSessionsServer(g, &sessionsServer{s: s})
	pb.RegisterForwardServer(g, &forwardServer{s: s})
	pb.RegisterFilesServer(g, &filesServer{s: s})
	pb.RegisterWorkspaceServer(g, &workspaceServer{s: s})
	return g
}

// ---------------------------------------------------------------- Node

type nodeServer struct {
	pb.UnimplementedNodeServer
	s *Server
}

func (n *nodeServer) Info(ctx context.Context, _ *pb.InfoRequest) (*pb.InfoResponse, error) {
	host, _ := os.Hostname()
	id := IdentityFrom(ctx)
	return &pb.InfoResponse{
		Name: n.s.Name, Version: n.s.Version, Os: runtime.GOOS, Arch: runtime.GOARCH,
		Hostname: host, NumCpu: int32(runtime.NumCPU()), DataDir: n.s.DataDir,
		StartedAt: timestamppb.New(n.s.StartedAt), CallerName: id.Name, CallerRole: string(id.Role),
		Listeners: n.s.ListenerNames(), SshHostKey: n.s.SSHHostKey,
	}, nil
}

// ---------------------------------------------------------------- Admin

type adminServer struct {
	pb.UnimplementedAdminServer
	s *Server
}

func (a *adminServer) Shutdown(ctx context.Context, _ *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	a.s.Audit.Log(IdentityFrom(ctx), pb.Admin_Shutdown_FullMethodName, nil)
	a.s.Log.Info("shutdown requested", "by", IdentityFrom(ctx).String())
	if a.s.Shutdown != nil {
		go func() {
			time.Sleep(100 * time.Millisecond) // let the response go out
			a.s.Shutdown()
		}()
	}
	return &pb.ShutdownResponse{}, nil
}

// ---------------------------------------------------------------- Metrics

type metricsServer struct {
	pb.UnimplementedMetricsServer
	s *Server
}

// SnapshotToPB converts a metrics snapshot to the API type.
func SnapshotToPB(m metrics.Snapshot) *pb.MetricsSnapshot {
	out := &pb.MetricsSnapshot{
		Time: timestamppb.New(m.Time), CpuPercent: m.CPUPercent, Threads: int32(m.Threads),
		MemTotalBytes: m.MemTotal, MemUsedBytes: m.MemUsed, JobsRunning: int32(m.JobsRunning),
		UptimeSeconds: int64(m.Uptime / time.Second), Nproc: int32(m.NProc),
		Agents:        map[string]*pb.AgentStat{},
		DiskFreeBytes: m.DiskFree, DiskTotalBytes: m.DiskTotal,
	}
	for k, v := range m.Agents {
		out.Agents[k] = &pb.AgentStat{Sessions: int32(v.Sessions), Processes: int32(v.Processes), MemBytes: v.MemBytes}
	}
	conv := func(gs []metrics.ProcGroup) []*pb.ProcGroup {
		r := make([]*pb.ProcGroup, len(gs))
		for i, g := range gs {
			r[i] = &pb.ProcGroup{Name: g.Name, Count: int32(g.Count), MemBytes: g.MemBytes, CpuPercent: g.CPUPercent}
		}
		return r
	}
	out.TopCpu, out.TopMem = conv(m.TopCPU), conv(m.TopMem)
	return out
}

func (m *metricsServer) Snapshot(ctx context.Context, _ *pb.MetricsRequest) (*pb.MetricsSnapshot, error) {
	snap, err := m.s.Sampler.Latest(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return SnapshotToPB(snap), nil
}

func (m *metricsServer) Stream(req *pb.MetricsStreamRequest, st pb.Metrics_StreamServer) error {
	interval := 2 * time.Second
	if req.Interval != nil {
		interval = req.Interval.AsDuration()
	}
	if interval < 500*time.Millisecond {
		interval = 500 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		snap, err := m.s.Sampler.Latest(st.Context())
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		if err := st.Send(SnapshotToPB(snap)); err != nil {
			return err
		}
		select {
		case <-st.Context().Done():
			return nil
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------- Jobs

type jobsServer struct {
	pb.UnimplementedJobsServer
	s *Server
}

var stateToPB = map[jobs.State]pb.JobState{
	jobs.Queued: pb.JobState_JOB_STATE_QUEUED, jobs.Assigned: pb.JobState_JOB_STATE_ASSIGNED,
	jobs.Preparing: pb.JobState_JOB_STATE_PREPARING, jobs.Running: pb.JobState_JOB_STATE_RUNNING,
	jobs.Done: pb.JobState_JOB_STATE_DONE, jobs.Failed: pb.JobState_JOB_STATE_FAILED,
	jobs.Killed: pb.JobState_JOB_STATE_KILLED, jobs.Lost: pb.JobState_JOB_STATE_LOST,
}

// StateFromPB maps a proto state to the job model.
func StateFromPB(s pb.JobState) (jobs.State, bool) {
	for k, v := range stateToPB {
		if v == s {
			return k, true
		}
	}
	return "", false
}

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// JobToPB converts a supervisor snapshot to the API type.
func JobToPB(j supervisor.Job) *pb.Job {
	out := &pb.Job{
		Spec: &pb.JobSpec{
			Id: j.Spec.ID, Name: j.Spec.Name, Argv: j.Spec.Argv, Cwd: j.Spec.Cwd, Env: j.Spec.Env,
			SecretEnvNames: j.Spec.SecretEnvNames, MemLimitBytes: j.Spec.MemLimitBytes, Labels: j.Spec.Labels,
			SubmittedBy: j.Spec.SubmittedBy,
		},
		State: stateToPB[j.Status.State], Pid: int32(j.Status.PID), ShimPid: int32(j.Status.ShimPID),
		CreatedAt: ts(j.Status.CreatedAt), StartedAt: ts(j.Status.StartedAt), EndedAt: ts(j.Status.EndedAt),
		LogSize: j.LogSize, Reason: j.Status.Reason, Orphaned: j.Status.Orphaned, Workdir: j.Status.Workdir,
	}
	if w := j.Spec.Workspace; w != nil {
		out.Spec.Workspace = &pb.JobWorkspace{Repo: w.Repo, Url: w.URL, Ref: w.Ref, Commit: w.Commit, StagingId: w.StagingID}
	}
	if j.Status.ExitCode != nil {
		out.HasExitCode, out.ExitCode = true, int32(*j.Status.ExitCode)
	}
	return out
}

func MgrErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, supervisor.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, supervisor.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, supervisor.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	}
	return status.Error(codes.Internal, err.Error())
}

func (j *jobsServer) Submit(ctx context.Context, req *pb.SubmitRequest) (*pb.Job, error) {
	if req.Spec == nil {
		return nil, status.Error(codes.InvalidArgument, "missing spec")
	}
	id := IdentityFrom(ctx)
	spec := jobs.Spec{
		ID: req.Spec.Id, Name: req.Spec.Name, Argv: req.Spec.Argv, Cwd: req.Spec.Cwd, Env: req.Spec.Env,
		MemLimitBytes: req.Spec.MemLimitBytes, Labels: req.Spec.Labels, SubmittedBy: id.String(),
		LogMaxBytes: j.s.LogMaxBytes,
	}
	if w := req.Spec.Workspace; w != nil {
		if !workspace.ValidRepo(w.Repo) {
			return nil, status.Errorf(codes.InvalidArgument, "bad workspace repo %q", w.Repo)
		}
		spec.Workspace = &jobs.Workspace{Repo: w.Repo, URL: w.Url, Ref: w.Ref, Commit: w.Commit, StagingID: w.StagingId}
	}
	job, err := j.s.Mgr.Submit(ctx, spec, req.SecretEnv)
	j.s.Audit.Log(id, pb.Jobs_Submit_FullMethodName, map[string]any{
		"job": spec.ID, "argv": spec.Argv, "cwd": spec.Cwd, "env_keys": keys(spec.Env),
		"secret_keys": keys(req.SecretEnv), "error": errString(err),
	})
	if err != nil {
		return nil, MgrErr(err)
	}
	return JobToPB(job), nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (j *jobsServer) Get(_ context.Context, req *pb.GetJobRequest) (*pb.Job, error) {
	job, err := j.s.Mgr.Get(req.Id)
	if err != nil {
		return nil, MgrErr(err)
	}
	return JobToPB(job), nil
}

func (j *jobsServer) List(_ context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	var states []jobs.State
	for _, s := range req.States {
		if st, ok := StateFromPB(s); ok {
			states = append(states, st)
		}
	}
	out := &pb.ListJobsResponse{}
	for _, job := range j.s.Mgr.List(states, int(req.Limit)) {
		out.Jobs = append(out.Jobs, JobToPB(job))
	}
	return out, nil
}

const logChunk = 64 << 10

func (j *jobsServer) Logs(req *pb.LogsRequest, st pb.Jobs_LogsServer) error {
	return StreamLogs(j.s.Mgr, req, st)
}

// StreamLogs serves Jobs.Logs and Sessions.Logs (and longarmd's local API).
func StreamLogs(mgr *supervisor.Manager, req *pb.LogsRequest, st interface {
	Send(*pb.LogChunk) error
	Context() context.Context
}) error {
	dir, err := mgr.Dir(req.Id)
	if err != nil {
		return MgrErr(err)
	}
	ctx := st.Context()
	off := req.Offset
	tick := time.NewTicker(100 * time.Millisecond) // log growth is not a state change
	defer tick.Stop()
	for {
		changed := mgr.Changed()
		job, err := mgr.Get(req.Id)
		if err != nil {
			return MgrErr(err)
		}
		terminal := job.Status.State.Terminal() // checked before reading: the shim
		// writes all output before the terminal status.
		data, start, _, err := dir.ReadLog(off, logChunk)
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		off = start
		if len(data) > 0 {
			if err := st.Send(&pb.LogChunk{Offset: start, Data: data}); err != nil {
				return err
			}
			off += int64(len(data))
			continue
		}
		if !req.Follow || terminal {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		case <-tick.C:
		}
	}
}

func (j *jobsServer) Wait(req *pb.WaitRequest, st pb.Jobs_WaitServer) error {
	ctx := st.Context()
	var lastSeq uint64
	var lastState jobs.State
	first := true
	for {
		changed := j.s.Mgr.Changed()
		job, err := j.s.Mgr.Get(req.Id)
		if err != nil {
			return MgrErr(err)
		}
		if first || job.Status.Seq != lastSeq || job.Status.State != lastState {
			if err := st.Send(JobToPB(job)); err != nil {
				return err
			}
			first, lastSeq, lastState = false, job.Status.Seq, job.Status.State
		}
		if job.Status.State.Terminal() {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		}
	}
}

func (j *jobsServer) Kill(ctx context.Context, req *pb.KillRequest) (*pb.Job, error) {
	job, err := j.s.Mgr.Kill(ctx, req.Id)
	j.s.Audit.Log(IdentityFrom(ctx), pb.Jobs_Kill_FullMethodName, map[string]any{"job": req.Id, "error": errString(err)})
	if err != nil {
		return nil, MgrErr(err)
	}
	return JobToPB(job), nil
}

func (a *adminServer) Gc(ctx context.Context, req *pb.GcRequest) (*pb.GcResponse, error) {
	if a.s.GC == nil {
		return nil, status.Error(codes.Unimplemented, "gc not configured")
	}
	resp, err := a.s.GC(ctx, req)
	a.s.Audit.Log(IdentityFrom(ctx), pb.Admin_Gc_FullMethodName, map[string]any{
		"dry_run": req.DryRun, "removed": len(resp.GetRemoved()), "freed": resp.GetFreedBytes(), "error": errString(err),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return resp, nil
}
