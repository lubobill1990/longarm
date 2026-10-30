# Agent sessions in the tray: list, watch, attach, steer

Status: spec for implementation (v1). Owner of the integration: the
coordinator; work packages A, B, C below are implemented in parallel.

## 1. Goal

From the tray, a user sees the Claude Code / Copilot CLI sessions of their
workers, reads what each one is doing, attaches to its terminal and steers
it - without opening a terminal or knowing CLI commands.

- Clicking **Claude Code N** / **Copilot CLI N** on a worker card (Pool tab)
  opens the **Sessions window** filtered to that worker and agent.
- The window lists sessions per worker, shows one session per tab with a
  **terminal view** (xterm.js, live) and a **conversation view** (parsed
  transcript), and has a **steer box** (text + Enter / Esc / y / Ctrl-C).
- **New session** starts `claude` / `copilot` (or `--resume <id>`) in a
  Longarm session on a worker.

### What works where

| | agent in a **Longarm session** (`attach -- claude`, `session start`, the tray's "New session", handoff) | agent started **outside** Longarm (a terminal on the Dev Box desktop, VS Code) |
|---|---|---|
| listed | yes | yes |
| conversation view (transcript) | yes, live | yes, live, read-only |
| terminal view (xterm.js) | yes | no (Longarm has no access to its terminal) |
| steer | yes (`Sessions.Send`, no takeover) | no - "Continue here": resume it in a Longarm session (fork when it is still open) |

### Non-goals (v1)

- Steering a process Longarm did not start.
- tmux-like split panes (a tab can be popped out into its own window later).
- Mobile / hub web panel (the API is local to the user's daemon).
- Any worker-side (longarmd) change: everything uses existing RPCs
  (`Sessions.*`, `Files.*`), so it works with workers already deployed.

## 2. Architecture

```
Sessions window (web, /sessions)  ──HTTP/SSE/WebSocket──▶  longarm-tray (/local/longarm/* reverse proxy, /trayapi/*)
                                                              │ local pipe / socket (user-only)
                                                              ▼
                                                        longarm daemon (cmd/longarm)
                                                          • /v1/agent-sessions ...   (package A)
                                                          • internal/agentlog parse  (package B)
                                                              │ gRPC/mTLS (existing conns: daemon.conn(worker))
                                                              ▼
                                                        longarmd: Sessions.List/Get/Start/Send/Stop/Attach, Files.Read/Tail/List
```

- The tray's `/local/longarm/` proxy (`httputil.ReverseProxy`) passes
  WebSocket upgrades through; its same-origin check
  (`internal/tray/server.go`) applies to the upgrade request too.
- The daemon reuses the agent adapters of `longarm handoff` /
  `agent-sessions` (`cmd/longarm/handoff_agents.go`: `allAdapters`,
  `agentAdapter.list`, `openEndpoint`) to find transcripts on a worker,
  and `daemon.conn(worker)` for the gRPC connection.

## 3. Daemon HTTP API (package A)

All paths are on the daemon's local API (the tray serves them at
`/local/longarm/<path>`). JSON field names are snake_case. Errors:
`{"error": "message"}` with 400 (bad input), 404 (no such worker /
session), 409 (not idle, see send), 502 (worker unreachable), 500.

### 3.1 List

`GET /v1/agent-sessions[?worker=W][&agent=claude|copilot][&all=1][&since=T][&until=T]`

Workers: `W`, else every approved worker of the user that is online in the
pool view (`d.mon`), queried in parallel, 15 s per worker. `all=1` also
returns ended Longarm sessions and handed-off transcripts.

Transcripts are listed by time (RFC 3339 `since` / `until`, modification
time, both inclusive): the **first page** (no `until`) holds those of the
last 7 days (`since` default) plus the running sessions - Longarm
sessions, live agent processes and what they resume - however old their
transcripts; an **older page** (`until`) holds the transcripts of
[`since` (default `until` - 7 days), `until`] other than those. The answer's
`older` is the modification time of the newest transcript before the
window (absent: none): the next page is `until=<older>`, so a page is never
empty. Only the transcripts in the window are read (their first and last
bytes); what was read is cached by machine, path, size and modification
time, so listing again (the window refreshes every 5 s) reads only what
changed.

```json
{
  "items": [
    {
      "key": "cpc-a/claude/9f1c2d3e-...",
      "worker": "cpc-a",
      "agent": "claude",
      "id": "9f1c2d3e-...",
      "title": "fix the flaky relay test",
      "project_dir": "C:\\Users\\me\\src\\longarm",
      "modified": "2026-10-30T09:12:44Z",
      "size": 482113,
      "where": "longarm",
      "session": "agent1",
      "state": "waiting",
      "last_activity": "2026-10-30T09:12:44Z",
      "pid": 12345
    },
    {
      "key": "cpc-a/session/shell1",
      "worker": "cpc-a", "agent": "", "id": "", "title": "pwsh",
      "where": "longarm", "session": "shell1", "state": "running", "last_activity": "..."
    }
  ],
  "unreachable": ["cpc-c"],
  "older": "2026-10-20T08:00:00.123456789Z"
}
```

- `key` is unique and stable: `<worker>/<agent>/<id>` for agent sessions,
  `<worker>/session/<name>` for Longarm sessions without a known agent.
- `where`: `longarm` (the agent runs in Longarm session `session`),
  `desktop` (a live process outside Longarm), `none` (no live process: the
  transcript only).
- `state`: `working` | `waiting` (the agent finished its turn and waits for
  input) | `running` (a Longarm session whose program is not a known agent)
  | `ended`. Inferred, best effort (section 4): an agent in a Longarm
  session whose terminal printed in the last 3 s is `working` (its TUI
  animates while it works, and is quiet while it waits); otherwise the
  transcript's end decides.
- Matching an agent transcript to a Longarm session: the session's argv[0]
  base name is `claude` / `copilot` (`.exe`, `.cmd` stripped) and its `dir`
  equals the transcript's project directory (case-insensitive on Windows),
  and the transcript was modified after the session started; Claude Code's
  `sessions/<pid>.json` (pid, sessionId, cwd) of live processes refines
  it. A session matching no transcript yet (a fresh `claude`) is listed
  with `agent` set and `id` "".
- Sorted: running/waiting first, then by `last_activity` descending.

### 3.2 Conversation

`GET /v1/agent-sessions/{worker}/{agent}/{id}/messages[?offset=N][&tail_bytes=B][&before=N]`

Reads the transcript with `Files.Read` from `offset` (default: the last
`tail_bytes`, default 2 MiB, before `before` - default the end - aligned to
the next line start) and returns complete lines parsed with
`agentlog.Parse`. A window inside one long line (a big tool result) grows
until a line fits (up to the read limit).

```json
{
  "messages": [
    {"seq": 0, "offset": 1024, "time": "2026-10-30T09:10:01Z", "role": "user", "kind": "text", "text": "fix the flaky relay test"},
    {"seq": 1, "offset": 1290, "time": "...", "role": "assistant", "kind": "tool_call", "text": "go test ./internal/relay -run TestUDP", "tool": {"id": "toolu_01", "name": "Bash"}},
    {"seq": 2, "offset": 1580, "time": "...", "role": "tool", "kind": "tool_result", "text": "ok ... 2.1s", "tool": {"id": "toolu_01", "name": "Bash"}, "is_error": false}
  ],
  "start": 1024,
  "next_offset": 482113,
  "size": 482113,
  "truncated": true,
  "state": "waiting"
}
```

`start`: where the first message's line begins; `truncated`: older content
exists before it - scrolling to the top loads it with `?before=<start>`.
`seq` numbers the messages of one answer; a message's identity is
(`offset`, `seq`).

`GET /v1/agent-sessions/{worker}/{agent}/{id}/messages/watch?offset=N` -
SSE (same framing as `/v1/pool/watch`): `event: messages`, data = the
object above (only new messages) whenever the transcript grows (Files.Tail
follow); `event: state` data `{"state": "..."}` when the inferred state
changes. Ends when the client goes.

### 3.3 Longarm sessions

- `POST /v1/workers/{worker}/sessions` - start:
  ```json
  {"name": "agent1", "agent": "claude", "cwd": "C:\\src\\longarm", "resume": "", "fork": false, "argv": [], "cols": 120, "rows": 36}
  ```
  `agent` claude|copilot builds argv: `claude` / `claude --resume <id>
  [--fork-session]`, `copilot` / `copilot --resume=<id>`; `argv` instead
  runs anything. `name` empty: generated (`<agent>-<4 hex>`). Agent
  credentials are forwarded like `longarm session start --agent-auth auto`
  (existing code path, memory-only on the worker). Response: the session
  (`pb.Session` as JSON, protojson with snake_case names) - 201; an
  existing session with the same spec: 200.
- `POST /v1/workers/{worker}/sessions/{name}/send` - steer:
  `{"text": "fix it", "keys": ["enter"], "if_idle_ms": 1500, "raw": false}`.
  Same semantics as `longarm session send` (newline → CR unless raw; keys
  by `keyBytes`; text and each key written in that order; Enter as its
  own write). `{"input_seq": N}`; 409 `{"error": "not idle"}` when
  `if_idle_ms` refused.
- `POST /v1/workers/{worker}/sessions/{name}/stop` - `{"grace_ms": 10000}`
  → the session.
- `GET /v1/workers/{worker}/sessions/{name}` → the session.

### 3.4 Terminal (WebSocket)

`GET /v1/workers/{worker}/sessions/{name}/attach?cols=C&rows=R` with
`Upgrade: websocket` (library: `github.com/coder/websocket`, already in
go.mod; Origin check: accept any origin on the daemon - the daemon's
local API is user-only; the tray checks the browser's origin).

- One `Sessions.Attach` stream per WebSocket: open with
  `{name, no_create: true, size}`; replay included.
- **server → client**: binary frames = terminal output bytes (as
  received); text frames = JSON control:
  `{"type":"started","session":{...}}` first, `{"type":"exit","code":N}`
  when the program ended (then close 1000),
  `{"type":"error","message":"..."}` (then close 1011).
- **client → server**: binary frames = keyboard input bytes (as typed in
  xterm.js); text frames `{"type":"resize","cols":C,"rows":R}`,
  `{"type":"detach"}`.
- Closing the WebSocket detaches (the session keeps running). Keepalive:
  websocket ping every 20 s. Back-pressure: if the client can't keep up
  (write blocks > 10 s) the bridge closes with 1008 and the UI
  re-attaches (replay restores the screen).

## 4. internal/agentlog (package B)

Pure parsing, no I/O; the API in `internal/agentlog/agentlog.go` is fixed
(stubs committed with this spec):

```go
type Message struct { Seq int; Offset int64; Time time.Time; Role, Kind, Text string; Tool *Tool; IsError bool }
type Tool struct { ID, Name string }
func Parse(agent string, lines []Line, firstSeq int) []Message
func InferState(agent string, msgs []Message, live bool, lastActivity time.Time, now time.Time) string
```

Rules:
- **Claude Code** (`~/.claude/projects/<p>/<id>.jsonl`): lines with
  `type` `user` / `assistant` carry `message.content` - a string or blocks
  `text`, `tool_use` (name, input), `tool_result` (content, is_error),
  `thinking`. One Message per block. `user` lines that only carry
  `tool_result` blocks become role `tool`. `summary`, `system`, meta lines
  (`isMeta`), and sidechain lines (`isSidechain`, subagents) are skipped;
  `isCompactSummary` becomes kind `summary`. Text of a tool call: the
  most telling input field (`command`, `file_path`, `pattern`, `url`,
  `description`, else compact JSON), one line, max 300 chars. Tool
  results: text content, max 2000 chars (keep the end, mark the cut).
- **Copilot CLI** (`~/.copilot/session-state/<id>/events.jsonl`): one
  event per line with `type` and `data`; map user messages, assistant
  messages, tool execution start/complete, errors; skip the rest. Verify
  the event names against a real file (copilot 1.0.x) and keep unknown
  types out of the output.
- Unparsable lines are skipped (never an error).
- **InferState**: not `live` → `ended`; the last message is an assistant
  `text` with no tool call after it, or the agent's explicit turn-end
  marker (Claude: `stop_reason` "end_turn") → `waiting`; otherwise
  `working`; `working` with no activity for 10 minutes → `waiting`.
- Fixtures under `internal/agentlog/testdata/` are **synthetic** (written
  by hand in the real format) - never copies of real transcripts.

## 5. Tray (package B)

- `POST /trayapi/sessions/open` `{"worker": "...", "agent": "...", "key": "..."}`
  (all optional) opens the **Sessions window** at
  `/sessions#worker=..&agent=..&key=..` or, when it is open, focuses it
  and sets `location.hash` (Wails `ExecJS`); the page reacts to
  `hashchange`. `--browser` / no-webview builds: open the URL in the
  default browser. Validate values like `validWorker` (names, agent
  claude|copilot, key: `[A-Za-z0-9._/-]{1,200}`).
- The window: Wails v3 `app.Window.NewWithOptions`, name `sessions`,
  title "Longarm Sessions", 1200×760, resizable, normal frame, not
  always-on-top, not hidden on focus loss, remembers size/position like
  the popup does (optional in v1); closing it hides it (re-open is
  instant), the tray quitting closes it.
- `/sessions` is served by the tray like `/tray` (the web UI's index).

## 6. Web (package C)

- `web/src/sessions/`: `SessionsApp.tsx` (the page), `SessionList.tsx`
  (left: grouped by worker, state dots ● working ◐ waiting ○ ended ◇
  desktop, agent icon, title, relative time; filter box), `SessionTabs.tsx`
  (tabs, Ctrl+Tab / Ctrl+1..9, close = detach, badge on background tabs
  with new output or state `waiting`), `TerminalView.tsx` (xterm.js +
  fit addon over the WebSocket of 3.4: binary in/out, resize on fit,
  reconnect with backoff, "session ended (code N)" overlay),
  `ConversationView.tsx` (3.2 messages + watch SSE; user / assistant
  bubbles, tool calls collapsed with result, auto-scroll while at the
  bottom), `SteerBox.tsx` (textarea; Enter = send text + enter key,
  Shift+Enter newline; buttons Esc, "y", Ctrl-C with confirm; uses
  `if_idle_ms: 1500` and shows "someone is typing - retry" on 409),
  `NewSessionDialog.tsx` (worker, agent, directory, optional resume id).
- A tab of a `where: longarm` session shows Terminal | Conversation
  (default Terminal); `desktop` / `none` show Conversation only, with
  **Continue here** (starts `resume` with `fork: true` when the source is
  live) instead of the steer box.
- At most 6 tabs keep their WebSocket; older background tabs disconnect
  and re-attach when shown (replay).
- `web/src/api/sessions.ts`: typed client of section 3 (types mirror the
  JSON above); WebSocket URL: `${location.protocol === 'https:' ? 'wss' :
  'ws'}://${location.host}/local/longarm/v1/workers/..../attach`.
- `web/src/main.tsx`: route `/sessions` → `SessionsApp`.
- Pool tab: the Claude Code / Copilot CLI counters on a worker card
  (`WorkerCard.tsx`, `data-testid="agent-claude"` / `agent-copilot`) become
  buttons → `trayApi.openSessions({worker, agent})`
  (`POST /trayapi/sessions/open`).
- Dependencies: `@xterm/xterm`, `@xterm/addon-fit` (pinned exact
  versions, lockfile updated). jsdom has no canvas: tests mock the
  terminal module.
- Look: reuse `tokens.css` variables (light/dark), monospace for the
  terminal, the existing `Section`/button styles.

## 7. Security

- Everything runs as the user: the daemon's local API is user-only; the
  tray serves the window from its own origin and refuses cross-origin
  requests and upgrades. No new listener.
- Workers decide as today: `Sessions.*` and `Files.*` apply the owner /
  isolation rules; transcripts are read with the caller's rights.
- Input sent (steer) is never logged by the daemon or the tray (sizes
  only), like `session send`.
- Transcript contents are shown, never stored by the tray or the daemon.

## 8. Work packages

Each package is a branch from the commit carrying this spec; touch only
the files listed (others: ask the coordinator in your final report).

| | owner files | tests |
|---|---|---|
| **A** daemon API + WebSocket bridge | `cmd/longarm/daemon_agents.go`, `cmd/longarm/daemon_agents_ws.go`, `cmd/longarm/daemon_agents_test.go`, one registration line in `cmd/longarm/daemon.go` (`gateway`) | Go unit tests with fakes (session list matching, start argv, send keys, messages offset/line alignment, WS bridge against an in-process fake Sessions server) |
| **B** agentlog + tray window | `internal/agentlog/*`, `internal/tray/server.go` (route), `internal/tray/sessions.go` (+ test), `cmd/longarm-tray/shell_wails.go`, `cmd/longarm-tray/shell_none.go` | parsers on synthetic fixtures for both agents, InferState table test, `/trayapi/sessions/open` validation + opener hook |
| **C** web | `web/src/sessions/*`, `web/src/api/sessions.ts`, `web/src/main.tsx`, `web/src/components/WorkerCard.tsx`, `web/src/tray/PoolTab.tsx`, `web/src/api/local.ts` (`openSessions`), `web/package.json`, `web/pnpm-lock.yaml` | vitest: list grouping/state, tabs, steer (409), conversation rendering, card click; typecheck |

The coordinator integrates (rebuilds `internal/webui/dist`, docs/usage.md,
CHANGELOG), runs the whole suite and an end-to-end check against real
workers.
