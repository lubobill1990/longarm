# Steerable runs: `longarm run --steerable -- claude | copilot`

Status: implemented (v1); verified with Claude Code 2.1.310 and Copilot
CLI 1.0.105 on Windows. Follow-ups: todo/steerable-runs.md.

## 1. Goal

`longarm run <worker> -- claude -p "..."` (or `copilot -p "..."`) runs an
agent headless: no terminal, nobody can look at it or step in. A
**steerable run** runs the agent *interactively* on a pseudo terminal on the
worker, so the Sessions window (and `longarm attach --job`) can show its
terminal and a human can steer it, while it stays a **job**:

- `longarm run --steerable` prints a job id; `longarm jobs`, `wait`, `logs`,
  `kill`, the hub's placement and queue work as for any job;
- the run ends by itself when the agent is done (section 4) and exits 0, or
  with a non-zero code when it failed;
- `longarm logs <id>` shows the agent's answers - what `-p` would have
  printed - not terminal escape codes.

```
longarm run cpc-1 --steerable --repo . -- claude "fix the flaky test"
longarm run --steerable --wait -- copilot -p "update the changelog" --allow-all
longarm attach cpc-1 --job 01KQ...          # watch / type into it
longarm session send cpc-1 --job 01KQ... --text "also add a test" --key Enter
```

Non-goals (v1): per-user isolation workers (the hook cannot write into the
job directory there - the submit is refused); agents other than Claude Code
and Copilot CLI; agents inside a wrapper script (`run -- pwsh run-agent.ps1`).

## 2. The command line

The client turns a print-mode command into an interactive one (the job's
argv shows the result):

| | given | runs |
|---|---|---|
| Claude Code | `claude -p "task" [--output-format X] [--input-format X] [--include-partial-messages] [--replay-user-messages] ...` | `claude "task" ...` (the positional prompt is submitted at start) |
| Copilot CLI | `copilot -p "task" [-s] ...` / `--prompt "task"` | `copilot -i "task" ...` |

`claude "task"` / `copilot -i "task"` are taken as they are. Without a
prompt the run waits for a human to type one (it never finishes on its own
before a first turn). Permission flags (`--permission-mode`,
`--allowedTools`, `--dangerously-skip-permissions`; `--allow-all`,
`--allow-tool`) are the user's: without them the agent stops at permission
prompts until somebody answers (state "needs you", section 5).

Refused: another agent; Claude's own `--settings` (the run uses it for its
hooks); Copilot without a private `COPILOT_HOME` (forwarded credentials:
`--agent-auth copilot`, the default for a copilot command), because its
hooks go there.

## 3. On the worker

`JobSpec.steer {agent, until_exit}` (feature `steerable-run`). longarmd
sets `pty` (the shim runs it like a session: ConPTY, attach channel,
scrollback) with a 120x40 terminal. The shim, before the agent starts:

- creates `<job dir>/steer/` with `events.jsonl` and the hook
  configuration:
  - Claude: `steer/claude-settings.json` passed as `--settings <file>`
    (inserted after `argv[0]`): hooks `UserPromptSubmit`, `Stop`,
    `StopFailure`, `Notification`, `SessionEnd`;
  - Copilot: `$COPILOT_HOME/hooks/longarm-steer.json` (UTF-8 **without
    BOM** - with one Copilot ignores the file): `userPromptSubmitted`,
    `agentStop`, `notification`, `permissionRequest`, `sessionEnd`; and
    `$COPILOT_HOME/config.json` `trusted_folders: [<workdir>]`;
- every hook runs `"<longarmd>" steer-hook "<events.jsonl>" <event>`
  (forward slashes: the command runs in bash or cmd for Claude, PowerShell
  for Copilot), which appends `{"event": ..., "time": ..., "payload": <the
  hook's stdin JSON>}` as one line and exits 0 (never blocks the agent).

The shim reads `events.jsonl` every 200 ms and drives a state machine
(`internal/steer`, pure, tested with a fake clock).

**Start-up screens**, answered until the first prompt is submitted. The
screen is a small virtual terminal (`steer.Screen`: cursor moves, erases,
scrolling) fed with the output: ConPTY repaints only what changed with
absolute cursor moves - after Down, Claude's trust dialog redraws just the
`❯` on the other row - so the output stream's text is not the screen.

| screen | answer |
|---|---|
| Claude folder trust: `> No, exit` ... `Yes, I trust this folder` | Down while `No, exit` is selected, Enter once `Yes, I trust this folder` is |
| Claude first run: `Choose the text style` | Enter (the default) |
| Claude first run: `Press Enter to continue` | Enter |
| Copilot folder trust: `Do you trust the files in this folder?` with `❯ 1. Yes` | Enter |

A screen is answered once the terminal has been quiet 400 ms: at once
when it changed since the last answer, again after 2 s when it did not (a
TUI that was not reading its keyboard yet ignored the key). After 20
answers the automation gives up; a start-up that is quiet for 60 s without
a prompt is "needs you" (a screen Longarm does not know, or no prompt).

**Terminal and log.** The raw terminal goes to `term.log` (redacted, capped
like `out.log`); `out.log` - the job's log - gets each finished turn's
answer: Claude's `last_assistant_message` from `Stop`, Copilot's last
assistant text from the transcript (`agentStop.transcriptPath`, parsed with
`internal/agentlog`). Turns after the first are preceded by a blank line.

**State** goes to `steer.json` in the job dir (`Job.steer` in the API):
`state` (`starting`, `working`, `needs_you`, `idle`, `finishing`, `done`),
`turns`, `message` (e.g. the permission prompt), `session_id`,
`transcript`, `updated_at`.

## 4. When is it done

Done when all of these hold:

1. at least one turn finished: a `Stop` / `agentStop` came after the last
   `UserPromptSubmit` / `userPromptSubmitted`;
2. no permission prompt is open (a `Notification` / `notification` with
   `notification_type: "permission_prompt"`, or `permissionRequest`, after
   that stop's prompt; cleared by the next prompt or stop);
3. Claude: the stop's `background_tasks` is empty (else wait for the next
   stop; at most 30 minutes after the stop, then done anyway);
4. the terminal has printed nothing for 5 s;
5. nobody typed for 60 s (input from an attached client or `session send`;
   the shim's own keystrokes do not count), and when a human steered the run
   at all, its last stop is 60 s old too (time to read the answer and reply).

Then the shim types, 500 ms apart: Esc (closes a dialog the agent opened
after its turn - Claude's "Teach auto mode ..." did), Ctrl-C (clears a
draft), `/exit`, Enter (state `finishing`); the agent exits (`SessionEnd`)
and the job ends `done` with code 0. No exit within 15 s: Ctrl-C twice,
then 5 s later the process tree is killed (code 0 all the same: the work
was finished).

Fails (non-zero exit, `failed`): Claude's `StopFailure` (API error, the turn
could not finish) - code 1, reason from the payload; the agent exits by
itself before any turn finished (its own code, 1 if it was 0); `longarm
kill` as usual.

`--until-exit`: never type `/exit`: the run ends when the agent exits
(somebody typed `/exit`, or `longarm kill`).

## 5. Seeing and steering it

- `Job.steer` (state, turns) in `longarm jobs --json` and the hub's list;
  `longarm jobs` shows `running (needs you)` etc.
- `longarm attach <worker> --job <id>` attaches to the terminal (no
  create); `longarm session send <worker> --job <id> ...` types into it
  (`AttachOpen.job`, `SendSessionRequest.job`: the name is a job id; the
  caller must be able to control the job).
- Sessions window: a steerable job's conversation is its job item (matched
  by `steer.transcript`); its tab has the terminal and the steer box; the
  list marks `needs_you` (!) next to the job mark.

## 6. Work packages

1. proto + `internal/jobs` (Spec.Steer, steer.json) + `internal/steer`
   (argv rewrite, hook config, screen answers, state machine) + longarmd
   `steer-hook`.
2. shim: steer mode (hooks, events, term.log/out.log, finish).
3. server: Submit validation, `Job.steer`, Attach/Send for jobs.
4. CLI: `run --steerable [--until-exit]`, `attach --job`, `session send
   --job`, `jobs` state.
5. daemon + web: steerable job items get terminal + steer; needs-you mark.
