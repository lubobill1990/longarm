package userspace

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/platform"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// HelperCommand is longarmd's subcommand that serves a user's file and git
// operations as that user (server.ServeUserHelper).
const HelperCommand = "user-helper"

// Helper is a running `longarmd user-helper` as a space's account; its gRPC
// connection runs over the helper's stdin/stdout. Close it when done.
type Helper struct {
	Conn *grpc.ClientConn
	ctr  platform.Container
	cmd  *exec.Cmd
	pipe *stdioConn
	done chan struct{}
	once sync.Once
}

// StartHelper starts the space's helper. The helper (and anything it starts,
// like git) runs as the account in a container that dies with Close.
func (sp *Space) StartHelper(ctx context.Context) (*Helper, error) {
	if sp.Account == nil {
		return nil, errors.New("no account for this space")
	}
	env, err := sp.Env()
	if err != nil {
		return nil, err
	}
	ctr, err := platform.NewContainer(platform.ContainerOptions{Name: "longarm-helper", RunAs: sp.Account,
		Cgroup: sp.Cgroup(fmt.Sprintf("helper-%d", time.Now().UnixNano()))})
	if err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		ctr.Close()
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		ctr.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		ctr.Close()
		return nil, err
	}
	cmd := exec.Command(sp.sp.cfg.Exe, HelperCommand, "--root", sp.Root)
	cmd.Env, cmd.Dir = env, sp.Home()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	err = ctr.Start(cmd)
	inR.Close()
	outW.Close()
	errW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		errR.Close()
		ctr.Close()
		return nil, fmt.Errorf("start user helper as %s: %w", sp.Account.Name, err)
	}
	h := &Helper{ctr: ctr, cmd: cmd, pipe: &stdioConn{r: outR, w: inW}, done: make(chan struct{})}
	log := sp.sp.log.With("user", sp.Owner, "component", "user-helper")
	go func() { // the helper's diagnostics
		sc := bufio.NewScanner(errR)
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); l != "" {
				log.Warn(l)
			}
		}
		errR.Close()
	}()
	go func() { cmd.Wait(); close(h.done) }()
	used := false
	var mu sync.Mutex
	h.Conn, err = grpc.NewClient("passthrough:///user-helper",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			if used { // one stdio pipe, no reconnects
				return nil, errors.New("user helper connection is gone")
			}
			used = true
			return h.pipe, nil
		}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20), grpc.MaxCallSendMsgSize(64<<20)))
	if err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

// Close ends the helper: its stdin closes (it exits), then the container
// kills whatever is left.
func (h *Helper) Close() error {
	h.once.Do(func() {
		if h.Conn != nil {
			h.Conn.Close()
		}
		h.pipe.Close()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
		}
		h.ctr.Close()
	})
	return nil
}

// stdioConn is a net.Conn over the helper's stdout (read) and stdin (write).
type stdioConn struct {
	r    *os.File
	w    *os.File
	once sync.Once
}

func (c *stdioConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *stdioConn) Write(b []byte) (int, error) { return c.w.Write(b) }
func (c *stdioConn) Close() error {
	c.once.Do(func() { c.w.Close(); c.r.Close() })
	return nil
}
func (c *stdioConn) LocalAddr() net.Addr              { return stdioAddr{} }
func (c *stdioConn) RemoteAddr() net.Addr             { return stdioAddr{} }
func (c *stdioConn) SetDeadline(time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }

type stdioAddr struct{}

func (stdioAddr) Network() string { return "stdio" }
func (stdioAddr) String() string  { return "stdio" }

// NewStdioConn wraps a reader and writer (the helper's stdin and stdout)
// into a net.Conn.
func NewStdioConn(r io.Reader, w io.WriteCloser) net.Conn { return &rwConn{r: r, w: w} }

type rwConn struct {
	r    io.Reader
	w    io.WriteCloser
	once sync.Once
}

func (c *rwConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *rwConn) Write(b []byte) (int, error) { return c.w.Write(b) }
func (c *rwConn) Close() error {
	c.once.Do(func() { c.w.Close() })
	return nil
}
func (c *rwConn) LocalAddr() net.Addr              { return stdioAddr{} }
func (c *rwConn) RemoteAddr() net.Addr             { return stdioAddr{} }
func (c *rwConn) SetDeadline(time.Time) error      { return nil }
func (c *rwConn) SetReadDeadline(time.Time) error  { return nil }
func (c *rwConn) SetWriteDeadline(time.Time) error { return nil }

// prepare is the per-user Prepare hook: the helper fetches and checks out as
// the user.
func (sp *Space) prepare(ctx context.Context, _ jobs.Dir, spec jobs.Spec, secrets map[string]string, log io.Writer) (string, error) {
	h, err := sp.StartHelper(ctx)
	if err != nil {
		return "", err
	}
	defer h.Close()
	w := spec.Workspace
	req := &pb.PrepareJobRequest{JobId: spec.ID, SecretEnv: map[string]string{},
		Workspace: &pb.JobWorkspace{Repo: w.Repo, Url: w.URL, Ref: w.Ref, Commit: w.Commit, StagingId: w.StagingID}}
	if t, ok := secrets["GH_TOKEN"]; ok {
		req.SecretEnv["GH_TOKEN"] = t
	}
	st, err := pb.NewUserHelperClient(h.Conn).PrepareJob(ctx, req)
	if err != nil {
		return "", err
	}
	workdir := ""
	for {
		m, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if len(m.Log) > 0 {
			log.Write(m.Log)
		}
		if m.Workdir != "" {
			workdir = m.Workdir
		}
	}
	if workdir == "" {
		return "", errors.New("user helper returned no worktree")
	}
	return workdir, nil
}

// prune runs `git worktree prune` in the user's mirrors, as the user.
func (sp *Space) prune(ctx context.Context) {
	if sp.Account == nil {
		return
	}
	h, err := sp.StartHelper(ctx)
	if err != nil {
		sp.sp.log.Warn("prune worktrees", "user", sp.Owner, "err", err)
		return
	}
	defer h.Close()
	pb.NewUserHelperClient(h.Conn).Prune(ctx, &pb.PruneRequest{})
}
