package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/platform"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// detachPrefix (Ctrl-]) followed by 'd' detaches; Ctrl-] Ctrl-] sends one
// literal Ctrl-].
const detachPrefix = 0x1d

func attachCmd() *cobra.Command {
	var session, cwd, owner string
	var env, secrets []string
	var kill, noReplay, noCreate bool
	c := &cobra.Command{
		Use:   "attach <worker> [--session NAME] [-- command [args...]]",
		Short: "Attach to a named interactive session (created if missing); detach with Ctrl-] d",
		Long: `Attach to an interactive terminal session on the worker, tmux-style.

The session (default name "default") is created on first attach, running the
given command or the worker user's shell (pwsh on Windows). It keeps running
when you detach (Ctrl-] then d), close the terminal or lose the network, and
when longarmd restarts. Attaching again replays recent output, then continues live.
Several clients may attach to one session at the same time. longarm exits with the
session's exit code when its process ends.

Secrets (--secret-env) are injected when the session is created: memory-only
on the worker and redacted from the session transcript (longarm logs --session).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			worker := args[0]
			var argv []string
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				if dash != 1 {
					return errors.New("usage: longarm attach <worker> [--session NAME] [-- command...]")
				}
				argv = args[1:]
			} else if len(args) > 1 {
				return errors.New("put the command after --")
			}
			envM, err := parseEnv(env)
			if err != nil {
				return err
			}
			sec, err := secretEnv(secrets)
			if err != nil {
				return err
			}
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			conn, err := s.conn(worker)
			if err != nil {
				return err
			}
			defer conn.Close()
			if kill {
				name := session
				if name == "" {
					name = "default"
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
				defer cancel()
				ss, err := conn.Sessions().Kill(ctx, &pb.KillSessionRequest{Name: name, Owner: owner})
				if err != nil {
					return err
				}
				fmt.Printf("%s %s\n", ss.Name, stateName(ss.State))
				return nil
			}
			open := &pb.AttachOpen{Name: session, Argv: argv, Cwd: cwd, Env: envM, SecretEnv: sec,
				NoCreate: noCreate, NoReplay: noReplay, Owner: owner}
			return runAttach(cmd.Context(), conn, open)
		},
	}
	c.Flags().StringVarP(&session, "session", "s", "", `session name (default "default")`)
	c.Flags().StringVar(&cwd, "cwd", "", "working directory for a new session (default: home)")
	c.Flags().StringArrayVarP(&env, "env", "e", nil, "KEY=VALUE for a new session (repeatable)")
	c.Flags().StringArrayVar(&secrets, "secret-env", nil, "NAME or NAME=LOCALVAR: local env var injected into a new session (memory-only, redacted)")
	c.Flags().BoolVar(&kill, "kill", false, "kill the session's process tree instead of attaching")
	c.Flags().BoolVar(&noReplay, "no-replay", false, "do not replay recent output")
	c.Flags().BoolVar(&noCreate, "no-create", false, "fail if the session does not exist")
	c.Flags().StringVar(&owner, "owner", "", "admins: another user's session (multi-tenancy)")
	return c
}

// inputReader reads stdin, handles the detach sequence and delivers input.
type inputReader struct {
	data     chan []byte
	detach   chan struct{} // closed on Ctrl-] d
	eof      chan struct{} // closed when stdin ends
	onDetach sync.Once
}

func startInput(r io.Reader) *inputReader {
	in := &inputReader{data: make(chan []byte, 64), detach: make(chan struct{}), eof: make(chan struct{})}
	go func() {
		buf := make([]byte, 4096)
		pending := false // saw detachPrefix
		for {
			n, err := r.Read(buf)
			var out []byte
			for _, b := range buf[:n] {
				switch {
				case pending && (b == 'd' || b == 'D'):
					if len(out) > 0 {
						in.data <- out
					}
					in.onDetach.Do(func() { close(in.detach) })
					return
				case pending && b == detachPrefix:
					out = append(out, detachPrefix)
					pending = false
				case pending:
					out = append(out, detachPrefix, b)
					pending = false
				case b == detachPrefix:
					pending = true
				default:
					out = append(out, b)
				}
			}
			if len(out) > 0 {
				in.data <- out
			}
			if err != nil {
				close(in.eof)
				return
			}
		}
	}()
	return in
}

type attachEnd struct {
	exited   bool
	code     int
	detached bool
}

func runAttach(ctx context.Context, conn *client.Conn, open *pb.AttachOpen) error {
	stdinFd, stdoutFd := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	tty := term.IsTerminal(stdinFd) && term.IsTerminal(stdoutFd)
	size := func() *pb.TermSize {
		if !tty {
			return nil
		}
		w, h, err := term.GetSize(stdoutFd)
		if err != nil || w <= 0 || h <= 0 {
			return nil
		}
		return &pb.TermSize{Cols: uint32(w), Rows: uint32(h)}
	}
	open.Size = size()
	var src io.Reader = os.Stdin
	if !tty {
		// Scripted input: a newline means Enter, which terminals send as CR
		// (ConPTY does not treat LF as Enter; a Unix pty maps CR to NL).
		src = crReader{os.Stdin}
	}
	in := startInput(src)
	var restore func()
	defer func() {
		if restore != nil {
			restore()
		}
	}()
	enterRaw := func() {
		if !tty || restore != nil {
			return
		}
		old, err := term.MakeRaw(stdinFd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "longarm: raw mode: %v\n", err)
			restore = func() {}
			return
		}
		outRestore, _ := platform.TerminalOutput(os.Stdout)
		restore = func() {
			outRestore()
			term.Restore(stdinFd, old)
		}
	}
	name := open.Name
	if name == "" {
		name = "default"
	}
	notice := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "\r\n[longarm] "+format+"\r\n", a...)
	}
	first := true
	var end attachEnd
	err := client.Retry(ctx, 10*time.Minute, func(err error, wait time.Duration) {
		notice("connection to %s lost (%s); re-attaching in %s", conn.Worker.Name, oneLine(err), wait)
	}, func(ctx context.Context, progress func()) error {
		var err error
		end, err = attachOnce(ctx, conn, open, in, size, func(started *pb.AttachStarted) {
			progress()
			if first {
				verb := "attached to"
				if started.Created {
					verb = "created"
				}
				notice("%s session %q on %s; detach with Ctrl-] d", verb, name, conn.Worker.Name)
				first = false
			}
			enterRaw()
		}, progress)
		if err == nil {
			// Re-attach after a disconnect: never re-create a session
			// that ended meanwhile.
			open.NoCreate = true
		}
		return err
	})
	if restore != nil {
		restore()
		restore = nil
	}
	if err != nil {
		return err
	}
	switch {
	case end.exited:
		if !first {
			notice("session %q ended (exit %d)", name, end.code)
		}
		if end.code != 0 {
			code := end.code
			if code < 0 || code > 255 {
				code = exitLongarmError
			}
			return &exitError{code: code}
		}
	case end.detached:
		notice("detached from session %q (longarm attach %s --session %s to resume)", name, conn.Worker.Name, name)
	}
	return nil
}

// attachOnce runs one Attach stream until the session exits, the user
// detaches, or the connection breaks (error, retryable if Unavailable).
func attachOnce(ctx context.Context, conn *client.Conn, open *pb.AttachOpen, in *inputReader,
	size func() *pb.TermSize, onStart func(*pb.AttachStarted), progress func()) (attachEnd, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := conn.Sessions().Attach(ctx)
	if err != nil {
		return attachEnd{}, err
	}
	if err := st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Open{Open: open}}); err != nil {
		return attachEnd{}, recvErr(st, err)
	}
	m, err := st.Recv()
	if err != nil {
		return attachEnd{}, err
	}
	started := m.GetStarted()
	if started == nil {
		return attachEnd{}, errors.New("protocol: expected started")
	}
	onStart(started)

	var detached bool
	vt := &win32InputFilter{}
	// whatever the stream ends with (exit, detach, EOF, an error) gets out,
	// also a held-back prefix that turned out not to be the sequence
	defer func() { os.Stdout.Write(vt.flush()) }()
	var dmu sync.Mutex
	go func() { // the only sender
		last := open.Size
		resized := make(chan *pb.TermSize, 1)
		go watchResize(ctx, func() {
			if sz := size(); sz != nil && (last == nil || sz.Cols != last.Cols || sz.Rows != last.Rows) {
				last = sz
				select {
				case resized <- sz:
				default:
				}
			}
		})
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-in.data:
				if st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Data{Data: b}}) != nil {
					return
				}
			case sz := <-resized:
				if st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Resize{Resize: sz}}) != nil {
					return
				}
			case <-in.detach:
				dmu.Lock()
				detached = true
				dmu.Unlock()
				st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Detach{Detach: true}})
				st.CloseSend()
				return
			case <-in.eof:
				dmu.Lock()
				detached = true
				dmu.Unlock()
				st.CloseSend()
				return
			}
		}
	}()
	for {
		m, err := st.Recv()
		if errors.Is(err, io.EOF) {
			dmu.Lock()
			d := detached
			dmu.Unlock()
			if !d {
				return attachEnd{}, status.Error(codes.Unavailable, "session stream ended unexpectedly")
			}
			return attachEnd{detached: true}, nil
		}
		if err != nil {
			return attachEnd{}, err
		}
		progress()
		switch x := m.Msg.(type) {
		case *pb.AttachOutput_Data:
			os.Stdout.Write(vt.filter(x.Data))
		case *pb.AttachOutput_ExitCode:
			return attachEnd{exited: true, code: int(x.ExitCode)}, nil
		}
	}
}

// recvErr returns the stream's real status when Send failed with EOF.
func recvErr(st pb.Sessions_AttachClient, err error) error {
	if errors.Is(err, io.EOF) {
		if _, rerr := st.Recv(); rerr != nil {
			return rerr
		}
	}
	return err
}

// crReader turns LF (and CRLF) into CR.
type crReader struct{ r io.Reader }

func (c crReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	out := p[:0]
	for i := 0; i < n; i++ {
		switch {
		case p[i] == '\n' && len(out) > 0 && out[len(out)-1] == '\r':
		case p[i] == '\n':
			out = append(out, '\r')
		default:
			out = append(out, p[i])
		}
	}
	return len(out), err
}

// win32InputFilter drops the win32-input-mode switch (ESC[?9001h / l) from
// the session's output. A Windows worker's pseudo console sends it to ask
// its terminal for win32 key events; passed on to a terminal that is itself
// a pseudo console (longarm attach inside ConPTY - portable-pty, xterm.js
// in a desktop app - or a nested session), that console then waits for
// such events and drops the plain keystrokes its host writes: input stops.
// Plain VT input works everywhere, so longarm never asks for the mode.
type win32InputFilter struct{ carry []byte }

var (
	win32InputOn  = []byte("\x1b[?9001h")
	win32InputOff = []byte("\x1b[?9001l")
)

// flush returns the bytes held back at the end of the stream.
func (f *win32InputFilter) flush() []byte {
	b := f.carry
	f.carry = nil
	return b
}

func (f *win32InputFilter) filter(p []byte) []byte {
	b := p
	if len(f.carry) > 0 {
		b = append(f.carry, p...)
		f.carry = nil
	}
	if bytes.IndexByte(b, 0x1b) < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] == 0x1b {
			rest := b[i:]
			if bytes.HasPrefix(rest, win32InputOn) || bytes.HasPrefix(rest, win32InputOff) {
				i += len(win32InputOn)
				continue
			}
			if len(rest) < len(win32InputOn) && (bytes.HasPrefix(win32InputOn, rest) || bytes.HasPrefix(win32InputOff, rest)) {
				f.carry = append([]byte(nil), rest...) // maybe split across chunks
				break
			}
		}
		out = append(out, b[i])
		i++
	}
	return out
}
