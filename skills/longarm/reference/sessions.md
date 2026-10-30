# Interactive sessions

A session is a PTY on the worker, named per worker (default name `default`). It runs in its own shim process, so it outlives detach, network loss and longarmd restarts. Humans use `attach`; programs use `session start|send|stop`. Both can work on the same session at once.

## Start

`longarm session start [<w>|auto] -s NAME [--cwd DIR | --repo PATH|URL [--ref R] [--git-auth forward]] [-e K=V] [--secret-env N] [--agent-auth ...] [--require k=v] [--no-queue] [--json] [-- cmd...]`

- `--json` prints `{"worker","session","cwd","created","state"}`. `cwd` is the worktree when `--repo` is used.
- Idempotent: the same command, cwd, env, secret names and workspace gives `created:false` with exit 0. A different spec under a running name gives exit 2. An ended session with that name is replaced.
- With no worker, a running session of that name on one of your workers is reused. Otherwise the session is placed like `run`.
- If the session ends before start returns, start exits with its code: 126 when the workspace failed, 127 when the program did not start.
- Sessions get `LONGARM_WORKER` and `LONGARM_SESSION` in their environment.
- `--repo` worktrees live as long as the session data. They are removed when the ended session is garbage-collected or replaced.

## Steer

```sh
longarm session send [<w>] -s NAME --text "..."          # newline = Enter
longarm session send [<w>] -s NAME --key enter           # enter tab shift-tab esc backspace space up down left right home end pageup pagedown delete ctrl-<letter>
longarm session send [<w>] -s NAME --stdin-file F|-      # <= 256 KiB; --raw = bytes unchanged
longarm session send ... --if-idle-ms 2000               # only if no input in the last 2 s, else exit 75 (nothing sent)
```

- Each send is one atomic write, serialized with attached humans' keystrokes.
- TUIs (Claude Code, Copilot) may treat a large write as a paste. Send the text, then a separate `--key enter` send.
- Exit codes: 2 = the session ended; 3 = no such session; 75 = the `--if-idle-ms` check refused the send.
- To answer an agent's permission prompt, read the transcript first (`longarm logs -s NAME --from -4000`), then send the key (`--key enter`, `--text 1`, ...).

## Observe

- `longarm logs [<w>] -s NAME [-f] [--from N]`: the raw terminal transcript, with secrets redacted.
- `longarm sessions [<w>] [--all] [-s NAME] --json`: `{"sessions":[...],"source":"workers","unreachable":[...]}`. Activity fields:
  - `lastOutput` and `lastInput`.
  - `lastInputSource`: `attach` = a human typed, `send` = a program, `stop`.
  - `inputSeq`: compare it with a remembered value to see whether someone else typed.
  - `attached`: the number of clients.

  The hub does not index sessions. Each worker answers for its own.
- `longarm ls -o wide` also lists Claude Code / Copilot CLI sessions per worker.

## Take over / end

- `longarm attach <w> -s NAME` attaches to the session and replays recent output. Detach with `Ctrl-] d`. `--no-create` fails if the session doesn't exist. Attach exits with the session's exit code when it ends.
- To move the agent itself to your machine (or another worker), use `longarm handoff --from <w> --to local|<w2>` (handoff.md). It stops the source session once it is idle.
- `longarm session stop [<w>] -s NAME --grace 10s` sends Ctrl-C twice, then kills the process tree. `attach --kill` kills at once.
- Admins: `--owner <user>` on attach, logs and sessions works with another user's session.
