package longarmd

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/fwcheck"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/localapi"
	"github.com/lubobill1990/longarm/internal/server"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// LocalAddrFile records the local control API's channel address (named
// pipe / unix socket) in the data dir.
const LocalAddrFile = "longarmd.local"

// localWorker implements longarmd's local control API (DESIGN §11.3). CLI
// equivalents: longarmd status, longarmd drain/undrain, longarmd toolchain apply,
// longarmd events.
type localWorker struct {
	pb.UnimplementedLocalWorkerServer
	srv   *server.Server
	agent *hubAgent
	st    *state
	fw    fwcheck.Cache // Windows Firewall verdict for this longarmd.exe
}

func (l *localWorker) Status(ctx context.Context, _ *pb.LocalStatusRequest) (*pb.WorkerStatus, error) {
	s := &pb.WorkerStatus{
		Name: l.srv.Name, Version: version.Version, DataDir: l.srv.DataDir, Endpoints: l.srv.ListenerNames(),
		Hub: l.agent.Link(), State: l.agent.State(), JobsRunning: int32(l.st.spaces.RunningJobs()),
		SessionsRunning: int32(l.st.spaces.RunningSessions()), RevokedCerts: int32(l.agent.revoked.Len()),
		Pid: int32(os.Getpid()), CertNotAfter: timestamppb.New(l.agent.live.Leaf().NotAfter),
		Limits: l.agent.Limits(), Os: runtime.GOOS, Arch: runtime.GOARCH, Firewall: l.fw.Get(),
	}
	if l.agent.tc != nil {
		s.Toolchain = l.agent.tc.Status()
	}
	if m, err := l.srv.Sampler.Latest(ctx); err == nil {
		s.Metrics = server.SnapshotToPB(m)
	}
	return s, nil
}

func (l *localWorker) SetDrain(ctx context.Context, req *pb.LocalDrainRequest) (*pb.WorkerStatus, error) {
	if err := l.agent.SetLocalDrain(req.Draining, req.Reason); err != nil {
		return nil, err
	}
	l.srv.Log.Info("local drain", "draining", req.Draining, "reason", req.Reason)
	return l.Status(ctx, nil)
}

func (l *localWorker) ToolchainApply(ctx context.Context, _ *pb.ToolchainApplyRequest) (*pb.ToolchainReport, error) {
	if l.agent.tc == nil {
		return &pb.ToolchainReport{State: "none"}, nil
	}
	l.agent.tc.Kick()
	return l.agent.tc.Status(), nil
}

func (l *localWorker) Events(_ *pb.LocalEventsRequest, st pb.LocalWorker_EventsServer) error {
	ch, stop := l.agent.watch()
	defer stop()
	for {
		select {
		case <-st.Context().Done():
			return nil
		case ev := <-ch:
			if err := st.Send(ev); err != nil {
				return err
			}
		}
	}
}

// gateway is the JSON/SSE face of the same operations:
//
//	GET  /v1/status
//	POST /v1/drain            {"draining": true, "reason": "..."}
//	POST /v1/toolchain/apply
//	GET  /v1/events           (SSE)
//	GET  /v1/jobs?state=running&limit=50
//	GET  /v1/jobs/{id}/logs?offset=0&follow=1  (SSE, event "log": LogChunk)
//	POST /v1/jobs/{id}/kill
//	POST /v1/limits           {"max_jobs": 4, "reserved_mem_gb": 16}
//	POST /v1/leave            {"reason": "..."}
func (l *localWorker) gateway() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		req := &pb.ListJobsRequest{}
		q := r.URL.Query()
		for _, s := range q["state"] {
			if v, ok := pb.JobState_value["JOB_STATE_"+strings.ToUpper(s)]; ok {
				req.States = append(req.States, pb.JobState(v))
			}
		}
		if n, err := strconv.Atoi(q.Get("limit")); err == nil {
			req.Limit = int32(n)
		}
		s, err := l.ListJobs(r.Context(), req)
		localapi.WriteProto(w, s, err)
	})
	mux.HandleFunc("GET /v1/jobs/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		off, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
		req := &pb.LogsRequest{Id: r.PathValue("id"), Offset: off, Follow: q.Get("follow") == "1" || q.Get("follow") == "true"}
		localapi.SSEStream(w, r, "log", func(ctx context.Context, send func(proto.Message) error) error {
			sp, _, err := l.st.spaces.FindJob(req.Id)
			if err != nil {
				return server.MgrErr(err)
			}
			return server.StreamLogs(sp.Jobs, req, sendStream[*pb.LogChunk]{ctx: ctx, send: send})
		})
	})
	mux.HandleFunc("POST /v1/jobs/{id}/kill", func(w http.ResponseWriter, r *http.Request) {
		j, err := l.KillJob(r.Context(), &pb.KillRequest{Id: r.PathValue("id")})
		localapi.WriteProto(w, j, err)
	})
	mux.HandleFunc("POST /v1/limits", func(w http.ResponseWriter, r *http.Request) {
		req := &pb.LocalLimits{}
		if err := localapi.ReadProto(r, req); err != nil {
			localapi.WriteProto(w, nil, err)
			return
		}
		s, err := l.SetLimits(r.Context(), req)
		localapi.WriteProto(w, s, err)
	})
	mux.HandleFunc("POST /v1/leave", func(w http.ResponseWriter, r *http.Request) {
		req := &pb.LeaveRequest{}
		if err := localapi.ReadProto(r, req); err != nil {
			localapi.WriteProto(w, nil, err)
			return
		}
		s, err := l.Leave(r.Context(), req)
		localapi.WriteProto(w, s, err)
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		s, err := l.Status(r.Context(), nil)
		localapi.WriteProto(w, s, err)
	})
	mux.HandleFunc("POST /v1/drain", func(w http.ResponseWriter, r *http.Request) {
		req := &pb.LocalDrainRequest{}
		if err := localapi.ReadProto(r, req); err != nil {
			localapi.WriteProto(w, nil, err)
			return
		}
		s, err := l.SetDrain(r.Context(), req)
		localapi.WriteProto(w, s, err)
	})
	mux.HandleFunc("POST /v1/toolchain/apply", func(w http.ResponseWriter, r *http.Request) {
		s, err := l.ToolchainApply(r.Context(), nil)
		localapi.WriteProto(w, s, err)
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		ch, stop := l.agent.watch()
		defer stop()
		localapi.SSE(w, r, "event", func(ctx context.Context) (proto.Message, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case ev := <-ch:
				return ev, nil
			}
		})
	})
	return mux
}

func (l *localWorker) grpcServer() *grpc.Server {
	g := grpc.NewServer()
	pb.RegisterLocalWorkerServer(g, l)
	return g
}

func (l *localWorker) ListJobs(ctx context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	var states []jobs.State
	for _, s := range req.States {
		if st, ok := server.StateFromPB(s); ok {
			states = append(states, st)
		}
	}
	out := &pb.ListJobsResponse{}
	for _, j := range l.st.spaces.ListJobs(states, int(req.Limit)) {
		out.Jobs = append(out.Jobs, server.JobToPB(j))
	}
	return out, nil
}

// The local control API is the machine's (its channel is LocalSystem's /
// root's, or the service account's): like an admin, it sees every user's
// jobs.

func (l *localWorker) JobLogs(req *pb.LogsRequest, st pb.LocalWorker_JobLogsServer) error {
	sp, _, err := l.st.spaces.FindJob(req.Id)
	if err != nil {
		return server.MgrErr(err)
	}
	return server.StreamLogs(sp.Jobs, req, st)
}

// KillJob kills one of this worker's jobs (audited as the local user).
func (l *localWorker) KillJob(ctx context.Context, req *pb.KillRequest) (*pb.Job, error) {
	sp, _, err := l.st.spaces.FindJob(req.Id)
	if err != nil {
		return nil, server.MgrErr(err)
	}
	j, err := sp.Jobs.Kill(ctx, req.Id)
	l.srv.Audit.LogAs("local", pb.Jobs_Kill_FullMethodName, map[string]any{"job": req.Id, "error": errText(err)})
	if err != nil {
		return nil, server.MgrErr(err)
	}
	return server.JobToPB(j), nil
}

func (l *localWorker) SetLimits(ctx context.Context, req *pb.LocalLimits) (*pb.WorkerStatus, error) {
	if err := l.agent.SetLimits(req); err != nil {
		return nil, err
	}
	l.srv.Log.Info("local limits", "max_jobs", req.MaxJobs, "reserved_mem_gb", req.ReservedMemGb)
	return l.Status(ctx, nil)
}

func (l *localWorker) Leave(ctx context.Context, req *pb.LeaveRequest) (*pb.LeaveResponse, error) {
	msg, err := l.agent.leave(ctx, req.Reason, req.Force)
	if err != nil {
		return nil, err
	}
	return &pb.LeaveResponse{Message: msg}, nil
}

// sendStream adapts an SSE sender to a gRPC server stream's Send/Context.
type sendStream[T proto.Message] struct {
	ctx  context.Context
	send func(proto.Message) error
}

func (s sendStream[T]) Send(m T) error           { return s.send(m) }
func (s sendStream[T]) Context() context.Context { return s.ctx }
