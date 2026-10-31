package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/steer"
	"github.com/lubobill1990/longarm/internal/supervisor"
	"github.com/lubobill1990/longarm/internal/userspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The terminal a steerable run starts with (attached clients resize it).
const steerCols, steerRows = 120, 40

// steerSpec makes spec a steerable run when the request asks for one
// (docs/steerable-runs.md): the agent on a pseudo terminal.
// copilotHome: the job gets a private COPILOT_HOME (its hooks go there).
func (j *jobsServer) steerSpec(spec *jobs.Spec, req *pb.SubmitRequest, copilotHome bool) error {
	st := req.Spec.GetSteer()
	if st == nil {
		return nil
	}
	if st.Agent != steer.Claude && st.Agent != steer.Copilot {
		return status.Errorf(codes.InvalidArgument, "steer: agent %q (want claude or copilot)", st.Agent)
	}
	if got := steer.AgentOf(spec.Argv); got != st.Agent {
		return status.Errorf(codes.InvalidArgument, "steer: the command does not run %s", st.Agent)
	}
	if j.s.perUser() {
		return status.Error(codes.FailedPrecondition, "steerable runs are not supported on workers with per-user isolation yet")
	}
	if len(req.Stdin) > 0 {
		return status.Error(codes.InvalidArgument, "steer: a steerable run takes no stdin")
	}
	if st.Agent == steer.Copilot && (!copilotHome || hasEnv("COPILOT_HOME", spec.Env, req.SecretEnv)) {
		return status.Error(codes.InvalidArgument,
			"steer: a steerable copilot run needs forwarded Copilot credentials (a private COPILOT_HOME, where its hooks go; --agent-auth copilot) and no COPILOT_HOME of its own")
	}
	spec.Steer = &jobs.Steer{Agent: st.Agent, UntilExit: st.UntilExit}
	spec.Pty, spec.Cols, spec.Rows = true, steerCols, steerRows
	return nil
}

// steerable finds a steerable job the caller may control (attach, send).
func (ss *sessionsServer) steerable(ctx context.Context, id string) (*userspace.Space, supervisor.Job, error) {
	sp, j, err := ss.s.findJob(ctx, id, true)
	if err != nil {
		return nil, j, err
	}
	if j.Spec.Steer == nil || !j.Spec.Pty {
		return nil, j, status.Errorf(codes.FailedPrecondition, "job %s is not a steerable run (longarm run --steerable)", id)
	}
	return sp, j, nil
}
