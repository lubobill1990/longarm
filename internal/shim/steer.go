package shim

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/agentlog"
	"github.com/lubobill1990/longarm/internal/attach"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/steer"
)

// steerTick is how often a steerable run reads its hook events and asks
// its state machine what to do.
const steerTick = 200 * time.Millisecond

// answerWait bounds how long a Copilot answer is looked for in the
// transcript after its agentStop (the transcript may lag the hook).
const answerWait = 5 * time.Second

// SteerTimings, if set, adjusts a steerable run's timings (tests: a shim
// in the test binary).
var SteerTimings func(*steer.Config)

// steerRun drives a steerable job (spec.Steer; docs/steerable-runs.md):
// hook events and terminal activity into a steer.Machine, its keystrokes
// into the terminal, the agent's answers into out.log, its state into
// steer.json.
type steerRun struct {
	j      *job
	agent  string
	events string

	mu      sync.Mutex // m
	m       *steer.Machine
	off     int64  // read so far from events
	partial []byte // an unfinished last line
	last    jobs.SteerState
	answers int // written to out.log

	pending   string    // a Copilot transcript whose answer is not there yet
	pendingAt time.Time // when its agentStop came
}

// prepareSteer writes the run's hook configuration and returns the
// command with it (Claude's --settings).
func (j *job) prepareSteer(argv []string) (*steerRun, []string, error) {
	s := j.spec.Steer
	if j.spec.RunAs != nil {
		return nil, nil, errors.New("steerable runs are not supported with per-user isolation")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	dir := j.d.Path(jobs.SteerDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	events := filepath.Join(dir, steer.EventsFile)
	if err := os.WriteFile(events, nil, 0o600); err != nil {
		return nil, nil, err
	}
	cfg, err := steer.HookConfig(s.Agent, exe, events)
	if err != nil {
		return nil, nil, err
	}
	switch s.Agent {
	case steer.Claude:
		settings := filepath.Join(dir, "claude-settings.json")
		if err := os.WriteFile(settings, cfg, 0o600); err != nil {
			return nil, nil, err
		}
		argv = steer.Inject(steer.Claude, argv, settings)
	case steer.Copilot:
		home := envValue(j.env, "COPILOT_HOME")
		if home == "" {
			return nil, nil, errors.New("a steerable copilot run needs a private COPILOT_HOME (forwarded Copilot credentials)")
		}
		f := steer.CopilotHooksFile(home)
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(f, cfg, 0o600); err != nil { // no byte order mark: Copilot would ignore the file
			return nil, nil, err
		}
		if err := steer.CopilotTrust(home, j.workdir()); err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, fmt.Errorf("steerable runs run claude or copilot, not %q", s.Agent)
	}
	sc := steer.Defaults(s.Agent, s.UntilExit)
	if j.spec.Cols > 0 && j.spec.Rows > 0 {
		sc.Cols, sc.Rows = int(j.spec.Cols), int(j.spec.Rows)
	}
	if SteerTimings != nil {
		SteerTimings(&sc)
	}
	r := &steerRun{j: j, agent: s.Agent, events: events, m: steer.New(sc, time.Now())}
	r.writeState(time.Now(), true)
	return r, argv, nil
}

// envValue is the value of key in env ("" = unset; case-insensitive on
// Windows).
func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && (k == key || (runtime.GOOS == "windows" && strings.EqualFold(k, key))) {
			v = val
		}
	}
	return v
}

// output and input are the hub's observer (terminal activity).
func (r *steerRun) output(p []byte, t time.Time) {
	r.mu.Lock()
	r.m.Output(p, t)
	r.mu.Unlock()
}

func (r *steerRun) resize(cols, rows uint16) {
	r.mu.Lock()
	r.m.Resize(int(cols), int(rows))
	r.mu.Unlock()
}

func (r *steerRun) input(source string, t time.Time) {
	r.mu.Lock()
	r.m.Input(t, source == attach.SourceAttach || source == attach.SourceSend)
	r.mu.Unlock()
}

// loop runs until stop is closed: events, ticks, keystrokes.
func (r *steerRun) loop(h *hub, p platform.PTY, ctr platform.Container, stop <-chan struct{}) {
	t := time.NewTicker(steerTick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		now := time.Now()
		r.mu.Lock()
		r.readEvents(now)
		r.findPending(now)
		act := r.m.Tick(now)
		r.writeState(now, false)
		r.mu.Unlock()
		// typed outside r.mu: the hub calls input (r.mu) holding its own lock
		if act.Keys != "" {
			r.j.log.Info("steer", "action", act.What)
			if _, err := h.input(p, []byte(act.Keys), attach.SourceSteer, 0); err != nil {
				r.j.log.Warn("steer: terminal input", "err", err)
			}
		}
		if act.Kill {
			r.j.log.Info("steer", "action", act.What)
			if err := ctr.Kill(); err != nil {
				r.j.log.Error("steer: kill", "err", err)
			}
		}
	}
}

// finish reads the last events and returns the run's outcome for the
// agent's exit code (and records state done).
func (r *steerRun) finish(code int) (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.readEvents(now)
	if r.pending != "" {
		r.pendingAt = time.Time{} // last try
		r.findPending(now)
	}
	code, reason := r.m.Outcome(code)
	st := r.m.State()
	st.State, st.Message, st.UpdatedAt = jobs.SteerDone, reason, now.UTC()
	if err := r.j.d.WriteSteer(st); err != nil {
		r.j.log.Warn("steer: write state", "err", err)
	}
	return code, reason
}

// readEvents feeds the events appended since the last call (r.mu held).
func (r *steerRun) readEvents(now time.Time) {
	f, err := os.Open(r.events)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(r.off, io.SeekStart); err != nil {
		return
	}
	b, err := io.ReadAll(io.LimitReader(f, 16<<20))
	if err != nil || len(b) == 0 {
		return
	}
	r.off += int64(len(b))
	b = append(r.partial, b...)
	i := bytes.LastIndexByte(b, '\n')
	if i < 0 {
		r.partial = b
		return
	}
	r.partial = append([]byte(nil), b[i+1:]...)
	for _, line := range bytes.Split(b[:i], []byte{'\n'}) {
		ev, ok := steer.ParseEvent(bytes.TrimSpace(line))
		if !ok {
			continue
		}
		r.m.Event(ev, now)
		if ev.Kind != steer.EvStop {
			continue
		}
		switch {
		case ev.Answer != "":
			r.writeAnswer(ev.Answer)
		case ev.Transcript != "":
			r.pending, r.pendingAt = ev.Transcript, now
		}
	}
}

// findPending looks for the answer of the last stop in its transcript
// (r.mu held); a zero pendingAt: this is the last try.
func (r *steerRun) findPending(now time.Time) {
	if r.pending == "" {
		return
	}
	if text := lastAnswer(r.agent, r.pending); text != "" {
		r.writeAnswer(text)
	} else if !r.pendingAt.IsZero() && now.Sub(r.pendingAt) < answerWait {
		return
	}
	r.pending = ""
}

// lastAnswer is the assistant's text of the last turn in a transcript.
func lastAnswer(agent, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var lines []agentlog.Line
	var off int64
	for _, l := range bytes.Split(b, []byte{'\n'}) {
		lines = append(lines, agentlog.Line{Offset: off, Data: bytes.TrimRight(l, "\r")})
		off += int64(len(l)) + 1
	}
	msgs := agentlog.Parse(agent, lines, 0)
	var parts []string
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == agentlog.RoleUser && m.Kind == agentlog.KindText {
			break
		}
		if m.Role == agentlog.RoleAssistant && m.Kind == agentlog.KindText && strings.TrimSpace(m.Text) != "" {
			parts = append(parts, strings.TrimSpace(m.Text))
		}
	}
	for i, k := 0, len(parts)-1; i < k; i, k = i+1, k-1 {
		parts[i], parts[k] = parts[k], parts[i]
	}
	return strings.Join(parts, "\n\n")
}

// writeAnswer appends a turn's answer to out.log (r.mu held).
func (r *steerRun) writeAnswer(text string) {
	text = strings.TrimRight(text, "\r\n ")
	if text == "" {
		return
	}
	if r.answers > 0 {
		text = "\n" + text
	}
	r.answers++
	if _, err := io.WriteString(r.j.out, text+"\n"); err != nil {
		r.j.log.Warn("steer: write answer", "err", err)
	}
	if err := r.j.out.Flush(); err != nil {
		r.j.log.Warn("steer: flush answer", "err", err)
	}
}

// writeState writes steer.json when the state changed (r.mu held).
func (r *steerRun) writeState(now time.Time, force bool) {
	st := r.m.State()
	if !force && st == r.last {
		return
	}
	r.last = st
	st.UpdatedAt = now.UTC()
	if err := r.j.d.WriteSteer(st); err != nil {
		r.j.log.Warn("steer: write state", "err", err)
	}
}
