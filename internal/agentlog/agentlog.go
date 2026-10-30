// Package agentlog turns Claude Code and Copilot CLI transcripts into a
// common list of messages and infers whether the agent is working or
// waiting for input (docs/agent-sessions-ui.md section 4). Pure functions:
// the caller reads the files (longarm daemon, through Files).
package agentlog

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// Agents.
const (
	Claude  = "claude"
	Copilot = "copilot"
)

// Roles of a Message.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	RoleSystem    = "system"
)

// Kinds of a Message.
const (
	KindText       = "text"
	KindToolCall   = "tool_call"
	KindToolResult = "tool_result"
	KindThinking   = "thinking"
	KindSummary    = "summary"
	KindEvent      = "event"
)

// States of a session.
const (
	StateWorking = "working"
	StateWaiting = "waiting"
	StateEnded   = "ended"
)

// Line is one line of a transcript and the byte offset it starts at.
type Line struct {
	Offset int64
	Data   []byte // without the newline
}

// Tool identifies the tool of a call or result.
type Tool struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// Message is one entry of the conversation view.
type Message struct {
	Seq     int       `json:"seq"`
	Offset  int64     `json:"offset"`
	Time    time.Time `json:"time,omitzero"`
	Role    string    `json:"role"`
	Kind    string    `json:"kind"`
	Text    string    `json:"text"`
	Tool    *Tool     `json:"tool,omitempty"`
	IsError bool      `json:"is_error,omitempty"`
	// TurnEnd: the agent marked its turn as finished here (Claude's
	// stop_reason "end_turn"); used by InferState.
	TurnEnd bool `json:"-"`
}

// Parse turns transcript lines of agent (Claude or Copilot) into messages
// numbered from firstSeq. Unknown or unparsable lines are skipped.
func Parse(agent string, lines []Line, firstSeq int) []Message {
	switch agent {
	case Claude:
		return parseClaude(lines, firstSeq)
	case Copilot:
		return parseCopilot(lines, firstSeq)
	}
	return nil
}

// idleAfter: a working agent without activity for this long is taken to
// wait (a turn end the transcript did not show).
const idleAfter = 10 * time.Minute

// InferState says whether the agent is working, waiting for input or
// ended: live = a process still has the session open; lastActivity = the
// newest of the transcript's modification and the terminal's activity.
func InferState(agent string, msgs []Message, live bool, lastActivity, now time.Time) string {
	if !live {
		return StateEnded
	}
	if turnOver(msgs) || now.Sub(lastActivity) >= idleAfter {
		return StateWaiting
	}
	return StateWorking
}

// turnOver: no messages yet (a fresh agent waits for its first prompt),
// the turn-end marker, the assistant's text as the last word, or an
// interruption / error event.
func turnOver(msgs []Message) bool {
	if len(msgs) == 0 {
		return true
	}
	m := msgs[len(msgs)-1]
	return m.TurnEnd || m.Role == RoleAssistant && m.Kind == KindText || m.Role == RoleSystem && m.Kind == KindEvent
}

// stamp numbers msgs from seq and sets the offset and time of their line;
// it returns the next seq.
func stamp(msgs []Message, seq int, offset int64, timestamp string) int {
	t, _ := time.Parse(time.RFC3339Nano, timestamp)
	for i := range msgs {
		msgs[i].Seq, msgs[i].Offset, msgs[i].Time = seq, offset, t
		seq++
	}
	return seq
}

// Limits of a Message's text.
const (
	maxCallText   = 300
	maxResultText = 2000
)

// callFields: the input field that tells most about a tool call, by
// preference (path: Copilot's file tools).
var callFields = []string{"command", "file_path", "path", "pattern", "url", "description"}

// toolInput is a tool call's text: its most telling input field, else the
// compact JSON of the input, on one line.
func toolInput(raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) == nil {
		for _, f := range callFields {
			if s, ok := in[f].(string); ok && s != "" {
				return oneLine(s, maxCallText)
			}
		}
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return ""
	}
	return oneLine(b.String(), maxCallText)
}

// oneLine collapses s's whitespace (newlines too) and cuts it to max bytes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	cut := max - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// toolResult keeps the end of a tool's output (where results and errors
// are), at most maxResultText bytes, the cut marked.
func toolResult(s string) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= maxResultText {
		return s
	}
	const mark = "[…]\n"
	from := len(s) - (maxResultText - len(mark))
	for from < len(s) && !utf8.RuneStart(s[from]) {
		from++
	}
	return mark + s[from:]
}
