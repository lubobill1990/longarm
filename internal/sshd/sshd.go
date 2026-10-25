// Package sshd is longarmd's built-in SSH server (DESIGN §3.1 "sshd", the
// compatibility layer for OpenSSH ssh/scp/sftp, VS Code Remote-SSH and old
// scripts).
//
// It does not listen on the network by default. Connections arrive through
// `longarm proxy <worker>` (an OpenSSH ProxyCommand): longarm opens Forward.Ssh over
// the mTLS-authenticated gRPC channel and longarmd hands the byte stream to
// HandleProxyConn. The caller is already authenticated by its client
// certificate, so these connections need no ssh-level auth. Optional direct
// listeners ([ssh] listen in longarmd.toml) authenticate with public keys from
// <data>/authorized_keys.
//
// Supported: exec and shell sessions with or without a pty (ConPTY / pty),
// window changes, env requests, the sftp subsystem (pkg/sftp; scp in its
// default sftp mode), and local port forwarding (direct-tcpip), which VS
// Code Remote-SSH needs. Every session's process tree lives in a container
// (Job Object / process group) and dies with the session.
package sshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"

	"github.com/lubobill1990/longarm/internal/platform"
)

// Config configures the server.
type Config struct {
	HostKeyFile        string // created (ed25519) if missing
	AuthorizedKeysFile string // for direct listeners
	// Shell runs exec requests and pty-less shells: the raw command line is
	// appended (default: cmd.exe /c on Windows, like OpenSSH for Windows;
	// $SHELL -c elsewhere).
	Shell []string
	// InteractiveShell is the command of a shell request with a pty
	// (default: pwsh on Windows, $SHELL elsewhere).
	InteractiveShell func() []string
	// ConsoleHost is longarmd's `conhost` subcommand ([longarmd.exe, "conhost"]) used
	// on Windows to give commands UTF-8 consoles; nil = run directly.
	ConsoleHost []string
	// Audit records sessions, sftp and forwards (who, what, details).
	Audit func(who, what string, fields map[string]any)
	Log   *slog.Logger
}

// Server is the built-in sshd.
type Server struct {
	cfg    Config
	signer gossh.Signer
	proxy  *ssh.Server // no ssh auth: the caller came through mTLS
	direct *ssh.Server // public-key auth

	mu        sync.Mutex
	listeners []net.Listener
}

const whoKey = "longarm-who"

// New loads or creates the host key and builds the server.
func New(cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Audit == nil {
		cfg.Audit = func(string, string, map[string]any) {}
	}
	if len(cfg.Shell) == 0 {
		cfg.Shell = defaultExecShell()
	}
	signer, err := loadOrCreateHostKey(cfg.HostKeyFile)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, signer: signer}
	s.proxy = s.newServer(false)
	s.direct = s.newServer(true)
	return s, nil
}

func defaultExecShell() []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd.exe", "/c"}
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return []string{sh, "-c"}
	}
	return []string{"/bin/sh", "-c"}
}

// PublicKey returns the host key in authorized_keys format.
func (s *Server) PublicKey() string {
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(s.signer.PublicKey())))
}

func (s *Server) newServer(keyAuth bool) *ssh.Server {
	srv := &ssh.Server{
		Handler:     s.handleSession,
		HostSigners: []ssh.Signer{s.signer},
		Version:     "longarm",
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":      ssh.DefaultSessionHandler,
			"direct-tcpip": directTCPIPHandler,
		},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{"sftp": s.handleSFTP},
		LocalPortForwardingCallback: func(ctx ssh.Context, host string, port uint32) bool {
			s.cfg.Audit(who(ctx), "ssh direct-tcpip", map[string]any{"target": net.JoinHostPort(host, fmt.Sprint(port))})
			return true
		},
		PtyCallback: func(ssh.Context, ssh.Pty) bool { return true },
	}
	if keyAuth {
		srv.PublicKeyHandler = s.publicKeyAuth
	}
	// With no auth handler gliderlabs sets NoClientAuth: fine for the proxy
	// path only, whose callers authenticated with their certificate.
	return srv
}

// identConn tags a proxied connection with the caller's mTLS identity.
type identConn struct {
	net.Conn
	who string
}

type identAddr string

func (a identAddr) Network() string { return "longarm" }
func (a identAddr) String() string  { return string(a) }

func (c *identConn) RemoteAddr() net.Addr { return identAddr(c.who) }

// HandleProxyConn serves one connection that came through Forward.Ssh.
func (s *Server) HandleProxyConn(c net.Conn, who string) {
	s.proxy.HandleConn(&identConn{Conn: c, who: who})
}

// Serve accepts direct connections (public-key auth) on ln until Close.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.listeners = append(s.listeners, ln)
	s.mu.Unlock()
	err := s.direct.Serve(ln)
	if errors.Is(err, ssh.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops direct listeners and closes all connections.
func (s *Server) Close() error {
	s.proxy.Close()
	return s.direct.Close()
}

func who(ctx ssh.Context) string {
	if v, ok := ctx.Value(whoKey).(string); ok {
		return v
	}
	if a, ok := ctx.RemoteAddr().(identAddr); ok {
		return string(a)
	}
	return "ssh@" + ctx.RemoteAddr().String()
}

func (s *Server) publicKeyAuth(ctx ssh.Context, key ssh.PublicKey) bool {
	b, err := os.ReadFile(s.cfg.AuthorizedKeysFile)
	if err != nil {
		return false
	}
	for len(b) > 0 {
		k, comment, _, rest, err := gossh.ParseAuthorizedKey(b)
		if err != nil {
			break
		}
		b = rest
		if ssh.KeysEqual(k, key) {
			id := comment
			if id == "" {
				id = gossh.FingerprintSHA256(k)
			}
			ctx.SetValue(whoKey, "ssh-key/"+id+"@"+ctx.RemoteAddr().String())
			return true
		}
	}
	return false
}

func loadOrCreateHostKey(path string) (gossh.Signer, error) {
	if b, err := os.ReadFile(path); err == nil {
		return gossh.ParsePrivateKey(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := gossh.MarshalPrivateKey(priv, "longarmd host key")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		return nil, err
	}
	return gossh.NewSignerFromKey(priv)
}

// ---------------------------------------------------------------- sessions

func (s *Server) handleSession(sess ssh.Session) {
	ctx := sess.Context()
	ptyReq, winCh, isPty := sess.Pty()
	raw := sess.RawCommand()
	what := "ssh shell"
	if raw != "" {
		what = "ssh exec"
	}
	s.cfg.Audit(who(ctx), what, map[string]any{"command": raw, "pty": isPty, "user": sess.User()})
	env := append(os.Environ(), sess.Environ()...)
	if isPty && ptyReq.Term != "" {
		env = append(env, "TERM="+ptyReq.Term)
	}
	home, _ := os.UserHomeDir()
	var code int
	var err error
	if isPty {
		code, err = s.runPTY(sess, raw, home, env, ptyReq, winCh)
	} else {
		code, err = s.runPlain(sess, raw, home, env)
	}
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "longarmd: %v\r\n", err)
		s.cfg.Log.Warn("ssh session", "who", who(ctx), "err", err)
	}
	sess.Exit(code)
}

// argv returns the program, its arguments and (Windows) the raw command
// line for a session command; raw == "" means a shell.
func (s *Server) argv(raw string, pty bool) (argv []string, cmdline string) {
	if raw == "" {
		if pty && s.cfg.InteractiveShell != nil {
			return s.cfg.InteractiveShell(), ""
		}
		return s.cfg.Shell[:1], ""
	}
	if runtime.GOOS == "windows" {
		return s.cfg.Shell[:1], composeWindows(s.cfg.Shell) + " " + raw
	}
	return append(append([]string(nil), s.cfg.Shell...), raw), ""
}

// wrap prefixes `longarmd conhost` on Windows so the command gets a UTF-8 console.
func (s *Server) wrap(argv []string, cmdline string, pty bool) []string {
	if len(s.cfg.ConsoleHost) == 0 {
		return argv
	}
	w := append([]string(nil), s.cfg.ConsoleHost...)
	if pty {
		w = append(w, "--pty")
	}
	if cmdline != "" {
		return append(w, "--raw", "--", argv[0], cmdline)
	}
	return append(append(w, "--"), argv...)
}

func composeWindows(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = syscallEscape(a)
	}
	return strings.Join(q, " ")
}

const drainTimeout = 5 * time.Second

func (s *Server) runPlain(sess ssh.Session, raw, home string, env []string) (int, error) {
	argv, cmdline := s.argv(raw, false)
	ctr, err := platform.NewContainer(platform.ContainerOptions{Name: "longarm-ssh"})
	if err != nil {
		return 255, err
	}
	defer ctr.Close()
	var cmd *exec.Cmd
	if len(s.cfg.ConsoleHost) > 0 {
		wrapped := s.wrap(argv, cmdline, false)
		cmd = exec.Command(wrapped[0], wrapped[1:]...)
	} else {
		cmd = exec.Command(argv[0], argv[1:]...)
		if cmdline != "" {
			if err := platform.SetCmdLine(cmd, cmdline); err != nil {
				return 255, err
			}
		}
	}
	cmd.Dir, cmd.Env = home, env
	inR, inW, err := os.Pipe()
	if err != nil {
		return 255, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return 255, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return 255, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	startErr := ctr.Start(cmd)
	inR.Close()
	outW.Close()
	errW.Close()
	if startErr != nil {
		inW.Close()
		outR.Close()
		errR.Close()
		return 127, startErr
	}
	inputDone, finished := make(chan struct{}), make(chan struct{})
	defer close(finished)
	go func() {
		io.Copy(inW, sess) // ends on client EOF or when the channel closes
		inW.Close()
		close(inputDone)
	}()
	gone := channelGone(sess, inputDone, finished)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(sess, outR) }()
	go func() { defer wg.Done(); io.Copy(sess.Stderr(), errR) }()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case <-waited:
	case <-sess.Context().Done(): // connection gone
		ctr.Kill()
		<-waited
	case <-gone: // channel closed
		ctr.Kill()
		<-waited
	}
	ctr.Kill() // leftovers holding the pipes
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(drainTimeout):
		outR.Close()
		errR.Close()
		<-drained
	}
	outR.Close()
	errR.Close()
	inW.Close()
	return exitCode(cmd), nil
}

func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return 255
	}
	if c := cmd.ProcessState.ExitCode(); c >= 0 {
		return c
	}
	return 255
}

func (s *Server) runPTY(sess ssh.Session, raw, home string, env []string, req ssh.Pty, winCh <-chan ssh.Window) (int, error) {
	argv, cmdline := s.argv(raw, true)
	opts := platform.PTYOptions{Dir: home, Env: env, Cols: uint16(req.Window.Width), Rows: uint16(req.Window.Height)}
	if len(s.cfg.ConsoleHost) > 0 {
		opts.Argv = s.wrap(argv, cmdline, true)
	} else {
		opts.Argv, opts.CmdLine = argv, cmdline
	}
	ctr, err := platform.NewContainer(platform.ContainerOptions{Name: "longarm-ssh"})
	if err != nil {
		return 255, err
	}
	defer ctr.Close()
	p, err := platform.StartPTY(ctr, opts)
	if err != nil {
		return 127, err
	}
	defer p.Release()
	inputDone, finished := make(chan struct{}), make(chan struct{})
	defer close(finished)
	go func() {
		io.Copy(p, sess)
		close(inputDone)
	}()
	gone := channelGone(sess, inputDone, finished)
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		io.Copy(sess, p)
	}()
	go func() {
		for w := range winCh {
			if w.Width > 0 && w.Height > 0 {
				p.Resize(uint16(w.Width), uint16(w.Height))
			}
		}
	}()
	waited := make(chan struct{})
	var code int
	go func() {
		code, _ = p.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-sess.Context().Done(): // connection gone
		ctr.Kill()
		<-waited
	case <-gone: // channel closed
		ctr.Kill()
		<-waited
	}
	ctr.Kill()
	p.Close()
	select {
	case <-outDone:
	case <-time.After(drainTimeout):
	}
	return code, nil
}

// ---------------------------------------------------------------- sftp

func (s *Server) handleSFTP(sess ssh.Session) {
	s.cfg.Audit(who(sess.Context()), "ssh sftp", nil)
	// No WithServerWorkingDirectory: pkg/sftp would join "C:/x" (not
	// '/'-prefixed) onto it. longarmd runs in the home directory instead (see
	// longarmd.Run), so relative paths still resolve there, like OpenSSH.
	srv, err := sftp.NewServer(sess)
	if err != nil {
		s.cfg.Log.Warn("sftp", "err", err)
		sess.Exit(1)
		return
	}
	if err := srv.Serve(); err != nil && !errors.Is(err, io.EOF) {
		s.cfg.Log.Warn("sftp", "who", who(sess.Context()), "err", err)
	}
	// Exit status first: srv.Close closes the channel, and scp/sftp report
	// a failure when the channel closes without one.
	sess.Exit(0)
	srv.Close()
}

// channelGone reports when the client closed the session's channel. The
// session context of gliderlabs lives as long as the whole connection, and
// clients that multiplex (ControlMaster, VS Code) close channels on
// long-lived connections, so we probe: once the client's input has ended (a
// channel close always ends it), a closed channel rejects requests.
func channelGone(sess ssh.Session, inputDone, finished <-chan struct{}) <-chan struct{} {
	gone := make(chan struct{})
	go func() {
		select {
		case <-inputDone:
		case <-finished:
			return
		}
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			if _, err := sess.SendRequest("keepalive@openssh.com", false, nil); err != nil {
				close(gone)
				return
			}
			select {
			case <-finished:
				return
			case <-t.C:
			}
		}
	}()
	return gone
}
