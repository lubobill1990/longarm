// Package agentlog turns Claude Code and Copilot CLI transcripts into a
// common list of messages and infers whether the agent is working or
// waiting for input (docs/agent-sessions-ui.md section 4). Pure functions:
// the caller reads the files (longarm daemon, through Files).
package agentlog

import "time"

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
	// TODO(package B): implement (docs/agent-sessions-ui.md section 4).
	return nil
}

// InferState says whether the agent is working, waiting for input or
// ended: live = a process still has the session open; lastActivity = the
// newest of the transcript's modification and the terminal's activity.
func InferState(agent string, msgs []Message, live bool, lastActivity, now time.Time) string {
	// TODO(package B): implement (docs/agent-sessions-ui.md section 4).
	if !live {
		return StateEnded
	}
	return StateWorking
}
