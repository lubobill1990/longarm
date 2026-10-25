package e2e

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/client"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// dial returns a gRPC connection to w1 with the client certificate.
func (e *env) dial(t *testing.T) *client.Conn {
	t.Helper()
	cfg, err := client.LoadConfig(e.client)
	if err != nil {
		t.Fatal(err)
	}
	d, err := client.NewDialer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	w, err := cfg.Worker("w1")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := d.Dial(w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); d.Close() })
	return conn
}

// syncBuf is a goroutine-safe buffer.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[=>]|\r`)

// plain strips terminal escape sequences and CRs.
func plain(s string) string { return ansiRE.ReplaceAllString(s, "") }

func waitOutput(t *testing.T, what func() string, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !strings.Contains(plain(what()), want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in:\n%s", want, plain(what()))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// goAttach is an attached session driven through the gRPC API.
type goAttach struct {
	t       *testing.T
	st      pb.Sessions_AttachClient
	cancel  context.CancelFunc
	out     syncBuf
	started *pb.AttachStarted
	done    chan struct{}
	exit    int32
	exited  bool
	err     error
}

func attachGo(t *testing.T, conn *client.Conn, open *pb.AttachOpen) *goAttach {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := conn.Sessions().Attach(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Open{Open: open}}); err != nil {
		cancel()
		t.Fatal(err)
	}
	m, err := st.Recv()
	if err != nil || m.GetStarted() == nil {
		cancel()
		t.Fatalf("attach: %v %v", m, err)
	}
	a := &goAttach{t: t, st: st, cancel: cancel, started: m.GetStarted(), done: make(chan struct{})}
	go func() {
		defer close(a.done)
		for {
			m, err := st.Recv()
			if err != nil {
				if err != io.EOF {
					a.err = err
				}
				return
			}
			switch x := m.Msg.(type) {
			case *pb.AttachOutput_Data:
				a.out.Write(x.Data)
			case *pb.AttachOutput_ExitCode:
				a.exit, a.exited = x.ExitCode, true
			}
		}
	}()
	t.Cleanup(cancel)
	return a
}

func (a *goAttach) send(s string) {
	a.t.Helper()
	if err := a.st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Data{Data: []byte(s)}}); err != nil {
		a.t.Fatal(err)
	}
}

func (a *goAttach) resize(cols, rows uint32) {
	a.t.Helper()
	if err := a.st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Resize{Resize: &pb.TermSize{Cols: cols, Rows: rows}}}); err != nil {
		a.t.Fatal(err)
	}
}

func (a *goAttach) waitFor(s string) { a.t.Helper(); waitOutput(a.t, a.out.String, s, 30*time.Second) }

func (a *goAttach) detach() {
	a.st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Detach{Detach: true}})
	a.st.CloseSend()
	select {
	case <-a.done:
	case <-time.After(10 * time.Second):
		a.t.Error("stream did not end after detach")
	}
	a.cancel()
}

func (a *goAttach) waitEnd() {
	a.t.Helper()
	select {
	case <-a.done:
	case <-time.After(30 * time.Second):
		a.t.Fatal("session stream did not end")
	}
}

// bg is a background longarm process.
type bg struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout syncBuf
	stderr syncBuf
	done   chan struct{}
	err    error
}

func (e *env) longarmBG(t *testing.T, cfgDir string, args ...string) *bg {
	t.Helper()
	b := &bg{cmd: exec.Command(bins.longarm, args...), done: make(chan struct{})}
	b.cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+cfgDir)
	b.cmd.Stdout, b.cmd.Stderr = &b.stdout, &b.stderr
	var err error
	if b.stdin, err = b.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := b.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { b.err = b.cmd.Wait(); close(b.done) }()
	t.Cleanup(func() {
		select {
		case <-b.done:
		default:
			b.cmd.Process.Kill()
			<-b.done
		}
	})
	return b
}

func (b *bg) wait(t *testing.T, d time.Duration) int {
	t.Helper()
	select {
	case <-b.done:
	case <-time.After(d):
		t.Fatalf("longarm did not exit; stdout:\n%s\nstderr:\n%s", plain(b.stdout.String()), b.stderr.String())
	}
	if b.cmd.ProcessState == nil {
		t.Fatalf("longarm: %v", b.err)
	}
	return b.cmd.ProcessState.ExitCode()
}
