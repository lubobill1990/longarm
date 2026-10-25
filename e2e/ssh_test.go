package e2e

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/lubobill1990/longarm/internal/platform"
)

// opensshTool finds an OpenSSH client tool: on Windows the system's
// OpenSSH for Windows (C:\Windows\System32\OpenSSH), elsewhere from PATH.
func opensshTool(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		p := filepath.Join(os.Getenv("SystemRoot"), "System32", "OpenSSH", name+".exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed", name)
	}
	return p
}

type cmdResult struct {
	out  string
	code int
}

// toolOutput sends cmd's stdout and stderr to a file and returns a reader of
// what was written so far. OpenSSH for Windows' ssh.exe hangs when its
// stdout is a pipe (or its stdin is NUL) in a console-less logon session -
// e.g. tests started with `ssh host 'go test ...'` - even against
// Microsoft's own sshd. So the tests never give it an output pipe, and
// always give it a stdin pipe (see runTool).
func toolOutput(t *testing.T, cmd *exec.Cmd) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "tool-*.out")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	cmd.Stdout, cmd.Stderr = f, f
	return func() string {
		b, _ := os.ReadFile(f.Name())
		return string(b)
	}
}

func runTool(t *testing.T, stdin string, name string, args ...string) cmdResult {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Stdin = toolInput(t, stdin)
	out := toolOutput(t, cmd)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatalf("%s %v timed out:\n%s", filepath.Base(name), args, out())
	}
	return cmdResult{out(), cmd.ProcessState.ExitCode()}
}

// socks5Connect dials target through a SOCKS5 proxy (ssh -D), as VS Code
// Remote-SSH does.
func socks5Connect(proxy, host string, port int) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", proxy, 10*time.Second)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(15 * time.Second))
	c.Write([]byte{5, 1, 0})
	r := make([]byte, 2)
	if _, err := io.ReadFull(c, r); err != nil || r[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks greeting: %v %v", r, err)
	}
	req := []byte{5, 1, 0, 3, byte(len(host))}
	req = append(req, host...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	c.Write(req)
	resp := make([]byte, 10)
	if _, err := io.ReadFull(c, resp); err != nil || resp[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks connect: %v %v", resp, err)
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

// TestSSH drives the built-in sshd with the OpenSSH client tools: through
// `longarm proxy` (longarm ssh-config) - exec, exit codes, stdin, pty, scp, sftp,
// -L / -D forwarding - and through a direct listener with authorized_keys.
func TestSSH(t *testing.T) {
	ssh := opensshTool(t, "ssh")
	scp := opensshTool(t, "scp")
	sftp := opensshTool(t, "sftp")
	keygen := opensshTool(t, "ssh-keygen")
	e := newEnv(t)
	sshPort := freePort(t)
	os.MkdirAll(e.dataDir, 0o700)
	os.WriteFile(filepath.Join(e.dataDir, "longarmd.toml"), []byte(fmt.Sprintf("[ssh]\nlisten = [\"tcp://127.0.0.1:%d\"]\n", sshPort)), 0o600)
	e.startLongarmd()

	cfgFile := filepath.Join(e.root, "ssh_config")
	os.WriteFile(cfgFile, []byte(e.mustLongarm(e.client, "ssh-config")), 0o600)
	kh, _ := os.ReadFile(filepath.Join(e.client, "known_hosts"))
	if !strings.HasPrefix(string(kh), "longarm-w1 ssh-ed25519 ") {
		t.Fatalf("known_hosts: %q", kh)
	}
	sshArgs := func(args ...string) []string { return append([]string{"-F", cfgFile, "-o", "BatchMode=yes"}, args...) }

	t.Run("exec", func(t *testing.T) {
		cmdline := bins.helper + " print 3 0 9"
		r := runTool(t, "", ssh, sshArgs("longarm-w1", cmdline)...)
		if r.code != 9 || !strings.Contains(r.out, "line 3") {
			t.Fatalf("exec: %+v", r)
		}
		// stdin reaches the command; EOF ends it.
		r = runTool(t, "via stdin\n", ssh, sshArgs("longarm-w1", bins.helper+" repl")...)
		if r.code != 0 || !strings.Contains(r.out, "echo: via stdin") {
			t.Fatalf("exec stdin: %+v", r)
		}
		// Shell without a pty, script on stdin (VS Code Remote-SSH style).
		script := "echo shell-stdin-ok\nexit\n"
		r = runTool(t, script, ssh, sshArgs("-T", "longarm-w1")...)
		if !strings.Contains(r.out, "shell-stdin-ok") {
			t.Fatalf("shell -T: %+v", r)
		}
		audit, _ := os.ReadFile(filepath.Join(e.dataDir, "audit.log"))
		if !strings.Contains(string(audit), `"method":"ssh exec"`) || !strings.Contains(string(audit), `"who":"client/alice"}`) {
			t.Fatalf("no ssh audit record for the mTLS identity:\n%s", audit)
		}
	})

	t.Run("pty", func(t *testing.T) {
		// -tt with redirected input: input arrives as typed keys (Enter =
		// CR), buffered by the terminal until the program reads it. (A file,
		// not a pipe: see toolInput.)
		cmd := exec.Command(ssh, sshArgs("-tt", "longarm-w1", bins.helper+" repl")...)
		cmd.Stdin = toolInput(t, "size\rexit 6\r")
		out := toolOutput(t, cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer cmd.Process.Kill()
		waitOutput(t, out, "repl ready", 30*time.Second)
		waitOutput(t, out, "size ", 15*time.Second)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("ssh -tt did not exit:\n%s", plain(out()))
		}
		if code := cmd.ProcessState.ExitCode(); code != 6 {
			t.Fatalf("pty exit %d:\n%s", code, plain(out()))
		}
	})

	t.Run("scp_sftp", func(t *testing.T) {
		data := randBytes(t, 700_000)
		src := filepath.Join(e.root, "scp-src.bin")
		os.WriteFile(src, data, 0o644)
		remote := filepath.ToSlash(filepath.Join(e.root, "scp-remote.bin"))
		if r := runTool(t, "", scp, append(sshArgs(), src, "longarm-w1:"+remote)...); r.code != 0 {
			t.Fatalf("scp up: %+v", r)
		}
		if got, _ := os.ReadFile(filepath.FromSlash(remote)); !bytes.Equal(got, data) {
			t.Fatal("scp upload differs")
		}
		back := filepath.Join(e.root, "scp-back.bin")
		if r := runTool(t, "", scp, append(sshArgs(), "longarm-w1:"+remote, back)...); r.code != 0 {
			t.Fatalf("scp down: %+v", r)
		}
		if got, _ := os.ReadFile(back); !bytes.Equal(got, data) {
			t.Fatal("scp download differs")
		}
		os.MkdirAll(filepath.Join(e.root, "tree", "sub"), 0o755)
		os.WriteFile(filepath.Join(e.root, "tree", "sub", "f.txt"), []byte("in tree"), 0o644)
		rdir := filepath.ToSlash(filepath.Join(e.root, "tree-remote"))
		if r := runTool(t, "", scp, append(sshArgs("-r"), filepath.Join(e.root, "tree"), "longarm-w1:"+rdir)...); r.code != 0 {
			t.Fatalf("scp -r: %+v", r)
		}
		if got, _ := os.ReadFile(filepath.Join(e.root, "tree-remote", "sub", "f.txt")); string(got) != "in tree" {
			t.Fatalf("scp -r result %q", got)
		}
		batch := fmt.Sprintf("pwd\nls %s\nget %s %s\n", rdir+"/sub", remote, filepath.ToSlash(filepath.Join(e.root, "sftp-back.bin")))
		r := runTool(t, batch, sftp, append(sshArgs("-b", "-"), "longarm-w1")...)
		if r.code != 0 || !strings.Contains(r.out, "f.txt") {
			t.Fatalf("sftp: %+v", r)
		}
		if got, _ := os.ReadFile(filepath.Join(e.root, "sftp-back.bin")); !bytes.Equal(got, data) {
			t.Fatal("sftp get differs")
		}
	})

	t.Run("port_forwarding", func(t *testing.T) {
		port := startEchoServer(t, e.root)
		lport, dport := freePort(t), freePort(t)
		cmd := exec.Command(ssh, sshArgs("-N", "-L", fmt.Sprintf("%d:127.0.0.1:%d", lport, port), "-D", fmt.Sprint(dport), "longarm-w1")...)
		cmd.Stdin = strings.NewReader("")
		out := toolOutput(t, cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { cmd.Process.Kill(); cmd.Wait() }()
		roundTrip := func(dial func() (net.Conn, error)) {
			t.Helper()
			var c net.Conn
			var err error
			waitFor(t, "forward to come up", 30*time.Second, func() bool {
				c, err = dial()
				return err == nil
			})
			defer c.Close()
			c.Write([]byte("tunnel"))
			c.(*net.TCPConn).CloseWrite()
			c.SetReadDeadline(time.Now().Add(15 * time.Second))
			got, _ := io.ReadAll(c)
			if string(got) != "tunnel6 bytes\n" {
				t.Fatalf("through tunnel: %q (ssh: %s)", got, out())
			}
		}
		roundTrip(func() (net.Conn, error) { return net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", lport)) })
		roundTrip(func() (net.Conn, error) { return socks5Connect(fmt.Sprintf("127.0.0.1:%d", dport), "127.0.0.1", port) })
	})

	t.Run("direct_listener_authorized_keys", func(t *testing.T) {
		key := filepath.Join(e.root, "id_test")
		if r := runTool(t, "", keygen, "-q", "-t", "ed25519", "-N", "", "-C", "e2e-key", "-f", key); r.code != 0 {
			t.Fatalf("ssh-keygen: %+v", r)
		}
		other := filepath.Join(e.root, "id_other")
		runTool(t, "", keygen, "-q", "-t", "ed25519", "-N", "", "-f", other)
		pub, _ := os.ReadFile(key + ".pub")
		os.WriteFile(filepath.Join(e.dataDir, "authorized_keys"), pub, 0o600)
		kh := filepath.Join(e.root, "kh_direct")
		direct := func(k string) cmdResult {
			return runTool(t, "", ssh, "-F", "none", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile="+kh, "-o", "IdentitiesOnly=yes", "-i", k, "-p", fmt.Sprint(sshPort), "127.0.0.1",
				bins.helper+" print 1 0 0")
		}
		if r := direct(key); r.code != 0 || !strings.Contains(r.out, "line 1") {
			t.Fatalf("authorized key: %+v", r)
		}
		if r := direct(other); r.code == 0 {
			t.Fatalf("unauthorized key accepted: %+v", r)
		}
		audit, _ := os.ReadFile(filepath.Join(e.dataDir, "audit.log"))
		if !strings.Contains(string(audit), "ssh-key/e2e-key@") {
			t.Fatal("no audit record for the key identity")
		}
	})
}

// TestSSHChannelCloseKillsTree: closing one session channel on a connection
// that stays open (ControlMaster / VS Code style) ends that session's
// process tree.
func TestSSHChannelCloseKillsTree(t *testing.T) {
	keygen := opensshTool(t, "ssh-keygen")
	e := newEnv(t)
	sshPort := freePort(t)
	os.MkdirAll(e.dataDir, 0o700)
	os.WriteFile(filepath.Join(e.dataDir, "longarmd.toml"), []byte(fmt.Sprintf("[ssh]\nlisten = [\"tcp://127.0.0.1:%d\"]\n", sshPort)), 0o600)
	key := filepath.Join(e.root, "id")
	if r := runTool(t, "", keygen, "-q", "-t", "ed25519", "-N", "", "-f", key); r.code != 0 {
		t.Fatal(r.out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	os.WriteFile(filepath.Join(e.dataDir, "authorized_keys"), pub, 0o600)
	e.startLongarmd()

	pem, _ := os.ReadFile(key)
	signer, err := gossh.ParsePrivateKey(pem)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := gossh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", sshPort), &gossh.ClientConfig{
		User: "longarm", Auth: []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	for _, pty := range []bool{false, true} {
		pidfile := filepath.Join(e.root, fmt.Sprintf("tree-%v.pids", pty))
		s, err := cl.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if pty {
			if err := s.RequestPty("xterm", 25, 80, gossh.TerminalModes{}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Start(bins.helper + " tree " + pidfile); err != nil {
			t.Fatal(err)
		}
		var pids []int
		waitFor(t, "5 tree processes", 30*time.Second, func() bool {
			b, _ := os.ReadFile(pidfile)
			pids = pids[:0]
			for _, f := range strings.Fields(string(b)) {
				var p int
				fmt.Sscan(f, &p)
				pids = append(pids, p)
			}
			return len(pids) == 5
		})
		s.Close() // the channel only; the connection stays up
		waitFor(t, fmt.Sprintf("tree to die after channel close (pty=%v)", pty), 15*time.Second, func() bool {
			for _, p := range pids {
				if platform.ProcessAlive(p, 0) {
					return false
				}
			}
			return true
		})
	}
	// The connection is still usable.
	s, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := s.Output(bins.helper + " print 1 0 0"); err != nil || !strings.Contains(string(out), "line 1") {
		t.Fatalf("%q %v", out, err)
	}
}

// toolInput returns stdin for an OpenSSH tool: an empty pipe, or a file
// holding data. ssh.exe never reads data from a pipe (nor exits on NUL) in
// a console-less logon session, see toolOutput.
func toolInput(t *testing.T, data string) io.Reader {
	t.Helper()
	if data == "" {
		return strings.NewReader("")
	}
	p := filepath.Join(t.TempDir(), "tool.in")
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
