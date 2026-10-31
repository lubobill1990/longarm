package shim

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lubobill1990/longarm/internal/attach"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/logfile"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/redact"
)

// Scrollback is how much recent raw terminal output a session keeps in
// memory to replay on re-attach. Memory only: the on-disk transcript
// (out.log) is the redacted copy.
const Scrollback = 256 << 10

// clientQueue bounds the frames buffered for one attached client; a client
// that falls this far behind is disconnected (it can re-attach and replay).
const clientQueue = 4096

// helloTimeout: a connection must identify itself quickly.
const helloTimeout = 10 * time.Second

// ChannelName derives the attach channel name of a session dir (short and
// stable, so a restarted longarmd and the shim agree without coordination).
func ChannelName(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return "s-" + hex.EncodeToString(sum[:12])
}

// runSession runs an interactive session: the command on a pseudo terminal,
// its output into the scrollback ring, every attached client and out.log
// (a steerable run: term.log; out.log gets the agent's answers).
func (j *job) runSession(ctr platform.Container) error {
	argv := j.command(j.spec.Argv)
	var sr *steerRun
	term := io.Writer(j.out)
	if j.spec.Steer != nil {
		var err error
		if sr, argv, err = j.prepareSteer(argv); err != nil {
			return j.failStart("steerable run", err)
		}
		tl, err := logfile.OpenWriter(j.d.Path(jobs.TermLog), j.spec.LogMaxBytes)
		if err != nil {
			return j.failStart("steerable run", err)
		}
		defer tl.Close()
		tw := redact.NewWriter(tl, j.red)
		defer tw.Flush() // before tl.Close (deferred calls run last-in first-out)
		term = tw
	}
	if runtime.GOOS == "windows" {
		// Run through `longarmd conhost --pty` so the pseudo console uses UTF-8
		// code pages and Ctrl+C reaching the console cannot end the session.
		if exe, err := os.Executable(); err == nil {
			argv = append([]string{exe, "conhost", "--pty", "--"}, argv...)
		}
	}
	ln, addr, err := platform.ListenLocal(ChannelName(string(j.d)))
	if err != nil {
		return j.failStart("attach channel", err)
	}
	defer ln.Close()
	p, err := platform.StartPTY(ctr, platform.PTYOptions{
		Argv: argv, Dir: j.workdir(), Env: j.env, Cols: j.spec.Cols, Rows: j.spec.Rows,
	})
	if err != nil {
		return j.failStart("failed to start "+j.spec.Argv[0], err)
	}
	defer p.Release()

	j.st.State = jobs.Running
	j.st.PID = p.Pid()
	j.st.PIDStart, _ = platform.ProcessStartTime(j.st.PID)
	j.st.StartedAt = time.Now().UTC()
	j.st.AttachAddr = addr
	if err := j.d.WriteStatus(j.st); err != nil {
		j.log.Error("write running status", "err", err)
	}
	j.log.Info("session started", "pid", j.st.PID, "argv", j.spec.Argv, "attach", addr)

	h := &hub{ring: make([]byte, 0, Scrollback), clients: map[*client]struct{}{}}
	if sr != nil {
		h.obs = sr
	}
	go h.serve(ln, p, j)

	dst := &sink{w: term, log: j.log}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		buf := make([]byte, 32<<10)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				h.output(buf[:n])
				dst.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	var killed atomic.Bool
	stopWatch := make(chan struct{})
	go j.watchKill(ctr, &killed, stopWatch)
	steerDone := make(chan struct{})
	if sr != nil {
		go func() {
			defer close(steerDone)
			sr.loop(h, p, ctr, stopWatch)
		}()
	} else {
		close(steerDone)
	}

	code, werr := p.Wait()
	if werr != nil {
		j.log.Warn("wait", "err", werr)
	}
	close(stopWatch)
	<-steerDone // it writes out.log: done before the log is flushed
	_ = ctr.Kill()
	p.Close()
	select {
	case <-copied:
	case <-time.After(drainTimeout):
		// Something escaped the kill and still holds the terminal. Stop
		// recording before the log is closed; closing our side of the
		// terminal unblocks the reader.
		j.log.Warn("terminal output did not end after kill")
		p.Release()
	}
	dst.Stop()
	reason := ""
	if killed.Load() {
		code = platform.KilledExitCode
	} else if sr != nil {
		code, reason = sr.finish(code)
	}
	if err := j.out.Flush(); err != nil {
		j.log.Error("flush log", "err", err)
	}
	_ = j.logf.Sync()
	// Record the final state before telling attached clients, so that what
	// they see next (longarm sessions, longarm logs) is consistent.
	if reason != "" {
		err = j.finish(jobs.Failed, code, reason)
	} else {
		err = j.finishExit(killed.Load(), code)
	}
	h.exit(code)
	return err
}

type frame struct {
	typ  byte
	data []byte
}

type client struct {
	conn *attach.Conn
	out  chan frame
	once sync.Once
}

// drop closes the client's queue; its writer then closes the connection.
func (c *client) drop() { c.once.Do(func() { close(c.out) }) }

// hub fans terminal output out to attached clients and keeps the ring.
type hub struct {
	mu      sync.Mutex
	ring    []byte
	clients map[*client]struct{}
	done    bool
	code    int
	lastOut time.Time

	// in serializes writes to the terminal: each Input / Send frame is one
	// contiguous write, never interleaved with another client's.
	in       sync.Mutex
	inSeq    uint64
	lastIn   time.Time
	lastFrom string

	// obs (a steerable run) sees the terminal's activity; set before
	// serve.
	obs observer
}

// observer is told about terminal output, input and resizes as they happen.
type observer interface {
	output(p []byte, t time.Time)
	input(source string, t time.Time)
	resize(cols, rows uint16)
}

// input writes p to the terminal as one write and records it. With
// ifIdle > 0 it writes only if no input arrived within ifIdle (false, nil).
// A failed or short write is (false, err): never reported as sent.
func (h *hub) input(w io.Writer, p []byte, source string, ifIdle time.Duration) (bool, error) {
	h.in.Lock()
	defer h.in.Unlock()
	if ifIdle > 0 && !h.lastIn.IsZero() && time.Since(h.lastIn) < ifIdle {
		return false, nil
	}
	if len(p) == 0 {
		return true, nil
	}
	n, err := w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	if n > 0 { // the terminal got (some of) it
		h.inSeq++
		h.lastIn, h.lastFrom = time.Now().UTC(), source
		if h.obs != nil {
			h.obs.input(source, h.lastIn)
		}
	}
	return err == nil, err
}

// info is the terminal's activity.
func (h *hub) info() attach.InfoState {
	h.in.Lock()
	st := attach.InfoState{InputSeq: h.inSeq, LastInput: h.lastIn, LastInputSource: h.lastFrom}
	h.in.Unlock()
	h.mu.Lock()
	st.LastOutput = h.lastOut
	h.mu.Unlock()
	return st
}

func (h *hub) output(p []byte) {
	now := time.Now().UTC()
	if h.obs != nil {
		h.obs.output(p, now)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastOut = now
	if over := len(h.ring) + len(p) - Scrollback; over > 0 {
		if over >= len(h.ring) {
			h.ring = h.ring[:0]
			if len(p) > Scrollback {
				p = p[len(p)-Scrollback:]
			}
		} else {
			h.ring = append(h.ring[:0], h.ring[over:]...)
		}
	}
	h.ring = append(h.ring, p...)
	data := append([]byte(nil), p...)
	for c := range h.clients {
		select {
		case c.out <- frame{attach.Output, data}:
		default:
			delete(h.clients, c)
			c.drop()
		}
	}
}

// add registers a client, queueing the replay first so no output is lost
// or duplicated between replay and live data.
func (h *hub) add(c *client, replay bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if replay && len(h.ring) > 0 {
		c.out <- frame{attach.Output, append([]byte(nil), h.ring...)}
	}
	if h.done {
		c.out <- frame{attach.Exit, attach.ExitPayload(h.code)}
		c.drop()
		return false
	}
	h.clients[c] = struct{}{}
	return true
}

func (h *hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	c.drop()
}

// exit tells every client the session ended and disconnects them.
func (h *hub) exit(code int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.done, h.code = true, code
	for c := range h.clients {
		select {
		case c.out <- frame{attach.Exit, attach.ExitPayload(code)}:
		default:
		}
		delete(h.clients, c)
		c.drop()
	}
}

func (h *hub) serve(ln net.Listener, p platform.PTY, j *job) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				j.log.Warn("attach accept", "err", err)
			}
			return
		}
		go h.handle(conn, p, j)
	}
}

func (h *hub) handle(nc net.Conn, p platform.PTY, j *job) {
	conn := attach.NewConn(nc)
	nc.SetReadDeadline(time.Now().Add(helloTimeout))
	typ, payload, err := conn.ReadFrame()
	if err != nil || typ != attach.Hello || len(payload) < 1 {
		conn.Close()
		return
	}
	nc.SetReadDeadline(time.Time{})
	if payload[0]&attach.FlagControl != 0 {
		h.control(conn, p, j)
		return
	}
	c := &client{conn: conn, out: make(chan frame, clientQueue)}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer conn.Close()
		for f := range c.out {
			if err := conn.WriteFrame(f.typ, f.data); err != nil {
				h.remove(c)
				for range c.out { // drain until dropped
				}
				return
			}
		}
	}()
	if !h.add(c, payload[0]&attach.FlagReplay != 0) {
		<-writerDone
		return
	}
	for {
		typ, payload, err := conn.ReadFrame()
		if err != nil {
			break
		}
		switch typ {
		case attach.Input:
			if _, err := h.input(p, payload, attach.SourceAttach, 0); err != nil {
				j.log.Warn("terminal input", "err", err)
			}
		case attach.Resize:
			if cols, rows, ok := attach.ParseResize(payload); ok && cols > 0 && rows > 0 {
				if err := p.Resize(cols, rows); err != nil {
					j.log.Warn("resize", "err", err)
				} else if h.obs != nil {
					h.obs.resize(cols, rows)
				}
			}
		}
	}
	h.remove(c)
	<-writerDone
}

// control serves a control connection (longarmd's Sessions.Send / Stop and
// the activity of Sessions.List): Info at once, then Info per Query / Send.
// It never gets output and does not count as attached.
func (h *hub) control(conn *attach.Conn, p platform.PTY, j *job) {
	defer conn.Close()
	if conn.WriteFrame(attach.Info, attach.EncodeInfo(h.info())) != nil {
		return
	}
	for {
		typ, payload, err := conn.ReadFrame()
		if err != nil {
			return
		}
		switch typ {
		case attach.Query:
		case attach.Send:
			ifIdle, source, data, err := attach.ParseSend(payload)
			if err != nil {
				return
			}
			sent, err := h.input(p, data, source, time.Duration(ifIdle)*time.Millisecond)
			st := h.info()
			st.Sent = sent
			if err != nil {
				j.log.Warn("terminal input", "err", err)
				st.Error = err.Error()
			}
			if conn.WriteFrame(attach.Info, attach.EncodeInfo(st)) != nil {
				return
			}
			continue
		default:
			continue
		}
		if conn.WriteFrame(attach.Info, attach.EncodeInfo(h.info())) != nil {
			return
		}
	}
}
