package steer

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// HookArg is the first argument of `longarmd steer-hook <events file>
// <event>`, which every hook of a steerable run runs.
const HookArg = "steer-hook"

// EventsFile is the file in the job's steer directory the hooks append to.
const EventsFile = "events.jsonl"

// Hook events of each agent (the names its hook configuration uses).
var (
	claudeEvents  = []string{"UserPromptSubmit", "Stop", "StopFailure", "Notification", "SessionEnd"}
	copilotEvents = []string{"userPromptSubmitted", "agentStop", "notification", "sessionEnd"}
)

// HookLine is one line of events.jsonl.
type HookLine struct {
	Event   string          `json:"event"`
	Time    time.Time       `json:"time"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// maxPayload bounds what a hook reads from its stdin.
const maxPayload = 4 << 20

// HookMain is `longarmd steer-hook <events file> <event>`: it appends the
// hook's stdin (the agent's JSON) to the events file as one line. It
// always exits 0: a hook must never stand in the agent's way.
func HookMain(args []string, stdin io.Reader) int {
	if len(args) != 2 {
		return 0
	}
	b, _ := io.ReadAll(io.LimitReader(stdin, maxPayload))
	line := HookLine{Event: args[1], Time: time.Now().UTC()}
	var compact bytes.Buffer
	if json.Compact(&compact, bytes.TrimSpace(b)) == nil && compact.Len() > 0 {
		line.Payload = compact.Bytes()
	}
	out, err := json.Marshal(line)
	if err != nil {
		return 0
	}
	f, err := os.OpenFile(args[0], os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return 0
	}
	defer f.Close()
	f.Write(append(out, '\n')) // one write: lines of concurrent hooks do not mix
	return 0
}

// hookPath is a path as hook commands get it: forward slashes on Windows
// (Claude Code runs hooks in bash or cmd, Copilot in PowerShell - all take
// them).
func hookPath(p string) string {
	if runtime.GOOS == "windows" {
		return filepath.ToSlash(p)
	}
	return p
}

// command is the hook command for event: exe steer-hook events event, its
// paths double-quoted (no shell expands inside: paths with " ` $ are
// refused by HookConfig).
func command(exe, events, event string) string {
	return `"` + hookPath(exe) + `" ` + HookArg + ` "` + hookPath(events) + `" ` + event
}

// HookConfig is the hook configuration of a run: Claude's settings file
// (--settings) or Copilot's hooks file (<COPILOT_HOME>/hooks/*.json).
// Copilot ignores a hooks file with a byte order mark: none is written.
func HookConfig(agent, exe, events string) ([]byte, error) {
	for _, p := range []string{exe, events} {
		if strings.ContainsAny(p, "\"`$") {
			return nil, &PathError{Path: p}
		}
	}
	switch agent {
	case Claude:
		type hook struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		}
		type group struct {
			Hooks []hook `json:"hooks"`
		}
		hooks := map[string][]group{}
		for _, ev := range claudeEvents {
			hooks[ev] = []group{{Hooks: []hook{{Type: "command", Command: command(exe, events, ev), Timeout: 30}}}}
		}
		return json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
	case Copilot:
		type hook struct {
			Type       string `json:"type"`
			Bash       string `json:"bash"`
			PowerShell string `json:"powershell"`
			TimeoutSec int    `json:"timeoutSec"`
		}
		hooks := map[string][]hook{}
		for _, ev := range copilotEvents {
			c := command(exe, events, ev)
			hooks[ev] = []hook{{Type: "command", Bash: c, PowerShell: "& " + c, TimeoutSec: 30}}
		}
		return json.MarshalIndent(map[string]any{"version": 1, "hooks": hooks}, "", "  ")
	}
	return nil, &PathError{Path: agent}
}

// PathError: a path a hook command cannot carry safely.
type PathError struct{ Path string }

func (e *PathError) Error() string { return "steer: cannot use " + e.Path + " in a hook command" }

// CopilotHooksFile is where Copilot's hook configuration goes in its home.
func CopilotHooksFile(copilotHome string) string {
	return filepath.Join(copilotHome, "hooks", "longarm-steer.json")
}

// CopilotTrust adds dir to trusted_folders of Copilot's config.json in
// home (a private home of the run: Copilot does not ask whether to trust
// the folder).
func CopilotTrust(home, dir string) error {
	p := filepath.Join(home, "config.json")
	cfg := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		if json.Unmarshal(b, &cfg) != nil {
			cfg = map[string]any{}
		}
	}
	var folders []any
	if f, ok := cfg["trusted_folders"].([]any); ok {
		folders = f
	}
	for _, f := range folders {
		if s, ok := f.(string); ok && strings.EqualFold(filepath.Clean(s), filepath.Clean(dir)) {
			return nil
		}
	}
	cfg["trusted_folders"] = append(folders, dir)
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// Event kinds (both agents).
const (
	EvSubmit = "submit" // a prompt was submitted
	EvStop   = "stop"   // a turn finished
	EvFail   = "fail"   // a turn failed (Claude: StopFailure)
	EvNotify = "notify" // a notification (permission prompt, idle)
	EvEnd    = "end"    // the session ends
)

// Event is a hook event as the state machine sees it.
type Event struct {
	Kind string
	// Permission: a permission prompt waits for a human (Notify).
	Permission bool
	Message    string // the notification's text, the failure's reason
	// Background: Claude's stop with background tasks still running.
	Background bool
	// Answer: the turn's final message (Claude's Stop).
	Answer     string
	SessionID  string
	Transcript string
}

// ParseEvent reads one events.jsonl line.
func ParseEvent(line []byte) (Event, bool) {
	var hl HookLine
	if json.Unmarshal(line, &hl) != nil {
		return Event{}, false
	}
	var p struct {
		SessionID         string          `json:"session_id"`
		SessionID2        string          `json:"sessionId"`
		Transcript        string          `json:"transcript_path"`
		Transcript2       string          `json:"transcriptPath"`
		Message           string          `json:"message"`
		Title             string          `json:"title"`
		NotificationType  string          `json:"notification_type"`
		LastAssistant     string          `json:"last_assistant_message"`
		BackgroundTasks   json.RawMessage `json:"background_tasks"`
		Error             string          `json:"error"`
		ErrorMessage      string          `json:"error_message"`
		StopFailureReason string          `json:"reason"`
	}
	_ = json.Unmarshal(hl.Payload, &p)
	ev := Event{SessionID: first2(p.SessionID, p.SessionID2), Transcript: first2(p.Transcript, p.Transcript2)}
	switch hl.Event {
	case "UserPromptSubmit", "userPromptSubmitted":
		ev.Kind = EvSubmit
	case "Stop", "agentStop":
		ev.Kind = EvStop
		ev.Answer = p.LastAssistant
		ev.Background = nonEmptyList(p.BackgroundTasks)
	case "StopFailure":
		ev.Kind = EvFail
		ev.Message = first2(first2(p.Error, p.ErrorMessage), first2(p.Message, p.StopFailureReason))
		if ev.Message == "" {
			ev.Message = "the turn failed"
		}
	case "Notification", "notification":
		ev.Kind = EvNotify
		ev.Permission = p.NotificationType == "permission_prompt"
		ev.Message = first2(p.Message, p.Title)
	case "SessionEnd", "sessionEnd":
		ev.Kind = EvEnd
	default:
		return Event{}, false
	}
	return ev, true
}

func nonEmptyList(raw json.RawMessage) bool {
	var l []any
	if json.Unmarshal(raw, &l) == nil {
		return len(l) > 0
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		return len(m) > 0
	}
	return false
}

func first2(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
