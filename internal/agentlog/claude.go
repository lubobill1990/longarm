package agentlog

import (
	"encoding/json"
	"strings"
)

// claudeLine is one line of a Claude Code transcript
// (~/.claude/projects/<project>/<id>.jsonl).
type claudeLine struct {
	Type             string `json:"type"`
	Timestamp        string `json:"timestamp"`
	IsMeta           bool   `json:"isMeta"`
	IsSidechain      bool   `json:"isSidechain"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	IsAPIError       bool   `json:"isApiErrorMessage"`
	Message          struct {
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
	} `json:"message"`
}

// claudeBlock is one block of message.content.
type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// interrupted starts the text Claude Code records when the user stops a
// turn (Esc): the agent then waits.
const interrupted = "[Request interrupted by user"

// parseClaude: user / assistant lines, one Message per content block;
// summaries, system, meta and sidechain (subagent) lines are skipped.
func parseClaude(lines []Line, seq int) []Message {
	var out []Message
	names := map[string]string{} // tool_use id -> tool name
	for _, l := range lines {
		var cl claudeLine
		if json.Unmarshal(l.Data, &cl) != nil || cl.IsMeta || cl.IsSidechain {
			continue
		}
		if cl.Type != "user" && cl.Type != "assistant" {
			continue
		}
		msgs := claudeMessages(cl, names)
		seq = stamp(msgs, seq, l.Offset, cl.Timestamp)
		if n := len(msgs); n > 0 && cl.Type == "assistant" && cl.Message.StopReason == "end_turn" {
			msgs[n-1].TurnEnd = true
		}
		out = append(out, msgs...)
	}
	return out
}

// claudeMessages turns one user / assistant line into messages (without
// Seq, Offset and Time).
func claudeMessages(cl claudeLine, names map[string]string) []Message {
	role := RoleUser
	if cl.Type == "assistant" {
		role = RoleAssistant
	}
	var text string
	if json.Unmarshal(cl.Message.Content, &text) == nil {
		return claudeText(cl, role, text)
	}
	var blocks []claudeBlock
	if json.Unmarshal(cl.Message.Content, &blocks) != nil {
		return nil
	}
	var out []Message
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, claudeText(cl, role, b.Text)...)
		case "thinking":
			if b.Thinking != "" {
				out = append(out, Message{Role: role, Kind: KindThinking, Text: b.Thinking})
			}
		case "tool_use":
			names[b.ID] = b.Name
			out = append(out, Message{Role: role, Kind: KindToolCall, Text: toolInput(b.Input), Tool: &Tool{ID: b.ID, Name: b.Name}})
		case "tool_result":
			out = append(out, Message{Role: RoleTool, Kind: KindToolResult, Text: toolResult(blockText(b.Content)),
				Tool: &Tool{ID: b.ToolUseID, Name: names[b.ToolUseID]}, IsError: b.IsError})
		}
	}
	return out
}

// claudeText is a text block or string content: a compaction summary, an
// interruption, or plain text.
func claudeText(cl claudeLine, role, text string) []Message {
	switch {
	case text == "":
		return nil
	case cl.IsCompactSummary:
		return []Message{{Role: RoleSystem, Kind: KindSummary, Text: text}}
	case role == RoleUser && strings.HasPrefix(text, interrupted):
		return []Message{{Role: RoleSystem, Kind: KindEvent, Text: "interrupted by the user"}}
	}
	return []Message{{Role: role, Kind: KindText, Text: text, IsError: cl.IsAPIError}}
}

// blockText is a tool result's content: a string or text blocks (other
// blocks, e.g. images, are left out).
func blockText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []claudeBlock
	json.Unmarshal(raw, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
