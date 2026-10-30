package agentlog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// readLines splits a fixture into lines with their offsets.
func readLines(t *testing.T, name string) []Line {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var lines []Line
	var off int64
	for _, l := range bytes.SplitAfter(b, []byte("\n")) {
		if d := bytes.TrimRight(l, "\r\n"); len(d) > 0 {
			lines = append(lines, Line{Offset: off, Data: d})
		}
		off += int64(len(l))
	}
	return lines
}

// want is the part of a Message a table checks.
type want struct {
	role, kind, text string
	tool             string // "id name"
	isError, turnEnd bool
}

func check(t *testing.T, got []Message, wants []want, firstSeq int) {
	t.Helper()
	if len(got) != len(wants) {
		b, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("%d messages, want %d:\n%s", len(got), len(wants), b)
	}
	for i, w := range wants {
		m := got[i]
		tool := ""
		if m.Tool != nil {
			tool = m.Tool.ID + " " + m.Tool.Name
		}
		if m.Role != w.role || m.Kind != w.kind || m.Text != w.text || tool != w.tool || m.IsError != w.isError || m.TurnEnd != w.turnEnd {
			t.Errorf("message %d: %+v (tool %q), want %+v", i, m, tool, w)
		}
		if m.Seq != firstSeq+i {
			t.Errorf("message %d: seq %d", i, m.Seq)
		}
		if m.Time.IsZero() {
			t.Errorf("message %d: no time", i)
		}
	}
}

func TestParseClaude(t *testing.T) {
	lines := readLines(t, "claude.jsonl")
	got := Parse(Claude, lines, 7)
	check(t, got, []want{
		{role: RoleUser, kind: KindText, text: "fix the flaky relay test"},
		{role: RoleAssistant, kind: KindThinking, text: "The test probably races."},
		{role: RoleAssistant, kind: KindText, text: "Let me run it."},
		{role: RoleAssistant, kind: KindToolCall, text: "go test ./internal/relay -run TestUDP", tool: "toolu_01 Bash"},
		{role: RoleTool, kind: KindToolResult, text: "ok  \texample/internal/relay\t2.1s", tool: "toolu_01 Bash"},
		{role: RoleAssistant, kind: KindToolCall, text: `C:\src\example\relay_test.go`, tool: "toolu_02 Read"},
		{role: RoleTool, kind: KindToolResult, text: "file not found", tool: "toolu_02 Read", isError: true},
		{role: RoleSystem, kind: KindSummary, text: "Summary: the relay test was being fixed."},
		{role: RoleAssistant, kind: KindToolCall, text: `{"todos":[{"content":"fix","status":"pending"}]}`, tool: "toolu_03 TodoWrite"},
		{role: RoleAssistant, kind: KindText, text: "Fixed: the test now waits for the listener.", turnEnd: true},
	}, 7)
	if got[0].Offset != lines[1].Offset || !got[0].Time.Equal(time.Date(2026, 10, 30, 9, 10, 1, 0, time.UTC)) {
		t.Fatalf("first message at %d %v, want the second line's offset %d", got[0].Offset, got[0].Time, lines[1].Offset)
	}
	if InferState(Claude, got, true, got[len(got)-1].Time, got[len(got)-1].Time) != StateWaiting {
		t.Fatal("end_turn: not waiting")
	}
}

func TestParseCopilot(t *testing.T) {
	got := Parse(Copilot, readLines(t, "copilot.jsonl"), 0)
	check(t, got, []want{
		{role: RoleUser, kind: KindText, text: "list the failing tests"},
		{role: RoleAssistant, kind: KindThinking, text: "Run the tests first."},
		{role: RoleAssistant, kind: KindText, text: "Running the tests."},
		{role: RoleAssistant, kind: KindToolCall, text: "go test ./...", tool: "call_1 powershell"},
		{role: RoleTool, kind: KindToolResult, text: "FAIL example/internal/relay", tool: "call_1 powershell"},
		{role: RoleAssistant, kind: KindToolCall, text: `C:\src\example\relay_test.go`, tool: "call_2 view"},
		{role: RoleTool, kind: KindToolResult, text: "Path does not exist", tool: "call_2 view", isError: true},
		{role: RoleSystem, kind: KindEvent, text: "Rate limited, retrying", isError: true},
		{role: RoleSystem, kind: KindSummary, text: "The user asked for failing tests."},
		{role: RoleAssistant, kind: KindText, text: "TestUDP fails."},
		{role: RoleSystem, kind: KindEvent, text: "aborted user_initiated"},
	}, 0)
}

// TestParseEdges: unknown agents and garbage give nothing; long tool
// calls and results are cut (calls at the end, results at the start).
func TestParseEdges(t *testing.T) {
	if m := Parse("aider", []Line{{Data: []byte(`{"type":"user"}`)}}, 0); m != nil {
		t.Fatalf("unknown agent: %+v", m)
	}
	if m := Parse(Claude, []Line{{Data: []byte(`[1,2]`)}, {Data: nil}, {Data: []byte(`{"type":"user","message":{"content":42}}`)}}, 0); len(m) != 0 {
		t.Fatalf("garbage: %+v", m)
	}
	long := strings.Repeat("é", 400) // 2 bytes each
	out := strings.Repeat("x", 3000) + "END"
	line := func(v any) Line { b, _ := json.Marshal(v); return Line{Data: b} }
	got := Parse(Claude, []Line{
		line(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": long}}}}}),
		line(map[string]any{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": out}}}}),
		line(map[string]any{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "[Request interrupted by user for tool use]"}}}}),
	}, 0)
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	if c := got[0].Text; len(c) > maxCallText || !strings.HasSuffix(c, "…") || !strings.HasPrefix(c, "éé") || !utf8.ValidString(c) {
		t.Fatalf("call text %d bytes: %q", len(c), c)
	}
	if r := got[1].Text; len(r) > maxResultText || !strings.HasPrefix(r, "[…]\n") || !strings.HasSuffix(r, "xEND") {
		t.Fatalf("result text %d bytes", len(r))
	}
	if got[2].Role != RoleSystem || got[2].Kind != KindEvent {
		t.Fatalf("interruption: %+v", got[2])
	}
}

func TestInferState(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	recent, old := now.Add(-time.Minute), now.Add(-11*time.Minute)
	user := Message{Role: RoleUser, Kind: KindText}
	text := Message{Role: RoleAssistant, Kind: KindText}
	call := Message{Role: RoleAssistant, Kind: KindToolCall}
	result := Message{Role: RoleTool, Kind: KindToolResult}
	thinking := Message{Role: RoleAssistant, Kind: KindThinking}
	turnEnd := Message{Role: RoleAssistant, Kind: KindToolCall, TurnEnd: true}
	event := Message{Role: RoleSystem, Kind: KindEvent}
	summary := Message{Role: RoleSystem, Kind: KindSummary}
	for _, c := range []struct {
		name  string
		msgs  []Message
		live  bool
		last  time.Time
		state string
	}{
		{"not live", []Message{user}, false, recent, StateEnded},
		{"not live, waiting", []Message{user, text}, false, recent, StateEnded},
		{"fresh", nil, true, recent, StateWaiting},
		{"prompt sent", []Message{user}, true, recent, StateWorking},
		{"thinking", []Message{user, thinking}, true, recent, StateWorking},
		{"answered", []Message{user, call, result, text}, true, recent, StateWaiting},
		{"tool call after text", []Message{user, text, call}, true, recent, StateWorking},
		{"tool result", []Message{user, call, result}, true, recent, StateWorking},
		{"turn-end marker", []Message{user, turnEnd}, true, recent, StateWaiting},
		{"interrupted / error", []Message{user, call, event}, true, recent, StateWaiting},
		{"compacted mid-turn", []Message{user, call, summary}, true, recent, StateWorking},
		{"working, silent 11 min", []Message{user, call}, true, old, StateWaiting},
		{"working, silent 10 min", []Message{user, call}, true, now.Add(-10 * time.Minute), StateWaiting},
		{"working, silent 9 min", []Message{user, call}, true, now.Add(-9 * time.Minute), StateWorking},
	} {
		for _, agent := range []string{Claude, Copilot} {
			if got := InferState(agent, c.msgs, c.live, c.last, now); got != c.state {
				t.Errorf("%s (%s): %s, want %s", c.name, agent, got, c.state)
			}
		}
	}
}
