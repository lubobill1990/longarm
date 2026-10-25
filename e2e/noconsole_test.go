package e2e

import (
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// stdioConn is a net.Conn over a child process's stdin/stdout (`longarm proxy`).
type stdioConn struct {
	io.Reader
	io.WriteCloser
	cmd *exec.Cmd
}

func (c *stdioConn) Close() error {
	c.WriteCloser.Close()
	c.cmd.Process.Kill()
	return nil
}
func (c *stdioConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *stdioConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *stdioConn) SetDeadline(time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }

// sshViaProxy connects to the worker's built-in sshd the way OpenSSH's
// ProxyCommand does (`longarm proxy <worker>` over mTLS), with the Go ssh client:
// unlike ssh.exe it works in every process context (service, no console).
func sshViaProxy(t *testing.T, longarmExe, cfgDir, worker string) *gossh.Client {
	t.Helper()
	cmd := exec.Command(longarmExe, "proxy", worker)
	cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+cfgDir)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr syncBuf
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	conn := &stdioConn{Reader: out, WriteCloser: in, cmd: cmd}
	c, chans, reqs, err := gossh.NewClientConn(conn, "longarm-"+worker, &gossh.ClientConfig{
		User: "longarm", HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: 30 * time.Second,
	})
	if err != nil {
		conn.Close()
		t.Fatalf("ssh over longarm proxy: %v (longarm proxy: %s)", err, stderr.String())
	}
	cl := gossh.NewClient(c, chans, reqs)
	t.Cleanup(func() { cl.Close() })
	return cl
}

// checkSSHExecPTY runs an exec session (output + exit code) and an
// interactive pty session against the worker's sshd through longarm proxy.
func checkSSHExecPTY(t *testing.T, longarmExe, cfgDir, helperExe string) {
	t.Helper()
	cl := sshViaProxy(t, longarmExe, cfgDir, "w1")
	s, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.CombinedOutput(helperExe + " print 3 0 9")
	if ee, ok := err.(*gossh.ExitError); !ok || ee.ExitStatus() != 9 || !strings.Contains(string(out), "line 3") {
		t.Fatalf("ssh exec: %q %v", out, err)
	}

	s, err = cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPty("xterm", 30, 100, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	var buf syncBuf
	s.Stdout, s.Stderr = &buf, &buf
	stdin, _ := s.StdinPipe()
	if err := s.Start(helperExe + " repl"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	waitOutput(t, buf.String, "repl ready", 30*time.Second)
	stdin.Write([]byte("size\r"))
	waitOutput(t, buf.String, "size 100x30", 15*time.Second)
	stdin.Write([]byte("exit 6\r"))
	select {
	case err := <-done:
		if ee, ok := err.(*gossh.ExitError); !ok || ee.ExitStatus() != 6 {
			t.Fatalf("ssh pty exit: %v\n%s", err, plain(buf.String()))
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("ssh pty session did not end:\n%s", plain(buf.String()))
	}
}

// TestLongarmdWithoutConsole runs longarmd the way a service does - no console (on
// Windows DETACHED_PROCESS; elsewhere a new session without a controlling
// terminal) - and checks every path that creates consoles or terminals: Exec,
// a ConPTY/pty session, and ssh exec + pty through longarm proxy.
func TestLongarmdWithoutConsole(t *testing.T) {
	e := newEnv(t)
	e.longarmdSysProcAttr = noConsoleAttr()
	e.startLongarmd()

	r := e.longarm(e.client, "exec", "w1", "--", bins.helper, "print", "3", "0", "9")
	if r.code != 9 || !strings.Contains(r.stdout, "line 3") {
		t.Fatalf("exec: %+v", r)
	}

	conn := e.dial(t)
	a := attachGo(t, conn, &pb.AttachOpen{Name: "nocon", Argv: []string{bins.helper, "repl"}, Size: &pb.TermSize{Cols: 90, Rows: 20}})
	a.waitFor("repl ready")
	a.send("size\r")
	a.waitFor("size 90x20")
	a.send("exit 4\r")
	a.waitEnd()
	if !a.exited || a.exit != 4 {
		t.Fatalf("session exit %v %d", a.exited, a.exit)
	}

	checkSSHExecPTY(t, bins.longarm, e.client, bins.helper)
}
