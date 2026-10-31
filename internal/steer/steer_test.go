package steer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
)

func TestAgentOf(t *testing.T) {
	for argv0, want := range map[string]string{
		"claude": Claude, `C:\Users\me\.local\bin\claude.exe`: Claude, "/usr/local/bin/copilot": Copilot,
		"copilot.cmd": Copilot, "pwsh": "", "claude-code": "",
	} {
		if got := AgentOf([]string{argv0}); got != want {
			t.Errorf("AgentOf(%q) = %q, want %q", argv0, got, want)
		}
	}
	if AgentOf(nil) != "" {
		t.Error("AgentOf(nil)")
	}
}

func TestInteractive(t *testing.T) {
	cases := []struct {
		agent string
		in    []string
		want  []string
	}{
		{Claude, []string{"claude", "-p", "fix it", "--output-format", "stream-json", "--verbose", "--include-partial-messages"},
			[]string{"claude", "fix it", "--verbose"}},
		{Claude, []string{"claude", "--print", "--output-format=json", "--permission-mode", "acceptEdits", "task"},
			[]string{"claude", "--permission-mode", "acceptEdits", "task"}},
		{Claude, []string{"claude", "task"}, []string{"claude", "task"}},
		{Claude, []string{"claude", "-p", "--", "-p is text here"}, []string{"claude", "--", "-p is text here"}},
		{Copilot, []string{"copilot", "-p", "do x", "--allow-all", "-s"}, []string{"copilot", "-i", "do x", "--allow-all"}},
		{Copilot, []string{"copilot", "--prompt=do x"}, []string{"copilot", "-i", "do x"}},
		{Copilot, []string{"copilot", "--prompt", "do x", "--silent"}, []string{"copilot", "-i", "do x"}},
		{Copilot, []string{"copilot", "-i", "do x"}, []string{"copilot", "-i", "do x"}},
	}
	for _, c := range cases {
		got, err := Interactive(c.agent, c.in)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("Interactive(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range [][]string{
		{"claude", "--settings", "x.json", "task"}, {"claude", "--settings=x.json"}, {"pwsh", "-c", "claude"}, {"copilot", "-p"},
	} {
		agent := AgentOf(bad)
		if agent == "" {
			agent = Claude
		}
		if _, err := Interactive(agent, bad); err == nil {
			t.Errorf("Interactive(%q): no error", bad)
		}
	}
	if got := Inject(Claude, []string{"claude", "task"}, "s.json"); !slices.Equal(got, []string{"claude", "--settings", "s.json", "task"}) {
		t.Errorf("Inject = %q", got)
	}
	if got := Inject(Copilot, []string{"copilot", "-i", "x"}, "s.json"); !slices.Equal(got, []string{"copilot", "-i", "x"}) {
		t.Errorf("Inject copilot = %q", got)
	}
}

func TestConfig(t *testing.T) {
	exe, events := filepath.Join("C:", "Program Files", "Longarm", "longarmd.exe"), filepath.Join("D:", "data", "jobs", "J1", "steer", EventsFile)
	b, err := HookConfig(Claude, exe, events)
	if err != nil {
		t.Fatal(err)
	}
	var claude struct {
		Hooks map[string][]struct {
			Hooks []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal(b, &claude); err != nil {
		t.Fatal(err)
	}
	stop := claude.Hooks["Stop"][0].Hooks[0]
	if stop.Type != "command" || !strings.Contains(stop.Command, `" steer-hook "`) || !strings.HasSuffix(stop.Command, `" Stop`) {
		t.Errorf("Stop hook = %+v", stop)
	}
	for _, ev := range claudeEvents {
		if len(claude.Hooks[ev]) != 1 {
			t.Errorf("no %s hook", ev)
		}
	}
	b, err = HookConfig(Copilot, exe, events)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(b, []byte("\xef\xbb\xbf")) {
		t.Error("Copilot hooks with a byte order mark")
	}
	var cop struct {
		Version int
		Hooks   map[string][]struct {
			Type, Bash, PowerShell string
			TimeoutSec             int
		}
	}
	if err := json.Unmarshal(b, &cop); err != nil {
		t.Fatal(err)
	}
	h := cop.Hooks["agentStop"][0]
	if cop.Version != 1 || !strings.HasPrefix(h.PowerShell, `& "`) || !strings.HasSuffix(h.Bash, " agentStop") || h.TimeoutSec == 0 {
		t.Errorf("copilot hooks = %+v", cop)
	}
	if _, err := HookConfig(Claude, `C:\a"b\longarmd.exe`, events); err == nil {
		t.Error("a quote in the path: no error")
	}
}

func TestHookMainAndParse(t *testing.T) {
	f := filepath.Join(t.TempDir(), EventsFile)
	payloads := map[string]string{
		"UserPromptSubmit": `{"session_id":"s1","transcript_path":"C:/t.jsonl","prompt":"hi"}`,
		"Stop": `{"session_id":"s1","transcript_path":"C:/t.jsonl","last_assistant_message":"All done.",
			"background_tasks":[{"id":"b1"}],"stop_hook_active":false}`,
		"Notification": `{"message":"Claude needs your permission","notification_type":"permission_prompt"}`,
		"agentStop":    `{"sessionId":"c1","transcriptPath":"/h/events.jsonl","stopReason":"end_turn"}`,
		"notification": `{"title":"Permission needed","notification_type":"permission_prompt"}`,
		"StopFailure":  `{"error":"rate_limit"}`,
		"SessionEnd":   `{"reason":"prompt_input_exit"}`,
		"Unknown":      `{}`,
	}
	order := []string{"UserPromptSubmit", "Stop", "Notification", "agentStop", "notification", "StopFailure", "SessionEnd", "Unknown"}
	for _, ev := range order {
		if HookMain([]string{f, ev}, strings.NewReader(payloads[ev])) != 0 {
			t.Fatal("HookMain != 0")
		}
	}
	HookMain([]string{f, "Stop"}, strings.NewReader("not json"))
	HookMain([]string{f}, strings.NewReader("{}")) // bad args: nothing, exit 0
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != len(order)+1 {
		t.Fatalf("%d lines: %s", len(lines), b)
	}
	var evs []Event
	for _, l := range lines {
		if ev, ok := ParseEvent([]byte(l)); ok {
			evs = append(evs, ev)
		}
	}
	if len(evs) != len(order) { // Unknown dropped, "not json" Stop kept (no payload)
		t.Fatalf("%d events: %+v", len(evs), evs)
	}
	if e := evs[0]; e.Kind != EvSubmit || e.SessionID != "s1" || e.Transcript != "C:/t.jsonl" {
		t.Errorf("submit = %+v", e)
	}
	if e := evs[1]; e.Kind != EvStop || e.Answer != "All done." || !e.Background {
		t.Errorf("stop = %+v", e)
	}
	if e := evs[2]; e.Kind != EvNotify || !e.Permission || e.Message != "Claude needs your permission" {
		t.Errorf("notification = %+v", e)
	}
	if e := evs[3]; e.Kind != EvStop || e.SessionID != "c1" || e.Transcript != "/h/events.jsonl" || e.Background {
		t.Errorf("agentStop = %+v", e)
	}
	if e := evs[4]; e.Kind != EvNotify || !e.Permission || e.Message != "Permission needed" {
		t.Errorf("copilot notification = %+v", e)
	}
	if e := evs[5]; e.Kind != EvFail || e.Message != "rate_limit" {
		t.Errorf("failure = %+v", e)
	}
	if e := evs[6]; e.Kind != EvEnd {
		t.Errorf("end = %+v", e)
	}
}

// trustVT is Claude Code's folder trust screen as ConPTY drew it (Claude
// Code 2.1.310 on Windows), and the redraw after Down.
func trustVT(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "claude-trust.vt"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const trustDownVT = "\x1b[14;2H \x1b[15;2H❯"

func TestScreen(t *testing.T) {
	s := NewScreen(120, 40)
	s.Write(trustVT(t))
	text := s.Text()
	for _, want := range []string{" Quick safety check: Is this a project", " ❯ No, exit", "   Yes, I trust this folder", " Enter to confirm · Esc to cancel"} {
		if !strings.Contains(text, want) {
			t.Fatalf("screen lacks %q:\n%s", want, text)
		}
	}
	s.Write([]byte(trustDownVT))
	if text := s.Text(); !strings.Contains(text, "   No, exit\n ❯ Yes, I trust this folder") {
		t.Fatalf("after Down:\n%s", text)
	}

	// cut sequences and runes, wrapping, erasing, scrolling, wide runes
	s = NewScreen(10, 3)
	in := "ab\x1b[1;5Hcd\x1b[2;1Hlong line wraps\x1b[3;7Hx\xe2\x9d\xaf"
	for i := 0; i < len(in); i++ { // one byte at a time
		s.Write([]byte{in[i]})
	}
	if got, want := s.Text(), "ab  cd\nlong line\nwraps x❯"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	s.Write([]byte("\x1b[2;5H\x1b[K\x1b[1;1H\x1b[2P"))
	if got, want := s.Text(), "  cd\nlong\nwraps x❯"; got != want {
		t.Fatalf("erase: got %q, want %q", got, want)
	}
	s.Write([]byte("\x1b[3;10H\r\n界z"))
	if got, want := s.Text(), "long\nwraps x❯\n界z"; got != want {
		t.Fatalf("scroll: got %q, want %q", got, want)
	}
	s.Write([]byte("\x1b]0;title\x07\x1b[2J\x1b[Hok"))
	if got := s.Text(); got != "ok" {
		t.Fatalf("clear: %q", got)
	}
	s.Resize(4, 2)
	if got := s.Text(); got != "ok" {
		t.Fatalf("resize: %q", got)
	}
}

// The screens as the probe on a Dev Box saw them (Claude Code 2.1.302,
// Copilot CLI 1.0.105), as StripVT returns them.
const (
	claudeTrustScreen    = `steer-probe Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first. Claude Code'll be able to read, edit, and execute files here. Security guide > No, exit Yes, I trust this folder Enter to confirm · Esc to cancel`
	claudeTrustYesScreen = `No, exit ❯ Yes, I trust this folder Enter to confirm · Esc to cancel`
	copilotTrust         = `│ Do you trust the files in this folder? │ │ │ │ ❯ 1. Yes │ │ 2. Yes, and remember this folder for future sessions │ │ 3. No (Esc) │ │ ↑/↓ to navigate · enter to select · esc to cancel │`
	copilotTrustOn2      = `│ Do you trust the files in this folder? │ │ 1. Yes │ │ ❯ 2. Yes, and remember this folder for future sessions │ │ 3. No (Esc) │`
)

func TestStartupAnswer(t *testing.T) {
	cases := []struct{ agent, screen, keys string }{
		{Claude, claudeTrustScreen, KeyDown},
		{Claude, claudeTrustYesScreen, KeyEnter},
		{Claude, claudeTrustScreen + " " + claudeTrustYesScreen, KeyEnter}, // the redraw after Down
		{Claude, "Let's get started. Choose the text style that looks best with your terminal ❯ 1. Dark mode", KeyEnter},
		{Claude, "Security notes: ... Press Enter to continue…", KeyEnter},
		{Claude, "╭───╮ │ > Try \"fix lint errors\" │", ""},
		{Copilot, copilotTrust, KeyEnter},
		{Copilot, copilotTrustOn2, KeyUp},
		{Copilot, "Welcome to GitHub Copilot CLI", ""},
		{Copilot, claudeTrustScreen, ""},
	}
	for _, c := range cases {
		if keys, _ := StartupAnswer(c.agent, c.screen); keys != c.keys {
			t.Errorf("%s %q: keys %q, want %q", c.agent, c.screen, keys, c.keys)
		}
	}
}

// clock drives a Machine in tests.
type clock struct{ now time.Time }

func (c *clock) add(d time.Duration) time.Time { c.now = c.now.Add(d); return c.now }

// run ticks m every 200 ms for d and returns the actions taken.
func run(m *Machine, c *clock, d time.Duration) []Action {
	var out []Action
	for end := c.now.Add(d); c.now.Before(end); {
		if a := m.Tick(c.add(200 * time.Millisecond)); a.Keys != "" || a.Kill {
			out = append(out, a)
		}
	}
	return out
}

func keysOf(as []Action) []string {
	var out []string
	for _, a := range as {
		if a.Kill {
			out = append(out, "KILL")
		} else {
			out = append(out, a.Keys)
		}
	}
	return out
}

func TestMachineHeadless(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	m := New(Defaults(Claude, false), c.now)
	if st := m.State().State; st != jobs.SteerStarting {
		t.Fatalf("state %s", st)
	}
	// the trust screen: Down, then (after the redraw) Enter
	m.Output(trustVT(t), c.add(100*time.Millisecond))
	if got := keysOf(run(m, c, time.Second)); !slices.Equal(got, []string{KeyDown}) {
		t.Fatalf("trust: %q", got)
	}
	m.Output([]byte(trustDownVT), c.now)
	if got := keysOf(run(m, c, time.Second)); !slices.Equal(got, []string{KeyEnter}) {
		t.Fatalf("trust 2: %q", got)
	}
	m.Event(Event{Kind: EvSubmit, SessionID: "s1", Transcript: "t.jsonl"}, c.now)
	if st := m.State(); st.State != jobs.SteerWorking || st.SessionID != "s1" || st.Transcript != "t.jsonl" {
		t.Fatalf("state %+v", st)
	}
	// working: output keeps coming
	for range 20 {
		m.Output([]byte("…"), c.add(time.Second))
		if a := m.Tick(c.now); a.Keys != "" {
			t.Fatalf("action while working: %+v", a)
		}
	}
	m.Event(Event{Kind: EvStop, Answer: "done"}, c.now)
	if st := m.State(); st.State != jobs.SteerIdle || st.Turns != 1 {
		t.Fatalf("state %+v", st)
	}
	// 5 s quiet: Esc, Ctrl-C, /exit, Enter
	acts := run(m, c, 4*time.Second)
	if len(acts) != 0 {
		t.Fatalf("finished before 5 s of quiet: %+v", acts)
	}
	if got := keysOf(run(m, c, 3*time.Second)); !slices.Equal(got, []string{KeyEsc, KeyCtrlC, "/exit", KeyEnter}) {
		t.Fatalf("exit keys %q", got)
	}
	if st := m.State().State; st != jobs.SteerFinishing {
		t.Fatalf("state %s", st)
	}
	if code, why := m.Outcome(0); code != 0 || why != "" {
		t.Errorf("outcome %d %q", code, why)
	}
	// no exit: Ctrl-C twice after 15 s, kill 5 s later
	if got := keysOf(run(m, c, 25*time.Second)); !slices.Equal(got, []string{KeyCtrlC, KeyCtrlC, "KILL"}) {
		t.Fatalf("escalation %q", got)
	}
	if code, _ := m.Outcome(1); code != 0 {
		t.Errorf("outcome after kill: %d, want 0 (the work was done)", code)
	}
}

func TestMachineWaits(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	stopped := func(cfg Config) (*Machine, *clock) {
		c := &clock{now: start}
		m := New(cfg, c.now)
		m.Event(Event{Kind: EvSubmit}, c.add(time.Second))
		m.Event(Event{Kind: EvStop}, c.add(10*time.Second))
		return m, c
	}
	finishesAfter := func(m *Machine, c *clock, limit time.Duration) time.Duration {
		from := c.now
		for c.now.Sub(from) < limit {
			if a := m.Tick(c.add(200 * time.Millisecond)); a.Keys != "" {
				return c.now.Sub(from)
			}
		}
		return -1
	}

	// a permission prompt holds it until answered
	m, c := stopped(Defaults(Copilot, false))
	m.Event(Event{Kind: EvSubmit}, c.add(time.Second))
	m.Event(Event{Kind: EvNotify, Permission: true, Message: "Permission needed"}, c.add(time.Second))
	if st := m.State(); st.State != jobs.SteerNeedsYou || st.Message != "Permission needed" {
		t.Fatalf("state %+v", st)
	}
	if d := finishesAfter(m, c, 10*time.Minute); d >= 0 {
		t.Fatalf("finished with a permission prompt open (after %s)", d)
	}

	// a human steered: 60 s after the stop and the last keystroke
	m, c = stopped(Defaults(Claude, false))
	m.Input(c.add(time.Second), true)
	m.Event(Event{Kind: EvSubmit}, c.add(time.Second))
	m.Event(Event{Kind: EvStop}, c.add(20*time.Second))
	if d := finishesAfter(m, c, 5*time.Minute); d < 60*time.Second || d > 61*time.Second {
		t.Fatalf("steered run finished after %s, want ~60 s", d)
	}

	// the machine's own keystrokes do not count as steering
	m, c = stopped(Defaults(Claude, false))
	m.Output([]byte("answer"), c.now)
	m.Input(c.now, false)
	if d := finishesAfter(m, c, time.Minute); d < 5*time.Second || d > 6*time.Second {
		t.Fatalf("finished after %s, want ~5 s", d)
	}

	// background tasks: wait for the next stop, at most 30 minutes
	m, c = stopped(Defaults(Claude, false))
	m.Event(Event{Kind: EvStop, Background: true}, c.now)
	if d := finishesAfter(m, c, time.Hour); d < 30*time.Minute || d > 30*time.Minute+time.Second {
		t.Fatalf("background: finished after %s", d)
	}
	m, c = stopped(Defaults(Claude, false))
	m.Event(Event{Kind: EvStop, Background: true}, c.now)
	m.Output([]byte("the task's result"), c.add(time.Minute))
	m.Event(Event{Kind: EvStop}, c.now)
	if d := finishesAfter(m, c, time.Minute); d < 5*time.Second || d > 6*time.Second {
		t.Fatalf("after the background stop: %s", d)
	}

	// a TUI that keeps printing: done 2 minutes after the stop anyway
	m, c = stopped(Defaults(Claude, false))
	from := c.now
	for {
		m.Output([]byte("tick"), c.add(time.Second))
		if a := m.Tick(c.now); a.Keys != "" {
			break
		}
		if c.now.Sub(from) > 5*time.Minute {
			t.Fatal("never finished with output every second")
		}
	}
	if d := c.now.Sub(from); d < 2*time.Minute || d > 2*time.Minute+time.Second {
		t.Fatalf("finished after %s with constant output, want 2 min", d)
	}

	// --until-exit never types /exit
	m, c = stopped(Defaults(Claude, true))
	if d := finishesAfter(m, c, time.Hour); d >= 0 {
		t.Fatalf("until-exit finished after %s", d)
	}
}

func TestMachineOutcome(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := New(Defaults(Claude, false), now)
	if code, why := m.Outcome(0); code != 1 || why == "" {
		t.Errorf("no turn, exit 0: %d %q", code, why)
	}
	if code, _ := m.Outcome(3); code != 3 {
		t.Errorf("no turn, exit 3: %d", code)
	}
	m.Event(Event{Kind: EvSubmit}, now)
	m.Event(Event{Kind: EvFail, Message: "rate_limit"}, now)
	if st := m.State(); st.State != jobs.SteerIdle || st.Message != "rate_limit" {
		t.Errorf("failed turn: %+v", st)
	}
	if code, why := m.Outcome(0); code != 1 || !strings.Contains(why, "rate_limit") {
		t.Errorf("failed: %d %q", code, why)
	}
	m.Event(Event{Kind: EvSubmit}, now)
	m.Event(Event{Kind: EvStop}, now) // retried and succeeded
	if code, why := m.Outcome(0); code != 0 || why != "" {
		t.Errorf("retried: %d %q", code, why)
	}
}

func TestMachineStuck(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	m := New(Defaults(Claude, false), c.now)
	m.Output([]byte("Select login method: 1. Claude account"), c.add(time.Second))
	if got := run(m, c, 30*time.Second); len(got) != 0 {
		t.Fatalf("answered an unknown screen: %+v", got)
	}
	if st := m.State().State; st != jobs.SteerStarting {
		t.Fatalf("state %s", st)
	}
	run(m, c, 31*time.Second)
	if st := m.State(); st.State != jobs.SteerNeedsYou || st.Message == "" {
		t.Fatalf("state %+v", st)
	}
	m.Output([]byte("x"), c.now)
	if st := m.State().State; st != jobs.SteerStarting {
		t.Fatalf("after output: %s", st)
	}
}

// A TUI that ignored an answer (not reading its keyboard yet): the same
// keystroke again once nothing was redrawn for ScreenRetry; not after the
// screen changed.
func TestMachineScreenRetry(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	m := New(Defaults(Claude, false), c.now)
	m.Output(trustVT(t), c.add(100*time.Millisecond))
	if got := keysOf(run(m, c, 5*time.Second)); !slices.Equal(got, []string{KeyDown, KeyDown, KeyDown}) {
		t.Fatalf("retries: %q", got)
	}
	m.Output([]byte(trustDownVT), c.now)
	if got := keysOf(run(m, c, time.Second)); !slices.Equal(got, []string{KeyEnter}) {
		t.Fatalf("after the redraw: %q", got)
	}
	m.Output([]byte("\x1b[2J\x1b[HWelcome to Claude Code!"), c.now) // another screen
	if got := keysOf(run(m, c, 10*time.Second)); len(got) != 0 {
		t.Fatalf("answered a screen that is gone: %q", got)
	}
}
