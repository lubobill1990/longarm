package steer

import (
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
)

// Config tunes a Machine; Defaults has the values of the spec.
type Config struct {
	Agent      string
	UntilExit  bool // never end the run: the agent exits by itself
	Cols, Rows int  // the terminal's size at the start

	Quiet         time.Duration // the terminal printed nothing for this long
	QuietCap      time.Duration // ... unless the stop is this old (a TUI that keeps printing)
	HumanGrace    time.Duration // nobody typed for this long; a steered run's stop is this old
	BackgroundCap time.Duration // a stop with background tasks: done anyway after this long
	ScreenQuiet   time.Duration // a start-up screen is drawn once the terminal is quiet this long
	ScreenRetry   time.Duration // an answered screen still there (nothing redrawn) this long: answer again
	StuckAfter    time.Duration // no prompt submitted and quiet this long: needs you
	ExitGrace     time.Duration // after /exit: Ctrl-C after this long ...
	KillAfter     time.Duration // ... and the process tree killed this much later
	MaxAnswers    int           // start-up screens answered at most
}

// Defaults are the timings of docs/steerable-runs.md section 4.
func Defaults(agent string, untilExit bool) Config {
	return Config{
		Agent: agent, UntilExit: untilExit, Cols: 120, Rows: 40,
		Quiet: 5 * time.Second, QuietCap: 2 * time.Minute, HumanGrace: 60 * time.Second,
		BackgroundCap: 30 * time.Minute, ScreenQuiet: 400 * time.Millisecond, ScreenRetry: 2 * time.Second, StuckAfter: 60 * time.Second,
		ExitGrace: 15 * time.Second, KillAfter: 5 * time.Second, MaxAnswers: 20,
	}
}

// The steps of typing /exit (one at a time, a tick apart: a TUI reading a
// chunk at once could take "/exit\r" as text).
const (
	exitEsc   = iota // Esc: closes a dialog the agent opened after its turn
	exitClear        // Ctrl-C: clears a draft in the input
	exitType         // "/exit"
	exitEnter        // Enter
	exitWait         // wait for the agent to exit
)

// exitStep is the pause between the steps.
const exitStep = 500 * time.Millisecond

// Action is what the shim does after a Tick.
type Action struct {
	Keys string // type these (source "steer")
	What string // why, for the shim's log
	Kill bool   // kill the process tree
}

// Machine decides what a steerable run does next from its hook events,
// terminal activity and the clock (docs/steerable-runs.md section 4).
type Machine struct {
	cfg     Config
	started time.Time

	submitted          bool
	submitAt, stopAt   time.Time
	turnEnds, turns    int
	background         bool
	permission, failed string
	sessionID, script  string

	steered   bool
	lastHuman time.Time
	lastOut   time.Time

	screen     *Screen   // until the first prompt: what the terminal shows
	answered   string    // the screen last answered
	answeredAt time.Time // when
	answers    int
	stuck      bool

	exiting  bool
	exitStep int
	exitAt   time.Time // when the current exit step was taken
	exitFrom time.Time // when /exit was entered
	ctrlC    int
	killed   bool
}

// New starts a machine at now.
func New(cfg Config, now time.Time) *Machine {
	return &Machine{cfg: cfg, started: now, lastOut: now, screen: NewScreen(cfg.Cols, cfg.Rows)}
}

// Resize records the terminal's new size.
func (m *Machine) Resize(cols, rows int) {
	if m.screen != nil {
		m.screen.Resize(cols, rows)
	}
}

// Output records terminal output.
func (m *Machine) Output(p []byte, now time.Time) {
	m.lastOut = now
	if m.screen == nil || len(p) == 0 {
		return
	}
	m.screen.Write(p)
	m.stuck = false
}

// Input records input that reached the terminal; human: from an attached
// client or Sessions.Send, not the machine's own keystrokes.
func (m *Machine) Input(now time.Time, human bool) {
	if !human {
		return
	}
	m.lastHuman = now
	m.steered = true
	m.permission = "" // answered (or being answered)
}

// Event records a hook event.
func (m *Machine) Event(ev Event, now time.Time) {
	if ev.SessionID != "" {
		m.sessionID = ev.SessionID
	}
	if ev.Transcript != "" {
		m.script = ev.Transcript
	}
	switch ev.Kind {
	case EvSubmit:
		m.submitted, m.submitAt = true, now
		m.permission, m.background, m.stuck = "", false, false
		m.screen = nil // start-up is over
	case EvStop:
		m.submitted = true
		m.turnEnds++
		m.turns++
		m.stopAt = now
		m.background = ev.Background
		m.permission, m.failed = "", ""
	case EvFail:
		m.submitted = true
		m.turnEnds++
		m.stopAt = now
		m.background, m.permission = false, ""
		m.failed = ev.Message
	case EvNotify:
		if ev.Permission {
			m.permission = ev.Message
			if m.permission == "" {
				m.permission = "permission needed"
			}
		}
	}
}

// turnOver: the last turn ended (no prompt since).
func (m *Machine) turnOver() bool {
	return m.turnEnds > 0 && !m.stopAt.Before(m.submitAt)
}

// done: section 4 of the spec.
func (m *Machine) done(now time.Time) bool {
	if !m.turnOver() || m.permission != "" {
		return false
	}
	sinceStop := now.Sub(m.stopAt)
	if m.background && sinceStop < m.cfg.BackgroundCap {
		return false
	}
	if now.Sub(m.lastOut) < m.cfg.Quiet && sinceStop < m.cfg.QuietCap {
		return false
	}
	if !m.lastHuman.IsZero() && now.Sub(m.lastHuman) < m.cfg.HumanGrace {
		return false
	}
	return !m.steered || sinceStop >= m.cfg.HumanGrace
}

// Tick decides the next step.
func (m *Machine) Tick(now time.Time) Action {
	if m.exiting {
		return m.exitTick(now)
	}
	if !m.submitted {
		return m.startupTick(now)
	}
	if m.cfg.UntilExit || !m.done(now) {
		return Action{}
	}
	m.exiting, m.exitStep, m.exitAt = true, exitEsc, now
	return Action{Keys: KeyEsc, What: "done: close a dialog"}
}

func (m *Machine) exitTick(now time.Time) Action {
	switch m.exitStep {
	case exitEsc:
		if now.Sub(m.exitAt) >= exitStep {
			m.exitStep, m.exitAt = exitClear, now
			return Action{Keys: KeyCtrlC, What: "done: clear the input"}
		}
	case exitClear:
		if now.Sub(m.exitAt) >= exitStep {
			m.exitStep, m.exitAt = exitType, now
			return Action{Keys: "/exit", What: "done: type /exit"}
		}
	case exitType:
		if now.Sub(m.exitAt) >= exitStep {
			m.exitStep, m.exitAt, m.exitFrom = exitEnter, now, now
			return Action{Keys: KeyEnter, What: "done: enter /exit"}
		}
	case exitEnter:
		el := now.Sub(m.exitFrom)
		if el >= m.cfg.ExitGrace && m.ctrlC < 2 && now.Sub(m.exitAt) >= time.Second {
			m.ctrlC++
			m.exitAt = now
			return Action{Keys: KeyCtrlC, What: "no exit after /exit: Ctrl-C"}
		}
		if el >= m.cfg.ExitGrace+m.cfg.KillAfter && !m.killed {
			m.killed = true
			return Action{Kill: true, What: "no exit after /exit and Ctrl-C: kill"}
		}
	}
	return Action{}
}

// startupTick answers a known start-up screen once it is drawn (the
// terminal quiet): at once when it changed since the last answer, again
// after ScreenRetry when it did not (a TUI not reading its keyboard yet
// ignores keys).
func (m *Machine) startupTick(now time.Time) Action {
	quiet := now.Sub(m.lastOut)
	if m.screen != nil && quiet >= m.cfg.ScreenQuiet && m.answers < m.cfg.MaxAnswers {
		text := m.screen.Text()
		if keys, what := StartupAnswer(m.cfg.Agent, text); keys != "" && (text != m.answered || now.Sub(m.answeredAt) >= m.cfg.ScreenRetry) {
			if text == m.answered {
				what += " (again: nothing changed)"
			}
			m.answers++
			m.answered, m.answeredAt = text, now
			return Action{Keys: keys, What: "start-up: " + what}
		}
	}
	if quiet >= m.cfg.StuckAfter {
		m.stuck = true
	}
	return Action{}
}

// State is the run's state for steer.json (UpdatedAt is the caller's).
func (m *Machine) State() jobs.SteerState {
	st := jobs.SteerState{Turns: m.turns, SessionID: m.sessionID, Transcript: m.script}
	switch {
	case m.exiting:
		st.State = jobs.SteerFinishing
	case !m.submitted && (m.stuck || m.answers >= m.cfg.MaxAnswers):
		st.State = jobs.SteerNeedsYou
		st.Message = "waiting at a start-up screen or for a prompt: attach to see it"
	case !m.submitted:
		st.State = jobs.SteerStarting
	case m.permission != "":
		st.State, st.Message = jobs.SteerNeedsYou, m.permission
	case m.turnOver():
		st.State = jobs.SteerIdle
		st.Message = m.failed
	default:
		st.State = jobs.SteerWorking
	}
	return st
}

// Outcome is the run's exit code and reason ("" = none) once the agent
// exited with code.
func (m *Machine) Outcome(code int) (int, string) {
	switch {
	case m.failed != "":
		return 1, "the agent's turn failed: " + m.failed
	case m.exiting: // done: however the agent went (Ctrl-C, killed)
		return 0, ""
	case m.turns == 0 && code == 0:
		return 1, "the agent exited before finishing a turn"
	}
	return code, ""
}
