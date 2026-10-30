# Handoff: move an agent conversation between machines

`longarm handoff --to <w|local> [--from <w|local>] [--agent claude|copilot] (--session ID | --latest) [flags]`

`--from` defaults to the worker of the longarm session this runs in (`LONGARM_WORKER`), otherwise local. Running it from inside the very session being moved is refused with exit 2. Run it from another shell instead.

## Steps

1. **Locate**: `--session ID` (from `longarm agent-sessions [<w>|local] --json`) or `--latest`.
   - `--latest` means the newest session of the current checkout locally, or of `--repo-path` / the whole machine on a worker.
   - A copy that was already handed off is refused with exit 2. Hand it off from where it went.
2. **Quiesce**: on a worker, the longarm session running the agent is stopped after `--idle 5s` without I/O (`--idle-timeout 10m`, else exit 75). `--source-session NAME` says which session it is. Locally, an agent process that still has the session open gives exit 75: exit it first.
3. **Code**: a snapshot commit (HEAD + uncommitted + untracked, ignored files excluded) is kept on `refs/longarm/handoff/...`. The target gets a new worktree on branch `handoff/<id8>` with the changes uncommitted again; the staged/unstaged split is lost.
   - worker: `<home>/longarm-handoff/<repo>-<id8>`. It is never garbage-collected. Remove it later with `longarm exec <w> -- git -C <dir> worktree remove --force .`.
   - local: `<checkout>-handoff-<id8>` next to your checkout, or under `--worktree-dir` / `LONGARM_HANDOFF_DIR`. `--local-repo` names the checkout when cwd isn't one.
4. **Transcript**: copied, with absolute paths rewritten to the target worktree. The session id is kept. The source copy stays, marked "handed off".
5. **Resume**: on a worker, a longarm session `--name` (default `handoff-<id8>`) runs `claude --resume <id>` / `copilot --resume=<id>`, then attaches. `--no-attach` or `--json` prints the command instead. Locally, the agent runs in this terminal.

Running processes are not carried over: tool calls, background shells, MCP servers, dev servers. Claude Code may ask to trust the new directory.

## How the code travels: `--via relay|remote|auto` (default auto)

`auto` picks by the size of the objects the target lacks: below `--via-threshold 20M` it uses relay, otherwise remote. Defaults come from `[handoff] via` / `via_threshold` in workers.toml.

- **relay**: over longarm's own channels.
  - Worker to worker goes directly: a one-time offer for the target's certificate, valid 10 minutes.
  - If that fails, the bundle is relayed through this machine. Keep it online until the handoff finishes.
- **remote**: pushes the snapshot to a temporary branch `longarm/handoff/<user>/<id8>-<time>-<rand>` on the repository's https remote. The target fetches it, then the branch is deleted.
  - A worker source needs `--git-auth forward`. The forwarded credential then gets write access for that one push of that one ref.
  - A secret scan runs over the changed and untracked files first. A hit falls back to relay and names the file and rule. `--allow-secrets` pushes anyway.
  - Changed files over 50 MB, no https remote, no credential, or a refused push also fall back to relay, with a note.
  - `--keep-branch` keeps the branch. `longarm handoff --cleanup [--json]` (run in your checkout) lists your stale handoff branches and deletes those older than 1 h.

`--git-auth forward` also lets a worker target fetch the base commits from the remote first, so only the delta is shipped.

## Preflight and safety

- The agent must be installed on the target and be no older than the one that last wrote the session; otherwise exit 2 (`--force` turns the version check into a warning).
- Warnings: the agent looks logged out on the target; MCP servers configured on the source are missing on the target.
- The session's files may total at most `--max-size 64M` (else exit 2).
- Only your own workers: a worker owned by someone else, even if shared, is refused with exit 77. Only your own sessions.
- Admin workers (`-admin`) are used only when named explicitly.

Exit codes: 0 done; 2 usage / already handed off / no agent or an older agent on the target / too large / not a git checkout; 3 no such session; 75 open in a process / not idle; 77 not yours; 255 transport failure. After a 255 the source session may already be stopped: resume it there with the agent's `--resume <id>`.

## Storage (where the agents keep sessions)

| | Claude Code | Copilot CLI |
|---|---|---|
| state dir | `~/.claude` (`$CLAUDE_CONFIG_DIR`) | `~/.copilot` (`$COPILOT_HOME`) |
| transcript | `projects/<path-with-non-alnum-as-dashes>/<id>.jsonl` | `session-state/<id>/events.jsonl` |
| resume | `claude --resume <id>` (in the project dir) | `copilot --resume=<id>` |

Jobs with forwarded Copilot credentials get a private `COPILOT_HOME`, so their sessions are not in the account's `~/.copilot` and can't be listed or handed off from there.
