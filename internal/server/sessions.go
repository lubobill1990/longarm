package server

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/attach"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/supervisor"
	"github.com/lubobill1990/longarm/internal/userspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// DefaultSessionName is used when AttachOpen.name is empty.
const DefaultSessionName = "default"

type sessionsServer struct {
	pb.UnimplementedSessionsServer
	s *Server

	mu       sync.Mutex
	attached map[string]int
}

// DefaultShell is the command of a session opened without argv: pwsh (then
// Windows PowerShell, then cmd) on Windows, $SHELL (else /bin/sh) on Unix.
func DefaultShell() []string {
	if runtime.GOOS == "windows" {
		for _, c := range []string{"pwsh.exe", "powershell.exe"} {
			if p, err := exec.LookPath(c); err == nil {
				return []string{p, "-NoLogo"}
			}
		}
		return []string{"cmd.exe"}
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return []string{sh}
	}
	return []string{"/bin/sh"}
}

func (ss *sessionsServer) count(name string, d int) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.attached == nil {
		ss.attached = map[string]int{}
	}
	ss.attached[name] += d
	if ss.attached[name] <= 0 {
		delete(ss.attached, name)
	}
}

// attachKey counts attached clients per space and session.
func attachKey(sp *userspace.Space, name string) string { return sp.Root + "\x00" + name }

func (ss *sessionsServer) toPB(sp *userspace.Space, j supervisor.Job) *pb.Session {
	ss.mu.Lock()
	n := ss.attached[attachKey(sp, j.Spec.ID)]
	ss.mu.Unlock()
	out := &pb.Session{
		Name: j.Spec.ID, State: stateToPB[j.Status.State], Argv: j.Spec.Argv, Cwd: j.Spec.Cwd,
		Pid: int32(j.Status.PID), CreatedAt: ts(j.Status.CreatedAt), EndedAt: ts(j.Status.EndedAt),
		Attached: int32(n), LogSize: j.LogSize, CreatedBy: j.Spec.SubmittedBy, Owner: j.Spec.OwnerName(),
	}
	if j.Status.ExitCode != nil {
		out.HasExitCode, out.ExitCode = true, int32(*j.Status.ExitCode)
	}
	return out
}

// sessionSpace finds owner's space holding session name. Shared isolation
// has one namespace of names: with ownership enforced another user's
// session is reported as not found (create: "name in use").
func (ss *sessionsServer) sessionSpace(ctx context.Context, owner, name string, create bool) (*userspace.Space, error) {
	var sp *userspace.Space
	var err error
	if create {
		sp, err = ss.s.spaceFor(owner)
	} else if sp = ss.s.Spaces.Lookup(owner); sp == nil {
		return nil, status.Errorf(codes.NotFound, "no session %q", name)
	}
	if err != nil {
		return nil, err
	}
	if j, err := sp.Sessions.Get(name); err == nil && !ss.s.perUser() && ss.s.enforceFor(ctx) && j.Spec.OwnerName() != owner {
		if create { // even an ended one: re-creating would delete its transcript
			return nil, status.Errorf(codes.AlreadyExists, "session name %q is in use by another user (shared isolation has one namespace of names)", name)
		}
		return nil, status.Errorf(codes.NotFound, "no session %q", name)
	}
	return sp, nil
}

// ensure returns the running session, creating it if needed.
func (ss *sessionsServer) ensure(ctx context.Context, sp *userspace.Space, o *pb.AttachOpen, name, owner, by string) (supervisor.Job, bool, error) {
	mgr := sp.Sessions
	j, err := mgr.Get(name)
	if err == nil && !j.Status.State.Terminal() {
		return j, false, nil
	}
	if o.NoCreate {
		if err == nil {
			return j, false, status.Errorf(codes.FailedPrecondition, "session %q has ended (%s)", name, j.Status.State)
		}
		return j, false, status.Errorf(codes.NotFound, "no session %q", name)
	}
	if err == nil { // ended: a new session takes the name
		if err := mgr.Remove(name); err != nil && !errors.Is(err, supervisor.ErrNotFound) {
			return j, false, MgrErr(err)
		}
	}
	if ss.s.enforce() {
		if err := ss.s.checkSessionQuota(sp, owner); err != nil {
			return j, false, err
		}
	}
	argv := o.Argv
	if len(argv) == 0 {
		argv = DefaultShell()
		if sp.Account != nil && runtime.GOOS != "windows" {
			if env, err := sp.Env(); err == nil {
				for _, kv := range env {
					if sh, ok := strings.CutPrefix(kv, "SHELL="); ok && sh != "" {
						argv = []string{sh}
					}
				}
			}
		}
	}
	spec := jobs.Spec{
		ID: name, Name: name, Argv: argv, Cwd: o.Cwd, Env: o.Env, Pty: true, SubmittedBy: by, Owner: owner,
		Cols: uint16(o.GetSize().GetCols()), Rows: uint16(o.GetSize().GetRows()), LogMaxBytes: ss.s.LogMaxBytes,
	}
	j, err = ss.s.Spaces.SubmitSession(ctx, sp, spec, o.SecretEnv)
	if errors.Is(err, supervisor.ErrAlreadyExists) { // created concurrently by someone else
		if j, err = mgr.Get(name); err == nil && !j.Status.State.Terminal() {
			return j, false, nil
		}
	}
	if err != nil {
		return j, false, MgrErr(err)
	}
	return j, true, nil
}

// waitAttachable waits until the session's shim published its channel (or
// the session ended).
func (ss *sessionsServer) waitAttachable(ctx context.Context, sp *userspace.Space, name string) (supervisor.Job, error) {
	deadline := time.After(15 * time.Second)
	for {
		ch := sp.Sessions.Changed()
		j, err := sp.Sessions.Get(name)
		if err != nil {
			return j, MgrErr(err)
		}
		if j.Status.State.Terminal() || (j.Status.State == jobs.Running && j.Status.AttachAddr != "") {
			return j, nil
		}
		select {
		case <-ch:
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			return j, status.Errorf(codes.Unavailable, "session %q did not start", name)
		case <-ctx.Done():
			return j, status.FromContextError(ctx.Err()).Err()
		}
	}
}

func (ss *sessionsServer) Attach(st pb.Sessions_AttachServer) error {
	ctx := st.Context()
	id := IdentityFrom(ctx)
	first, err := st.Recv()
	if err != nil {
		return err
	}
	o := first.GetOpen()
	if o == nil {
		return status.Error(codes.InvalidArgument, "first message must be open")
	}
	name := o.Name
	if name == "" {
		name = DefaultSessionName
	}
	if !jobs.ValidID(name) {
		return status.Errorf(codes.InvalidArgument, "bad session name %q", name)
	}
	owner, err := ss.s.targetOwner(ctx, o.Owner)
	if err != nil {
		return err
	}
	var j supervisor.Job
	var created bool
	sp, err := ss.sessionSpace(ctx, owner, name, !o.NoCreate)
	if err == nil {
		j, created, err = ss.ensure(ctx, sp, o, name, owner, id.String())
	}
	ss.s.Audit.Log(id, pb.Sessions_Attach_FullMethodName, map[string]any{
		"session": name, "owner": owner, "created": created, "argv": j.Spec.Argv, "env_keys": keys(o.Env),
		"secret_keys": keys(o.SecretEnv), "error": errString(err),
	})
	if err != nil {
		return err
	}
	if j, err = ss.waitAttachable(ctx, sp, name); err != nil {
		return err
	}
	if j.Status.State.Terminal() { // e.g. the command failed to start
		if err := st.Send(&pb.AttachOutput{Msg: &pb.AttachOutput_Started{Started: &pb.AttachStarted{Session: ss.toPB(sp, j), Created: created}}}); err != nil {
			return err
		}
		return ss.sendExit(st, j)
	}

	var nc io.ReadWriteCloser
	for i := 0; ; i++ {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		nc, err = platform.DialLocal(dctx, j.Status.AttachAddr)
		cancel()
		if err == nil || i == 10 || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return status.Errorf(codes.Unavailable, "connect to session %q: %v", name, err)
	}
	conn := attach.NewConn(nc)
	defer conn.Close()
	flags := attach.FlagReplay
	if o.NoReplay {
		flags = 0
	}
	if err := conn.WriteFrame(attach.Hello, []byte{flags}); err != nil {
		return status.Errorf(codes.Unavailable, "session %q: %v", name, err)
	}
	if sz := o.GetSize(); !created && sz.GetCols() > 0 && sz.GetRows() > 0 {
		conn.WriteFrame(attach.Resize, attach.ResizePayload(uint16(sz.Cols), uint16(sz.Rows)))
	}
	ss.count(attachKey(sp, name), 1)
	defer ss.count(attachKey(sp, name), -1)
	if err := st.Send(&pb.AttachOutput{Msg: &pb.AttachOutput_Started{Started: &pb.AttachStarted{Session: ss.toPB(sp, j), Created: created}}}); err != nil {
		return err
	}

	// client -> shim
	detached := make(chan error, 1)
	go func() {
		for {
			in, err := st.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				detached <- err
				return
			}
			switch m := in.Msg.(type) {
			case *pb.AttachInput_Data:
				err = conn.WriteFrame(attach.Input, m.Data)
			case *pb.AttachInput_Resize:
				if m.Resize.GetCols() > 0 && m.Resize.GetRows() > 0 {
					err = conn.WriteFrame(attach.Resize, attach.ResizePayload(uint16(m.Resize.Cols), uint16(m.Resize.Rows)))
				}
			case *pb.AttachInput_Detach:
				detached <- nil
				return
			}
			if err != nil {
				detached <- nil // shim went away; the output side reports why
				return
			}
		}
	}()
	// shim -> client
	ended := make(chan error, 1)
	go func() {
		for {
			typ, p, err := conn.ReadFrame()
			if err != nil {
				ended <- err
				return
			}
			switch typ {
			case attach.Output:
				err = st.Send(&pb.AttachOutput{Msg: &pb.AttachOutput_Data{Data: p}})
			case attach.Exit:
				// Let the supervisor see the final state first, so a client
				// that lists sessions right after the exit sees it ended.
				ss.waitTerminal(ctx, sp, name, 2*time.Second)
				err = st.Send(&pb.AttachOutput{Msg: &pb.AttachOutput_ExitCode{ExitCode: int32(attach.ParseExit(p))}})
				ended <- errSessionExited
				return
			}
			if err != nil {
				ended <- err
				return
			}
		}
	}()
	select {
	case err := <-detached:
		conn.Close()
		<-ended
		return err
	case err := <-ended:
		conn.Close()
		if errors.Is(err, errSessionExited) {
			return nil
		}
		// The channel broke without an exit frame: the shim died or the
		// session ended right now. Report the final state if we can.
		if j, werr := ss.waitTerminal(ctx, sp, name, 3*time.Second); werr == nil {
			return ss.sendExit(st, j)
		}
		return status.Errorf(codes.Unavailable, "session %q: attach channel closed: %v", name, err)
	}
}

var errSessionExited = errors.New("session exited")

func (ss *sessionsServer) waitTerminal(ctx context.Context, sp *userspace.Space, name string, d time.Duration) (supervisor.Job, error) {
	deadline := time.After(d)
	for {
		ch := sp.Sessions.Changed()
		j, err := sp.Sessions.Get(name)
		if err != nil {
			return j, err
		}
		if j.Status.State.Terminal() {
			return j, nil
		}
		select {
		case <-ch:
		case <-time.After(200 * time.Millisecond):
		case <-deadline:
			return j, context.DeadlineExceeded
		case <-ctx.Done():
			return j, ctx.Err()
		}
	}
}

func (ss *sessionsServer) sendExit(st pb.Sessions_AttachServer, j supervisor.Job) error {
	code := int32(-1)
	if j.Status.ExitCode != nil {
		code = int32(*j.Status.ExitCode)
	}
	return st.Send(&pb.AttachOutput{Msg: &pb.AttachOutput_ExitCode{ExitCode: code}})
}

func (ss *sessionsServer) List(ctx context.Context, req *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error) {
	var states []jobs.State
	if !req.All {
		states = []jobs.State{jobs.Preparing, jobs.Running}
	}
	c := ss.s.callerOf(ctx)
	own := IdentityFrom(ctx).Owner()
	out := &pb.ListSessionsResponse{}
	for _, sp := range ss.s.Spaces.All() {
		for _, j := range sp.Sessions.List(states, 0) {
			owner := j.Spec.OwnerName()
			if req.Owner != "" && owner != req.Owner {
				continue
			}
			// Sessions are private (no team sharing); admins see all of
			// them, everybody else only their own.
			if ss.s.enforceFor(ctx) && owner != own && !c.Admin {
				continue
			}
			out.Sessions = append(out.Sessions, ss.toPB(sp, j))
		}
	}
	return out, nil
}

// lookup finds a session the caller may act on (admins: owner's).
func (ss *sessionsServer) lookup(ctx context.Context, owner, name string) (*userspace.Space, error) {
	o, err := ss.s.targetOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	if !ss.s.enforceFor(ctx) && owner == "" {
		return ss.s.Spaces.Shared(), nil
	}
	sp, err := ss.sessionSpace(ctx, o, name, false)
	if err != nil {
		return nil, err
	}
	if _, err := sp.Sessions.Get(name); err != nil {
		return nil, MgrErr(err)
	}
	return sp, nil
}

func (ss *sessionsServer) Kill(ctx context.Context, req *pb.KillSessionRequest) (*pb.Session, error) {
	sp, err := ss.lookup(ctx, req.Owner, req.Name)
	var j supervisor.Job
	if err == nil {
		j, err = sp.Sessions.Kill(ctx, req.Name)
		err = MgrErr(err)
	}
	ss.s.Audit.Log(IdentityFrom(ctx), pb.Sessions_Kill_FullMethodName, map[string]any{"session": req.Name, "owner": req.Owner, "error": errString(err)})
	if err != nil {
		return nil, err
	}
	return ss.toPB(sp, j), nil
}

func (ss *sessionsServer) Logs(req *pb.LogsRequest, st pb.Sessions_LogsServer) error {
	sp, err := ss.lookup(st.Context(), req.Owner, req.Id)
	if err != nil {
		return err
	}
	return StreamLogs(sp.Sessions, req, st)
}
