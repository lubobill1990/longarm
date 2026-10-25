package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/redact"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

const (
	defaultExecTimeout = 60 * time.Second
	maxExecTimeout     = time.Hour
	defaultExecOutput  = 1 << 20
	maxExecOutput      = 64 << 20
)

// limitedBuffer keeps the first max bytes and silently drops the rest (the
// child must never see a write error because we stopped capturing).
type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	room := l.max - l.buf.Len()
	if room < len(p) {
		l.truncated = true
		if room > 0 {
			l.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return l.buf.Write(p)
}

type execServer struct {
	pb.UnimplementedExecServer
	s *Server
}

// mergeEnv returns base with overrides applied (case-insensitive keys on
// Windows is handled by exec.Cmd's dedupEnv).
func mergeEnv(base []string, sets ...map[string]string) []string {
	env := append([]string(nil), base...)
	for _, m := range sets {
		for k, v := range m {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (e *execServer) Run(ctx context.Context, req *pb.ExecRequest) (*pb.ExecResponse, error) {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return nil, status.Error(codes.InvalidArgument, "empty argv")
	}
	timeout := defaultExecTimeout
	if req.Timeout != nil {
		timeout = req.Timeout.AsDuration()
	}
	if timeout <= 0 || timeout > maxExecTimeout {
		return nil, status.Errorf(codes.InvalidArgument, "timeout must be in (0, %s]", maxExecTimeout)
	}
	max := int(req.MaxOutputBytes)
	if max <= 0 {
		max = defaultExecOutput
	}
	if max > maxExecOutput {
		max = maxExecOutput
	}
	values := make([]string, 0, len(req.SecretEnv))
	for _, v := range req.SecretEnv {
		values = append(values, v)
	}
	red := redact.New(values)

	start := time.Now()
	resp := &pb.ExecResponse{}
	outBuf, errBuf := &limitedBuffer{max: max}, &limitedBuffer{max: max}
	outW := redact.NewWriter(outBuf, red)
	var errW *redact.Writer
	if req.Output == pb.OutputMode_OUTPUT_MODE_COMBINED {
		errW = outW
	} else {
		errW = redact.NewWriter(errBuf, red)
	}

	argv := append(append([]string(nil), e.s.ExecWrapper...), req.Argv...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = req.Cwd
	base := os.Environ()
	copts := platform.ContainerOptions{Name: "longarm-exec"}
	var code int
	var timedOut bool
	var err error
	if e.s.perUser() {
		// As the caller's OS account, with its environment and home.
		sp, serr := e.s.spaceFor(IdentityFrom(ctx).Owner())
		if serr != nil {
			return nil, serr
		}
		if base, err = sp.Env(); err != nil {
			return nil, status.Errorf(codes.Internal, "environment: %v", err)
		}
		copts.RunAs, copts.Cgroup = sp.Account, sp.Cgroup(fmt.Sprintf("exec-%d", time.Now().UnixNano()))
		if cmd.Dir == "" {
			cmd.Dir = sp.Home()
		}
	}
	if cmd.Dir == "" {
		cmd.Dir, _ = os.UserHomeDir()
	}
	cmd.Env = mergeEnv(base, req.Env, req.SecretEnv)
	var stdin *os.File
	if len(req.Stdin) > 0 {
		// a pipe (processes of other accounts get files only)
		r, w, perr := os.Pipe()
		if perr != nil {
			return nil, status.Error(codes.Internal, perr.Error())
		}
		cmd.Stdin, stdin = r, w
		go func() {
			w.Write(req.Stdin)
			w.Close()
		}()
		defer r.Close()
	}
	code, timedOut, err = runContained(ctx, copts, cmd, outW, errW, timeout)
	if stdin != nil {
		stdin.Close()
	}
	outW.Flush()
	errW.Flush()
	resp.ExitCode = int32(code)
	resp.TimedOut = timedOut
	if err != nil {
		resp.Error = err.Error()
	}
	if req.Output == pb.OutputMode_OUTPUT_MODE_COMBINED {
		resp.Combined = outBuf.buf.Bytes()
	} else {
		resp.Stdout, resp.Stderr = outBuf.buf.Bytes(), errBuf.buf.Bytes()
	}
	resp.Truncated = outBuf.truncated || errBuf.truncated
	resp.Duration = durationpb.New(time.Since(start))
	e.s.Audit.Log(IdentityFrom(ctx), pb.Exec_Run_FullMethodName, map[string]any{
		"argv": req.Argv, "cwd": req.Cwd, "env_keys": keys(req.Env), "secret_keys": keys(req.SecretEnv),
		"exit": code, "timed_out": timedOut,
	})
	return resp, nil
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// runContained runs cmd inside a process-tree container, copying its output
// through explicit pipes (so waiting for the main process does not also wait
// for grandchildren that inherited the pipe). On timeout or ctx cancellation
// the whole tree is killed; when the main process exits, leftovers are killed
// too. stdout and stderr may be the same writer (one shared pipe).
func runContained(ctx context.Context, opts platform.ContainerOptions, cmd *exec.Cmd, stdout, stderr io.Writer, timeout time.Duration) (code int, timedOut bool, err error) {
	ctr, err := platform.NewContainer(opts)
	if err != nil {
		return -1, false, err
	}
	defer ctr.Close()

	type pipe struct {
		r, w *os.File
		dst  io.Writer
	}
	var pipes []pipe
	newPipe := func(dst io.Writer) (*os.File, error) {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		pipes = append(pipes, pipe{r, w, dst})
		return w, nil
	}
	if cmd.Stdout, err = newPipe(stdout); err != nil {
		return -1, false, err
	}
	if stderr == stdout {
		cmd.Stderr = cmd.Stdout
	} else if cmd.Stderr, err = newPipe(stderr); err != nil {
		return -1, false, err
	}
	startErr := ctr.Start(cmd)
	for _, p := range pipes {
		p.w.Close()
	}
	if startErr != nil {
		for _, p := range pipes {
			p.r.Close()
		}
		return -1, false, startErr
	}
	var wg sync.WaitGroup
	for _, p := range pipes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			io.Copy(p.dst, p.r)
		}()
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-waited:
	case <-timer.C:
		timedOut = true
		ctr.Kill()
		<-waited
	case <-ctx.Done():
		ctr.Kill()
		<-waited
		err = ctx.Err()
	}
	ctr.Kill() // leftovers holding the pipes
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		for _, p := range pipes {
			p.r.Close()
		}
		<-drained
	}
	for _, p := range pipes {
		p.r.Close()
	}
	code = cmd.ProcessState.ExitCode()
	if timedOut {
		code = platform.KilledExitCode
		if err == nil {
			err = errors.New("timed out")
		}
	}
	return code, timedOut, err
}
