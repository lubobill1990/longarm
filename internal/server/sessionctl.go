package server

// Sessions for programs: Start (detached, idempotent by name), Get, Send
// (input without attaching), Stop (graceful, then kill) and the terminal
// activity of List / Get. Send and Stop write through a control connection
// to the session's shim (attach.FlagControl), which serializes every write
// to the terminal.

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/attach"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/supervisor"
	"github.com/lubobill1990/longarm/internal/userspace"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// legacyWait is how long longarmd waits for a shim's Info answer on a
// control connection before it takes the shim for one that predates them.
const legacyWait = 1500 * time.Millisecond

// maxStopGrace bounds StopSessionRequest.grace.
const maxStopGrace = 10 * time.Minute

// legacyShims remembers shims (by pid and start time) without control
// connections, so only the first call waits for them.
var legacyShims sync.Map

func shimKey(st jobs.Status) [2]uint64 { return [2]uint64{uint64(st.ShimPID), st.ShimStart} }

func (ss *sessionsServer) Start(ctx context.Context, req *pb.StartSessionRequest) (*pb.AttachStarted, error) {
	id := IdentityFrom(ctx)
	name := req.Name
	if !jobs.ValidID(name) {
		return nil, status.Errorf(codes.InvalidArgument, "bad session name %q", name)
	}
	owner := id.Owner()
	r := sessionReq{argv: req.Argv, cwd: req.Cwd, env: req.Env, secrets: req.SecretEnv, size: req.Size, strict: true}
	if w := req.Workspace; w != nil {
		if !workspace.ValidRepo(w.Repo) {
			return nil, status.Errorf(codes.InvalidArgument, "bad workspace repo %q", w.Repo)
		}
		if w.StagingId != "" && !jobs.ValidID(w.StagingId) {
			return nil, status.Errorf(codes.InvalidArgument, "bad staging id %q", w.StagingId)
		}
		r.ws = &jobs.Workspace{Repo: w.Repo, URL: w.Url, Ref: w.Ref, Commit: w.Commit, StagingID: w.StagingId}
	}
	var j supervisor.Job
	var created bool
	sp, err := ss.sessionSpace(ctx, owner, name, true)
	if err == nil {
		j, created, err = ss.ensure(ctx, sp, r, name, owner, id.String())
	}
	ss.s.Audit.Log(id, pb.Sessions_Start_FullMethodName, map[string]any{
		"session": name, "owner": owner, "created": created, "argv": j.Spec.Argv, "cwd": req.Cwd, "env_keys": keys(req.Env),
		"secret_keys": keys(req.SecretEnv), "workspace": r.ws != nil, "error": errString(err),
	})
	if err != nil {
		return nil, err
	}
	if !created && r.ws != nil && r.ws.StagingID != "" && (j.Spec.Workspace == nil || j.Spec.Workspace.StagingID != r.ws.StagingID) {
		sp.WS.RemoveStaging(r.ws.StagingID) // uploaded for nothing: the session exists (a retry of ours keeps it)
	}
	// Wait until it runs (or ended): a workspace may take long to prepare;
	// answer before the caller's deadline with what there is.
	wait := 10 * time.Minute
	if dl, ok := ctx.Deadline(); ok {
		wait = time.Until(dl) - 2*time.Second
	}
	if wait > 0 {
		if j2, err := ss.waitAttachable(ctx, sp, name, wait); err == nil {
			j = j2
		}
	}
	if latest, err := sp.Sessions.Get(name); err == nil {
		j = latest
	}
	return &pb.AttachStarted{Session: ss.toPB(sp, j), Created: created}, nil
}

func (ss *sessionsServer) Get(ctx context.Context, req *pb.GetSessionRequest) (*pb.Session, error) {
	sp, err := ss.lookup(ctx, req.Owner, req.Name)
	if err != nil {
		return nil, err
	}
	j, err := sp.Sessions.Get(req.Name)
	if err != nil {
		return nil, MgrErr(err)
	}
	out := ss.toPB(sp, j)
	ss.addActivity(ctx, []listed{{sp, j}}, []*pb.Session{out})
	return out, nil
}

func (ss *sessionsServer) Send(ctx context.Context, req *pb.SendSessionRequest) (*pb.SendSessionResponse, error) {
	if len(req.Data) > attach.MaxSend {
		return nil, status.Errorf(codes.InvalidArgument, "at most %d bytes per send", attach.MaxSend)
	}
	sp, err := ss.lookup(ctx, req.Owner, req.Name)
	var seq uint64
	if err == nil {
		seq, err = ss.send(ctx, sp, req.Name, req.Data, req.IfIdleMs, attach.SourceSend)
	}
	// the input is like a payload: its size only, never its bytes
	ss.s.Audit.Log(IdentityFrom(ctx), pb.Sessions_Send_FullMethodName, map[string]any{
		"session": req.Name, "owner": req.Owner, "bytes": len(req.Data), "if_idle_ms": req.IfIdleMs, "error": errString(err),
	})
	if err != nil {
		return nil, err
	}
	return &pb.SendSessionResponse{InputSeq: seq}, nil
}

// send writes data to the session's terminal in one write.
func (ss *sessionsServer) send(ctx context.Context, sp *userspace.Space, name string, data []byte, ifIdleMs uint32, source string) (uint64, error) {
	j, err := sp.Sessions.Get(name)
	if err != nil {
		return 0, MgrErr(err)
	}
	if !j.Status.State.Terminal() && j.Status.AttachAddr == "" { // preparing / starting
		if j, err = ss.waitAttachable(ctx, sp, name, 15*time.Second); err != nil {
			return 0, err
		}
	}
	if j.Status.State.Terminal() {
		return 0, status.Errorf(codes.FailedPrecondition, "session %q has ended (%s)", name, j.Status.State)
	}
	c, info, err := ss.control(ctx, j)
	if err != nil {
		if ended, gerr := sp.Sessions.Get(name); gerr == nil && ended.Status.State.Terminal() {
			return 0, status.Errorf(codes.FailedPrecondition, "session %q has ended (%s)", name, ended.Status.State)
		}
		return 0, status.Errorf(codes.Unavailable, "session %q: %v", name, err)
	}
	defer c.Close()
	if info == nil { // a shim from before control connections
		if ifIdleMs > 0 {
			return 0, status.Errorf(codes.FailedPrecondition,
				"session %q runs in a shim from an older longarmd without if_idle_ms; start the session again to use it", name)
		}
		if err := c.WriteFrame(attach.Input, data); err != nil {
			return 0, status.Errorf(codes.Unavailable, "session %q: %v", name, err)
		}
		return 0, nil
	}
	if err := c.WriteFrame(attach.Send, attach.SendPayload(ifIdleMs, source, data)); err != nil {
		return 0, status.Errorf(codes.Unavailable, "session %q: %v", name, err)
	}
	st, err := c.info(30 * time.Second)
	if err != nil {
		return 0, status.Errorf(codes.Unavailable, "session %q: no answer from its shim: %v", name, err)
	}
	if !st.Sent {
		ago := time.Since(st.LastInput).Round(time.Millisecond)
		return st.InputSeq, status.Errorf(codes.ResourceExhausted,
			"session %q: input arrived %s ago (if_idle_ms %d); nothing sent, retry later", name, ago, ifIdleMs)
	}
	return st.InputSeq, nil
}

func (ss *sessionsServer) Stop(ctx context.Context, req *pb.StopSessionRequest) (*pb.Session, error) {
	sp, err := ss.lookup(ctx, req.Owner, req.Name)
	var j supervisor.Job
	if err == nil {
		j, err = ss.stop(ctx, sp, req.Name, min(req.GetGrace().AsDuration(), maxStopGrace))
	}
	ss.s.Audit.Log(IdentityFrom(ctx), pb.Sessions_Stop_FullMethodName, map[string]any{
		"session": req.Name, "owner": req.Owner, "grace": req.GetGrace().AsDuration().String(), "state": string(j.Status.State), "error": errString(err),
	})
	if err != nil {
		return nil, err
	}
	return ss.toPB(sp, j), nil
}

// stop sends Ctrl-C (twice: TUIs such as Claude Code exit on the second),
// waits up to grace for the session to end, then kills its process tree.
// The caller going away does not cut it short (grace is bounded).
func (ss *sessionsServer) stop(ctx context.Context, sp *userspace.Space, name string, grace time.Duration) (supervisor.Job, error) {
	ctx = context.WithoutCancel(ctx)
	j, err := sp.Sessions.Get(name)
	if err != nil {
		return j, MgrErr(err)
	}
	if j.Status.State.Terminal() {
		return j, nil
	}
	if grace > 0 {
		deadline := time.Now().Add(grace)
		for i := 0; i < 2; i++ {
			if _, err := ss.send(ctx, sp, name, []byte{0x03}, 0, attach.SourceStop); err != nil {
				break
			}
			wait := min(time.Second, time.Until(deadline))
			if i == 1 {
				wait = time.Until(deadline)
			}
			if j, err := ss.waitTerminal(ctx, sp, name, max(wait, 0)); err == nil {
				return j, nil
			}
		}
		if j, err := ss.waitTerminal(ctx, sp, name, max(time.Until(deadline), 0)); err == nil {
			return j, nil
		}
	}
	j, err = sp.Sessions.Kill(ctx, name)
	return j, MgrErr(err)
}

// ctlConn is a control connection to a session's shim.
type ctlConn struct {
	*attach.Conn
	nc net.Conn
}

// info reads the shim's next Info frame (skipping output a shim may send).
func (c *ctlConn) info(d time.Duration) (attach.InfoState, error) {
	c.nc.SetReadDeadline(time.Now().Add(d))
	defer c.nc.SetReadDeadline(time.Time{})
	for {
		typ, p, err := c.ReadFrame()
		if err != nil {
			return attach.InfoState{}, err
		}
		switch typ {
		case attach.Info:
			return attach.DecodeInfo(p)
		case attach.Exit:
			return attach.InfoState{}, errors.New("the session ended")
		}
	}
}

// control opens a control connection to j's shim. info is the shim's first
// Info frame, nil for a shim that predates control connections (the
// connection then works for plain Input frames).
func (ss *sessionsServer) control(ctx context.Context, j supervisor.Job) (*ctlConn, *attach.InfoState, error) {
	var nc net.Conn
	var err error
	for i := 0; ; i++ {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		nc, err = platform.DialLocal(dctx, j.Status.AttachAddr)
		cancel()
		if err == nil || i == 3 || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return nil, nil, err
	}
	c := &ctlConn{Conn: attach.NewConn(nc), nc: nc}
	key := shimKey(j.Status)
	if _, legacy := legacyShims.Load(key); legacy {
		// replay off; as an ordinary client it also gets output, which
		// nobody reads: the connection is closed right after the write
		return c, nil, c.WriteFrame(attach.Hello, []byte{0})
	}
	if err := c.WriteFrame(attach.Hello, []byte{attach.FlagControl}); err != nil {
		c.Close()
		return nil, nil, err
	}
	st, err := c.info(legacyWait)
	var ne net.Error
	if err != nil && !(errors.As(err, &ne) && ne.Timeout()) {
		c.Close()
		return nil, nil, err
	}
	if err != nil {
		legacyShims.Store(key, true)
		return c, nil, nil
	}
	return c, &st, nil
}

// listed is a session found by List / Get.
type listed struct {
	sp *userspace.Space
	j  supervisor.Job
}

// addActivity fills the activity fields of out[i] (the session of
// found[i]): from the shim of a running session, else (ended sessions,
// shims that predate it) the transcript's modification time as the last
// output.
func (ss *sessionsServer) addActivity(ctx context.Context, found []listed, out []*pb.Session) {
	var wg sync.WaitGroup
	for i, f := range found {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var st *attach.InfoState
			if f.j.Status.State == jobs.Running && f.j.Status.AttachAddr != "" {
				cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				if c, info, err := ss.control(cctx, f.j); err == nil {
					c.Close()
					st = info
				}
				cancel()
			}
			o := out[i]
			if st != nil {
				o.InputSeq, o.LastInputSource = st.InputSeq, st.LastInputSource
				o.LastInput, o.LastOutput = ts(st.LastInput), ts(st.LastOutput)
			} else if d, err := f.sp.Sessions.Dir(f.j.Spec.ID); err == nil {
				if fi, err := os.Stat(d.Path(jobs.LogFile)); err == nil && fi.Size() > 0 {
					o.LastOutput = ts(fi.ModTime().UTC())
				}
			}
			last := o.LastOutput
			if o.LastInput != nil && (last == nil || o.LastInput.AsTime().After(last.AsTime())) {
				last = o.LastInput
			}
			o.LastActivity = last
		}()
	}
	wg.Wait()
}
