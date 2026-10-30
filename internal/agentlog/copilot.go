package agentlog

import (
	"encoding/json"
	"strings"
)

// copilotEvent is one line of a Copilot CLI session
// (~/.copilot/session-state/<id>/events.jsonl, copilot 1.0.x).
type copilotEvent struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	AgentID   string `json:"agentId"` // a sub-agent's event
	Data      struct {
		Content          string          `json:"content"`
		Source           string          `json:"source"`
		ToolCallID       string          `json:"toolCallId"`
		ToolName         string          `json:"toolName"`
		Arguments        json.RawMessage `json:"arguments"`
		ParentToolCallID string          `json:"parentToolCallId"`
		Success          bool            `json:"success"`
		Result           *struct {
			Content         string `json:"content"`
			DetailedContent string `json:"detailedContent"`
		} `json:"result"`
		Error   json.RawMessage `json:"error"` // {message} on tools, a string on compaction
		Message string          `json:"message"`
		Reason  string          `json:"reason"`
		Summary string          `json:"summaryContent"`
	} `json:"data"`
}

// parseCopilot maps user and assistant messages, reasoning, tool
// executions, errors, aborts and compaction summaries; other events and
// sub-agents' events are skipped.
func parseCopilot(lines []Line, seq int) []Message {
	var out []Message
	names := map[string]string{} // toolCallId -> tool name
	for _, l := range lines {
		var ev copilotEvent
		if json.Unmarshal(l.Data, &ev) != nil || ev.AgentID != "" || ev.Data.ParentToolCallID != "" {
			continue
		}
		m, ok := copilotMessage(ev, names)
		if !ok {
			continue
		}
		msgs := []Message{m}
		seq = stamp(msgs, seq, l.Offset, ev.Timestamp)
		out = append(out, msgs...)
	}
	return out
}

// copilotMessage is the message of one event (false: none).
func copilotMessage(ev copilotEvent, names map[string]string) (Message, bool) {
	d := ev.Data
	switch ev.Type {
	case "user.message":
		// skill-* sources are hidden injections, not typed by the user
		return Message{Role: RoleUser, Kind: KindText, Text: d.Content}, d.Content != "" && !strings.HasPrefix(d.Source, "skill-")
	case "assistant.message":
		return Message{Role: RoleAssistant, Kind: KindText, Text: d.Content}, d.Content != ""
	case "assistant.reasoning":
		return Message{Role: RoleAssistant, Kind: KindThinking, Text: d.Content}, d.Content != ""
	case "tool.execution_start":
		names[d.ToolCallID] = d.ToolName
		return Message{Role: RoleAssistant, Kind: KindToolCall, Text: toolInput(d.Arguments), Tool: &Tool{ID: d.ToolCallID, Name: d.ToolName}}, true
	case "tool.execution_complete":
		return Message{Role: RoleTool, Kind: KindToolResult, Text: toolResult(copilotResult(ev)),
			Tool: &Tool{ID: d.ToolCallID, Name: names[d.ToolCallID]}, IsError: !d.Success}, true
	case "session.error":
		return Message{Role: RoleSystem, Kind: KindEvent, Text: toolResult(d.Message), IsError: true}, true
	case "abort":
		return Message{Role: RoleSystem, Kind: KindEvent, Text: strings.TrimSpace("aborted " + d.Reason)}, true
	case "session.compaction_complete":
		return Message{Role: RoleSystem, Kind: KindSummary, Text: d.Summary}, d.Summary != ""
	}
	return Message{}, false
}

// copilotResult is a tool result's text: the error's message on failure,
// else the result (the detailed one when there is no concise one).
func copilotResult(ev copilotEvent) string {
	var e struct{ Message string }
	if !ev.Data.Success && json.Unmarshal(ev.Data.Error, &e) == nil && e.Message != "" {
		return e.Message
	}
	if r := ev.Data.Result; r != nil {
		if r.Content != "" {
			return r.Content
		}
		return r.DetailedContent
	}
	return ""
}
