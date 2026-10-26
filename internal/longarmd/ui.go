package longarmd

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/server"
	"github.com/lubobill1990/longarm/internal/supervisor"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// This file holds what the UI (longarm-tray's worker tab, the hub's web panel)
// needs from longarmd beyond P1: local limits, hub-relayed job requests and
// leaving the fleet. Every operation is also a CLI command
// (longarmd limits|jobs|logs|kill|leave, longarm admin kill|logs|run|toolchain-apply).

// LimitsFile persists `longarmd limits` across restarts.
const LimitsFile = "limits.json"

func loadLimits(dataDir string) *pb.LocalLimits {
	l := &pb.LocalLimits{}
	if b, err := os.ReadFile(filepath.Join(dataDir, LimitsFile)); err == nil {
		var v struct {
			MaxJobs       int32   `json:"max_jobs"`
			ReservedMemGB float64 `json:"reserved_mem_gb"`
		}
		if json.Unmarshal(b, &v) == nil {
			l.MaxJobs, l.ReservedMemGb = v.MaxJobs, v.ReservedMemGB
		}
	}
	return l
}

// Limits returns the local limits.
func (a *hubAgent) Limits() *pb.LocalLimits {
	a.mu.Lock()
	defer a.mu.Unlock()
	return &pb.LocalLimits{MaxJobs: a.limits.GetMaxJobs(), ReservedMemGb: a.limits.GetReservedMemGb()}
}

// SetLimits implements `longarmd limits`: persisted, reported to the hub (its
// scheduler honours them).
func (a *hubAgent) SetLimits(l *pb.LocalLimits) error {
	if l.MaxJobs < 0 || l.ReservedMemGb < 0 || math.IsNaN(l.ReservedMemGb) || math.IsInf(l.ReservedMemGb, 0) || l.ReservedMemGb > 1<<20 {
		return status.Error(codes.InvalidArgument, "limits must be >= 0")
	}
	a.limitsMu.Lock() // file and memory change together
	defer a.limitsMu.Unlock()
	b, _ := json.Marshal(map[string]any{"max_jobs": l.MaxJobs, "reserved_mem_gb": l.ReservedMemGb})
	if err := os.WriteFile(filepath.Join(a.dataDir, LimitsFile), b, 0o600); err != nil {
		return err
	}
	a.mu.Lock()
	a.limits = &pb.LocalLimits{MaxJobs: l.MaxJobs, ReservedMemGb: l.ReservedMemGb}
	st := a.stateLocked()
	a.mu.Unlock()
	a.send(&pb.NodeReport{Msg: &pb.NodeReport_State{State: st}}, false)
	a.notify(&pb.LocalEvent{Kind: "limits", Message: fmt.Sprintf("max jobs %d, reserved memory %.1f GB", l.MaxJobs, l.ReservedMemGb)})
	return nil
}

// allowRemote reports whether a hub request of kind ("control" or
// "submit") is allowed by [hub] remote_jobs.
func (a *hubAgent) allowRemote(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(a.remoteJobs)) {
	case "none", "off", "false":
		return false
	case "all":
		return true
	default: // "", "control"
		return kind == "control"
	}
}

// hubLogMax bounds one log reply (the hub session is shared with metrics).
const hubLogMax = 1 << 20

// hubRequest serves a job request from the hub (panel / longarm admin) and
// answers with a reply carrying the same request id.
func (a *hubAgent) hubRequest(cmd *pb.HubCommand) {
	rep := &pb.HubReply{}
	fail := func(err error) {
		st := status.Convert(err)
		rep.Error, rep.Code = st.Message(), int32(st.Code())
	}
	denied := func(what string) error {
		return status.Errorf(codes.PermissionDenied, "%s through the hub is disabled on this worker ([hub] remote_jobs = %q)", what, a.remoteJobs)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch x := cmd.Msg.(type) {
	case *pb.HubCommand_KillJob:
		rep.RequestId = x.KillJob.RequestId
		if !a.allowRemote("control") {
			fail(denied("kill"))
			break
		}
		var j supervisor.Job
		sp, _, err := a.spaces.FindJob(x.KillJob.Id)
		if err == nil {
			j, err = sp.Jobs.Kill(ctx, x.KillJob.Id)
		}
		a.audit.LogAs(x.KillJob.By+" via hub", pb.Jobs_Kill_FullMethodName, map[string]any{"job": x.KillJob.Id, "error": errText(err)})
		if err != nil {
			fail(jobErr(err))
			break
		}
		rep.Job = a.jobForHub(j)
	case *pb.HubCommand_LogRequest:
		r := x.LogRequest
		rep.RequestId = r.RequestId
		if !a.allowRemote("control") {
			fail(denied("reading logs"))
			break
		}
		sp, j, err := a.spaces.FindJob(r.Id)
		if err != nil {
			fail(jobErr(err))
			break
		}
		terminal := j.Status.State.Terminal() // before reading: output precedes the terminal status
		dir, err := sp.Jobs.Dir(r.Id)
		if err != nil {
			fail(jobErr(err))
			break
		}
		max := int(r.MaxBytes)
		if max <= 0 {
			max = 64 << 10
		}
		max = min(max, hubLogMax)
		data, start, size, err := dir.ReadLog(r.Offset, max)
		if err != nil {
			fail(status.Error(codes.Internal, err.Error()))
			break
		}
		rep.Log = &pb.LogChunk{Offset: start, Data: data}
		rep.LogSize = size
		rep.LogEof = terminal && start+int64(len(data)) >= size
	case *pb.HubCommand_SubmitJob:
		r := x.SubmitJob
		rep.RequestId = r.RequestId
		if !a.allowRemote("submit") {
			fail(denied("submitting jobs"))
			break
		}
		if r.Spec == nil || len(r.Spec.Argv) == 0 {
			fail(status.Error(codes.InvalidArgument, "missing command"))
			break
		}
		spec := jobs.Spec{ID: r.Spec.Id, Name: r.Spec.Name, Argv: r.Spec.Argv, Cwd: r.Spec.Cwd, Env: r.Spec.Env,
			Labels: r.Spec.Labels, SubmittedBy: r.By, LogMaxBytes: a.logMaxBytes}
		owner := r.Owner
		if owner == "" { // an older hub: the requester's name
			owner = spec.OwnerName()
		}
		j, err := a.srv.SubmitAs(ctx, owner, spec, nil)
		a.audit.LogAs(r.By+" via hub", pb.Jobs_Submit_FullMethodName, map[string]any{
			"job": spec.ID, "argv": spec.Argv, "cwd": spec.Cwd, "error": errText(err)})
		if err != nil {
			fail(jobErr(err))
			break
		}
		rep.Job = a.jobForHub(j)
	case *pb.HubCommand_ToolchainApply:
		rep.RequestId = x.ToolchainApply.RequestId
		if !a.allowRemote("control") {
			fail(denied("toolchain apply"))
			break
		}
		if a.tc == nil {
			fail(status.Error(codes.FailedPrecondition, "no toolchain reconciler on this worker"))
			break
		}
		a.audit.LogAs(x.ToolchainApply.By+" via hub", "toolchain/apply", nil)
		a.tc.Kick()
	default:
		return
	}
	a.send(&pb.NodeReport{Msg: &pb.NodeReport_Reply{Reply: rep}}, false)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// jobErr maps supervisor errors to gRPC status errors.
func jobErr(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	return server.MgrErr(err)
}

// ---------------------------------------------------------------- leave

// leave is the local API's Leave: unregister, then stop longarmd (jobs keep
// running in their shims; the service manager may restart longarmd, which then
// has no certificate and exits).
func (a *hubAgent) leave(ctx context.Context, reason string, force bool) (string, error) {
	msg, err := Leave(ctx, a.dataDir, LeaveOptions{Reason: reason, Force: force, locked: true, logout: a.tailnet.logoutAll})
	if err != nil {
		return "", err
	}
	a.log.Info("left the fleet; stopping", "hub", a.addr, "message", msg)
	a.notify(&pb.LocalEvent{Kind: "hub", Message: "left the fleet: " + msg})
	if a.stop != nil {
		go func() {
			time.Sleep(300 * time.Millisecond) // let the reply go out
			a.stop()
		}()
	}
	return msg, nil
}
